// Package config loads YAML defaults merged with environment overrides (secrets and ops).
// Design: non-secret defaults live in config/config.yaml; production sets DP1_FEED_* env vars.
package config

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	dp1sign "github.com/display-protocol/dp1-go/sign"

	"github.com/display-protocol/dp1-feed-v2/internal/dp1svc"
	"github.com/display-protocol/dp1-feed-v2/internal/notification"
)

const envPrefix = "DP1_FEED_"

// Config is the root application configuration.
type Config struct {
	Server        ServerConfig       `yaml:"server"`
	Database      DatabaseConfig     `yaml:"database"`
	Auth          AuthConfig         `yaml:"auth"`
	Sentry        SentryConfig       `yaml:"sentry"`
	Logging       LoggingConfig      `yaml:"logging"`
	Extensions    ExtensionsConfig   `yaml:"extensions"`
	Playlist      PlaylistConfig     `yaml:"playlist"`
	CORS          CORSConfig         `yaml:"cors"`
	Notifications NotificationConfig `yaml:"notifications"`
}

// NotificationConfig controls outbound channel lifecycle delivery.
type NotificationConfig struct {
	Timeout       time.Duration              `yaml:"timeout"`
	PrivateKeyHex string                     `yaml:"private_key_hex"`
	PublicKey     string                     `yaml:"-"`
	Clients       []NotificationClientConfig `yaml:"clients"`
}

// NotificationClientConfig describes one signed webhook consumer.
type NotificationClientConfig struct {
	Name string `json:"name" yaml:"name"`
	URL  string `json:"url" yaml:"url"`
}

// CORSConfig controls browser cross-origin access (gin-contrib/cors).
// Empty AllowOrigins, or a single "*", allows any origin (default). Otherwise only listed origins match.
// Do not combine "*" with other origins in the same list; use either wildcard alone or an explicit list.
type CORSConfig struct {
	AllowOrigins []string `yaml:"allow_origins"`
}

// ServerConfig controls the HTTP listener.
type ServerConfig struct {
	Host                 string        `yaml:"host"`
	Port                 int           `yaml:"port"`
	ReadTimeout          time.Duration `yaml:"read_timeout"`
	WriteTimeout         time.Duration `yaml:"write_timeout"`
	ResponseWriteReserve time.Duration `yaml:"response_write_reserve"`
	IdleTimeout          time.Duration `yaml:"idle_timeout"`
	// MaxRequestBytes caps an inbound request body. It bounds memory a client can force the server to
	// buffer before authentication (the signature middleware reads the whole body). Zero falls back to
	// DefaultMaxRequestBytes.
	MaxRequestBytes int64 `yaml:"max_request_bytes"`
}

// DefaultMaxRequestBytes is the inbound request-body cap used when config leaves it unset. It comfortably
// holds a signed playlist/group/channel document while bounding pre-auth memory use.
const DefaultMaxRequestBytes = 5 << 20 // 5 MiB

// DatabaseConfig holds PostgreSQL connection settings.
type DatabaseConfig struct {
	URL             string        `yaml:"url"`
	MaxConns        int32         `yaml:"max_conns"`
	MinConns        int32         `yaml:"min_conns"`
	MaxConnLifetime time.Duration `yaml:"max_conn_lifetime"`
}

// AuthConfig tunes signature-based authorization for mutating routes. There is no API key: every
// mutating request is authorized by the cryptographic signatures it carries — POST by the document's own
// signatures, PUT and DELETE additionally by a signed mutation-intent.
type AuthConfig struct {
	// IntentMaxClockSkew bounds how far a signed mutation-intent's "created" may sit from server time in
	// either direction, for both replace and delete. Because that "created" is inside the intent's signed
	// payload it cannot be forged, which is what makes it a usable replay bound (the per-signature "ts" is
	// NOT covered by the signature). Keep it small — it is the window in which a captured intent could be
	// replayed — while still tolerating honest client/server clock drift. Zero falls back to the package
	// default (see defaultConfig).
	IntentMaxClockSkew time.Duration `yaml:"intent_max_clock_skew"`
}

// DefaultIntentMaxClockSkew is the mutation-intent freshness window used when config leaves it unset.
const DefaultIntentMaxClockSkew = 5 * time.Minute

// SentryConfig is optional; empty DSN disables Sentry.
type SentryConfig struct {
	DSN string `yaml:"dsn"`
}

// LoggingConfig toggles development-style logs.
type LoggingConfig struct {
	Debug bool `yaml:"debug"`
}

// ExtensionsConfig gates optional DP-1 surfaces (registry validation, channel APIs, etc.).
type ExtensionsConfig struct {
	Enabled bool `yaml:"enabled"`
}

// PlaylistConfig controls outbound fetch when ingesting remote playlists (group/channel updates).
type PlaylistConfig struct {
	FetchTimeout      time.Duration `yaml:"fetch_timeout"`
	FetchMaxBodyBytes int64         `yaml:"fetch_max_body_bytes"`
	SigningKeyHex     string        `yaml:"signing_key_hex"` // Ed25519 seed (64 bytes = 128 hex) or full private key hex; required
	// SigningKid is set at load time from the signing key (did:key:…).
	SigningKid    string `yaml:"-"`
	PublicBaseURL string `yaml:"public_base_url"` // Used to build playlist URIs referenced from groups
	// AllowPrivateFetchDestinations lets playlist ingestion reach loopback and other private addresses.
	//
	// Off by default and intended for tests and local development only. Group and channel creation is open
	// to any client, so whoever names a playlist URL decides where the feed sends a request; with this on,
	// that becomes a probe of everything the feed can reach (loopback, cloud metadata, internal services).
	AllowPrivateFetchDestinations bool `yaml:"allow_private_fetch_destinations"`
	// MaxPlaylistReferences caps how many playlist URIs one group or channel may reference.
	//
	// This is a fan-out bound, not a modeling preference. Group and channel creation is open to any
	// client, and every reference the document lists becomes an outbound fetch for URIs this feed does not
	// already store. Without a cap the only limit is the request-body size, so a single valid,
	// self-signed request near MaxRequestBytes can name six figures' worth of URIs and turn one inbound
	// request into that many outbound ones. The SSRF guard constrains *where* those requests may go; it
	// does nothing about how many there are. Zero falls back to DefaultMaxPlaylistReferences.
	MaxPlaylistReferences int `yaml:"max_playlist_references"`
	// MaxResolvedBytes caps the total size of resolved playlist documents held in memory for one
	// group/channel mutation.
	//
	// The reference count and the per-fetch size cap do not bound memory between them, they multiply:
	// resolution keeps every resolved body until the whole set is ready to persist, so the default 1000
	// references at 4 MiB each is ~4 GiB from a single unauthenticated request (and persistence copies the
	// bodies again). The eight-way concurrency limit paces downloads without bounding what is retained.
	// This is the budget that actually bounds it. Zero falls back to DefaultMaxResolvedBytes.
	MaxResolvedBytes int64 `yaml:"max_resolved_bytes"`
}

// DefaultMaxResolvedBytes bounds retained resolved-playlist bytes per mutation when config leaves it
// unset. Comfortably fits a large curated collection while keeping one request's peak far below what a
// modest deployment can absorb, including the copy made at persistence time.
const DefaultMaxResolvedBytes = 64 << 20 // 64 MiB

// DefaultMaxPlaylistReferences bounds playlist references per group/channel when config leaves it unset.
// Chosen to sit far above any realistic curated collection while keeping worst-case fan-out per request
// to something a deployment can absorb.
const DefaultMaxPlaylistReferences = 1000

// Load reads YAML from path (if non-empty), merges DP1_FEED_* environment overrides, validates
// required fields, and sets Playlist.SigningKid from the Ed25519 public key (did:key).
func Load(configPath string) (*Config, error) {
	cfg := defaultConfig()
	if configPath != "" {
		// Path comes from operator (flag/env); Clean avoids oddities without changing intent.
		p := filepath.Clean(configPath)
		data, err := os.ReadFile(p) //nolint:gosec // G304: intentional config file path from deployment/CLI
		if err != nil {
			return nil, fmt.Errorf("config read: %w", err)
		}
		if err := yaml.Unmarshal(data, cfg); err != nil {
			return nil, fmt.Errorf("config yaml: %w", err)
		}
	}
	if err := applyEnv(cfg); err != nil {
		return nil, err
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := cfg.deriveSigningKid(); err != nil {
		return nil, err
	}
	if err := cfg.deriveWebhookPublicKey(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// defaultConfig is the baseline before YAML and env; local-dev friendly defaults.
func defaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host:                 "0.0.0.0",
			Port:                 8787,
			ReadTimeout:          30 * time.Second,
			WriteTimeout:         60 * time.Second,
			ResponseWriteReserve: time.Second,
			IdleTimeout:          120 * time.Second,
			MaxRequestBytes:      DefaultMaxRequestBytes,
		},
		Database: DatabaseConfig{
			URL:             "postgres://postgres:postgres@localhost:5432/dp1_feed?sslmode=disable", // #nosec G101 -- local development default; production config comes from YAML/env.
			MaxConns:        16,
			MinConns:        2,
			MaxConnLifetime: time.Hour,
		},
		Logging:    LoggingConfig{Debug: false},
		Auth:       AuthConfig{IntentMaxClockSkew: DefaultIntentMaxClockSkew},
		Extensions: ExtensionsConfig{Enabled: true},
		Playlist: PlaylistConfig{
			FetchTimeout:          30 * time.Second,
			FetchMaxBodyBytes:     4 << 20, // 4 MiB
			MaxPlaylistReferences: DefaultMaxPlaylistReferences,
			MaxResolvedBytes:      DefaultMaxResolvedBytes,
		},
		Notifications: NotificationConfig{Timeout: 15 * time.Second},
	}
}

// applyEnv overlays non-empty DP1_FEED_* variables onto cfg (ops secrets and overrides without editing YAML).
func applyEnv(cfg *Config) error {
	if v := os.Getenv(envPrefix + "DATABASE_URL"); v != "" {
		cfg.Database.URL = v
	}
	if v := os.Getenv(envPrefix + "INTENT_MAX_CLOCK_SKEW"); v != "" {
		d, err := time.ParseDuration(v)
		if err != nil {
			return fmt.Errorf("intent max clock skew env: %w", err)
		}
		cfg.Auth.IntentMaxClockSkew = d
	}
	if v := os.Getenv(envPrefix + "MAX_REQUEST_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("max request bytes env: %w", err)
		}
		cfg.Server.MaxRequestBytes = n
	}
	if v := os.Getenv(envPrefix + "MAX_PLAYLIST_REFERENCES"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil {
			return fmt.Errorf("max playlist references env: %w", err)
		}
		cfg.Playlist.MaxPlaylistReferences = n
	}
	if v := os.Getenv(envPrefix + "MAX_RESOLVED_BYTES"); v != "" {
		n, err := strconv.ParseInt(v, 10, 64)
		if err != nil {
			return fmt.Errorf("max resolved bytes env: %w", err)
		}
		cfg.Playlist.MaxResolvedBytes = n
	}
	if v := os.Getenv(envPrefix + "SENTRY_DSN"); v != "" {
		cfg.Sentry.DSN = v
	}
	if v := os.Getenv(envPrefix + "SERVER_HOST"); v != "" {
		cfg.Server.Host = v
	}
	if v := os.Getenv(envPrefix + "SERVER_PORT"); v != "" {
		if p, err := strconv.Atoi(v); err == nil {
			cfg.Server.Port = p
		}
	}
	if v := os.Getenv(envPrefix + "LOG_DEBUG"); v != "" {
		cfg.Logging.Debug = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv(envPrefix + "EXTENSIONS_ENABLED"); v != "" {
		cfg.Extensions.Enabled = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv(envPrefix + "SIGNING_KEY_HEX"); v != "" {
		cfg.Playlist.SigningKeyHex = v
	}
	if v := os.Getenv(envPrefix + "PUBLIC_BASE_URL"); v != "" {
		cfg.Playlist.PublicBaseURL = strings.TrimRight(v, "/")
	}
	if v := os.Getenv(envPrefix + "ALLOW_PRIVATE_FETCH_DESTINATIONS"); v != "" {
		cfg.Playlist.AllowPrivateFetchDestinations = strings.EqualFold(v, "true") || v == "1"
	}
	if v := os.Getenv(envPrefix + "CORS_ALLOW_ORIGINS"); v != "" {
		var origins []string
		for part := range strings.SplitSeq(v, ",") {
			part = strings.TrimSpace(part)
			if part != "" {
				origins = append(origins, part)
			}
		}
		if len(origins) > 0 {
			cfg.CORS.AllowOrigins = origins
		}
	}
	if v := os.Getenv(envPrefix + "NOTIFICATION_CLIENTS"); v != "" {
		var clients []NotificationClientConfig
		if err := json.Unmarshal([]byte(v), &clients); err != nil {
			return fmt.Errorf("notification clients env: %w", err)
		}
		cfg.Notifications.Clients = clients
	}
	if v := os.Getenv(envPrefix + "WEBHOOK_PRIVATE_KEY_HEX"); v != "" {
		cfg.Notifications.PrivateKeyHex = v
	}
	return nil
}

func (c *Config) validate() error {
	// Minimum bar for boot: DB and hex signing material (kid is filled in Load after this). There is no
	// API key to require: mutating routes are authorized by request signatures, not a shared secret.
	if c.Database.URL == "" {
		return fmt.Errorf("database url is required (yaml database.url or DP1_FEED_DATABASE_URL)")
	}
	if c.Auth.IntentMaxClockSkew < 0 {
		return fmt.Errorf("auth intent max clock skew must not be negative")
	}
	if c.Server.MaxRequestBytes < 0 {
		return fmt.Errorf("server max request bytes must not be negative")
	}
	if strings.TrimSpace(c.Playlist.SigningKeyHex) == "" {
		return fmt.Errorf("signing key is required (yaml playlist.signing_key_hex or DP1_FEED_SIGNING_KEY_HEX)")
	}
	if c.Playlist.FetchTimeout <= 0 {
		return fmt.Errorf("playlist fetch timeout must be positive")
	}
	if c.Server.ResponseWriteReserve <= 0 {
		return fmt.Errorf("response write reserve must be positive")
	}
	if c.Server.WriteTimeout <= c.Server.ResponseWriteReserve {
		return fmt.Errorf("server write timeout must exceed response write reserve")
	}
	// The request deadline handed to reference-resolving mutations is write_timeout - response_write_reserve,
	// and a group or channel write may spend up to fetch_timeout resolving a single remote reference. If the
	// budget is smaller than that, the deadline cancels a fetch the operator explicitly allowed time for and
	// the write fails with a generic error — a configuration that looks valid but cannot serve the writes it
	// accepts. Checked for every deployment, not only those with notification clients: groups fan out the
	// same way channels do.
	if c.Server.WriteTimeout <= c.Playlist.FetchTimeout+c.Server.ResponseWriteReserve {
		return fmt.Errorf("server write timeout must exceed playlist fetch timeout plus response write reserve")
	}
	if c.Playlist.MaxPlaylistReferences < 0 {
		return fmt.Errorf("max playlist references must not be negative")
	}
	// A budget below one document's fetch cap could not resolve even a single reference, so the deployment
	// would reject every group/channel write that fetches. Checked here rather than discovered in
	// production. The budget is deliberately NOT required to cover references x fetch cap: that product is
	// the unbounded figure this budget exists to replace, and requiring it would force an unusably small
	// reference limit.
	if c.Playlist.MaxResolvedBytes > 0 && c.Playlist.MaxResolvedBytes < c.Playlist.FetchMaxBodyBytes {
		return fmt.Errorf("max resolved bytes must be at least the per-playlist fetch cap (%d)", c.Playlist.FetchMaxBodyBytes)
	}
	if c.Playlist.MaxResolvedBytes < 0 {
		return fmt.Errorf("max resolved bytes must not be negative")
	}
	if len(c.Notifications.Clients) > 0 && c.Notifications.Timeout <= 0 {
		return fmt.Errorf("notification timeout must be positive")
	}
	if len(c.Notifications.Clients) > 0 && c.Server.WriteTimeout <= c.Playlist.FetchTimeout+c.Notifications.Timeout+c.Server.ResponseWriteReserve {
		return fmt.Errorf("server write timeout must exceed playlist fetch timeout plus notification timeout plus response write reserve")
	}
	if len(c.Notifications.Clients) > 0 && strings.TrimSpace(c.Notifications.PrivateKeyHex) == "" {
		return fmt.Errorf("webhook private key is required when notification clients are configured (yaml notifications.private_key_hex or DP1_FEED_WEBHOOK_PRIVATE_KEY_HEX)")
	}
	names := make(map[string]struct{}, len(c.Notifications.Clients))
	for i := range c.Notifications.Clients {
		client := &c.Notifications.Clients[i]
		client.Name = strings.TrimSpace(client.Name)
		client.URL = strings.TrimSpace(client.URL)
		if client.Name == "" {
			return fmt.Errorf("notification client %d name is required", i)
		}
		if _, exists := names[client.Name]; exists {
			return fmt.Errorf("notification client name %q is duplicated", client.Name)
		}
		names[client.Name] = struct{}{}
		endpoint, err := notification.ValidateWebhookEndpoint(client.URL)
		if err != nil {
			return fmt.Errorf("notification client %q url: %w", client.Name, err)
		}
		client.URL = endpoint
	}
	if len(c.Notifications.Clients) > 0 {
		c.Playlist.PublicBaseURL = strings.TrimRight(strings.TrimSpace(c.Playlist.PublicBaseURL), "/")
		publicBase, err := url.Parse(c.Playlist.PublicBaseURL)
		if err != nil || publicBase.Host == "" || publicBase.Hostname() == "" ||
			(!strings.EqualFold(publicBase.Scheme, "http") && !strings.EqualFold(publicBase.Scheme, "https")) {
			return fmt.Errorf("playlist public base url must be an absolute HTTP(S) URL when notification clients are configured")
		}
		hostname := strings.TrimSuffix(publicBase.Hostname(), ".")
		// url.Hostname preserves an RFC 6874 IPv6 zone (for example ::1%lo),
		// but net.ParseIP classifies only the address portion.
		ipHostname, _, _ := strings.Cut(hostname, "%")
		parsedIP := net.ParseIP(ipHostname)
		if strings.EqualFold(hostname, "localhost") || (parsedIP != nil && parsedIP.IsLoopback()) {
			return fmt.Errorf("playlist public base url must not use a loopback host when notification clients are configured")
		}
		if parsedIP != nil && parsedIP.IsUnspecified() {
			return fmt.Errorf("playlist public base url must not use an unspecified host when notification clients are configured")
		}
		if publicBase.RawQuery != "" || publicBase.ForceQuery || strings.Contains(c.Playlist.PublicBaseURL, "#") {
			return fmt.Errorf("playlist public base url must not contain a query or fragment when notification clients are configured")
		}
		if publicBase.User != nil {
			return fmt.Errorf("playlist public base url must not contain credentials when notification clients are configured")
		}
	}
	return nil
}

// deriveWebhookPublicKey validates the configured private scalar and publishes only its public half.
func (c *Config) deriveWebhookPublicKey() error {
	if strings.TrimSpace(c.Notifications.PrivateKeyHex) == "" {
		return nil
	}
	privateKey, err := notification.ParseP256PrivateKeyHex(c.Notifications.PrivateKeyHex)
	if err != nil {
		return fmt.Errorf("webhook private key: %w", err)
	}
	publicKey, err := notification.P256PublicKeyString(&privateKey.PublicKey)
	if err != nil {
		return fmt.Errorf("webhook public key: %w", err)
	}
	c.Notifications.PublicKey = publicKey
	return nil
}

// deriveSigningKid parses the playlist signing key and stores the did:key identifier used in DP-1 signatures.
func (c *Config) deriveSigningKid() error {
	priv, err := dp1svc.Ed25519PrivateKeyFromHex(c.Playlist.SigningKeyHex)
	if err != nil {
		return fmt.Errorf("playlist signing key: %w", err)
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		return fmt.Errorf("derive signing kid: unexpected public key type")
	}
	kid, err := dp1sign.Ed25519DIDKey(pub)
	if err != nil {
		return fmt.Errorf("derive signing kid: %w", err)
	}
	c.Playlist.SigningKid = kid
	return nil
}

// Address returns host:port for net.Listen.
func (c *Config) Address() string {
	return fmt.Sprintf("%s:%d", c.Server.Host, c.Server.Port)
}
