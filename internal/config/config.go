// Package config loads YAML defaults merged with environment overrides (secrets and ops).
// Design: non-secret defaults live in config/config.yaml; production sets DP1_FEED_* env vars.
package config

import (
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"gopkg.in/yaml.v3"

	dp1sign "github.com/display-protocol/dp1-go/sign"

	"github.com/display-protocol/dp1-feed-v2/internal/dp1svc"
)

const envPrefix = "DP1_FEED_"

// Config is the root application configuration.
type Config struct {
	Server        ServerConfig        `yaml:"server"`
	Database      DatabaseConfig      `yaml:"database"`
	Auth          AuthConfig          `yaml:"auth"`
	PublisherAuth PublisherAuthConfig `yaml:"publisher_auth"`
	Sentry        SentryConfig        `yaml:"sentry"`
	Logging       LoggingConfig       `yaml:"logging"`
	Extensions    ExtensionsConfig    `yaml:"extensions"`
	Playlist      PlaylistConfig      `yaml:"playlist"`
}

// ServerConfig controls the HTTP listener.
type ServerConfig struct {
	Host         string        `yaml:"host"`
	Port         int           `yaml:"port"`
	ReadTimeout  time.Duration `yaml:"read_timeout"`
	WriteTimeout time.Duration `yaml:"write_timeout"`
	IdleTimeout  time.Duration `yaml:"idle_timeout"`
}

// DatabaseConfig holds PostgreSQL connection settings.
type DatabaseConfig struct {
	URL             string        `yaml:"url"`
	MaxConns        int32         `yaml:"max_conns"`
	MinConns        int32         `yaml:"min_conns"`
	MaxConnLifetime time.Duration `yaml:"max_conn_lifetime"`
}

// AuthConfig protects mutating routes (Bearer API key).
type AuthConfig struct {
	APIKey          string               `yaml:"api_key"`
	PublisherTokens []PublisherTokenAuth `yaml:"publisher_tokens"`
}

// PublisherTokenAuth configures one publisher-scoped bearer token.
// The token authenticates a publisher identity; authorization is still checked against channel ownership.
type PublisherTokenAuth struct {
	Name         string `yaml:"name"`
	Token        string `yaml:"token"`
	PublisherKey string `yaml:"publisher_key"`
}

// PublisherAuthConfig controls the browser-based publisher account flow.
type PublisherAuthConfig struct {
	RPID                 string        `yaml:"rp_id"`
	RPDisplayName        string        `yaml:"rp_display_name"`
	RPOrigins            []string      `yaml:"rp_origins"`
	SessionCookieName    string        `yaml:"session_cookie_name"`
	CeremonyCookieName   string        `yaml:"ceremony_cookie_name"`
	SessionTTL           time.Duration `yaml:"session_ttl"`
	CeremonyTTL          time.Duration `yaml:"ceremony_ttl"`
	DomainResolverURL    string        `yaml:"domain_resolver_url"`
	DomainResolverAPIKey string        `yaml:"domain_resolver_api_key"`
	ENSRPCURL            string        `yaml:"ens_rpc_url"`
}

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
}

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
	applyEnv(cfg)
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if err := cfg.FinalizePublisherAuth(); err != nil {
		return nil, err
	}
	if err := cfg.deriveSigningKid(); err != nil {
		return nil, err
	}
	return cfg, nil
}

// defaultConfig is the baseline before YAML and env; local-dev friendly defaults.
func defaultConfig() *Config {
	return &Config{
		Server: ServerConfig{
			Host:         "0.0.0.0",
			Port:         8787,
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 30 * time.Second,
			IdleTimeout:  120 * time.Second,
		},
		Database: DatabaseConfig{
			URL:             "postgres://postgres:postgres@localhost:5432/dp1_feed?sslmode=disable",
			MaxConns:        16,
			MinConns:        2,
			MaxConnLifetime: time.Hour,
		},
		Logging:    LoggingConfig{Debug: false},
		Extensions: ExtensionsConfig{Enabled: true},
		PublisherAuth: PublisherAuthConfig{
			RPDisplayName:      "Feral File Publisher",
			SessionCookieName:  "dp1_publisher_session",
			CeremonyCookieName: "dp1_publisher_ceremony",
			SessionTTL:         30 * 24 * time.Hour,
			CeremonyTTL:        15 * time.Minute,
		},
		Playlist: PlaylistConfig{
			FetchTimeout:      30 * time.Second,
			FetchMaxBodyBytes: 4 << 20, // 4 MiB
		},
	}
}

// applyEnv overlays non-empty DP1_FEED_* variables onto cfg (ops secrets and overrides without editing YAML).
func applyEnv(cfg *Config) {
	if v := os.Getenv(envPrefix + "DATABASE_URL"); v != "" {
		cfg.Database.URL = v
	}
	if v := os.Getenv(envPrefix + "API_KEY"); v != "" {
		cfg.Auth.APIKey = v
	}
	if v := os.Getenv(envPrefix + "PUBLISHER_TOKENS_JSON"); v != "" {
		var entries []PublisherTokenAuth
		if err := json.Unmarshal([]byte(v), &entries); err == nil {
			cfg.Auth.PublisherTokens = entries
		}
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
	if v := os.Getenv(envPrefix + "PUBLISHER_AUTH_RP_ID"); v != "" {
		cfg.PublisherAuth.RPID = strings.TrimSpace(v)
	}
	if v := os.Getenv(envPrefix + "PUBLISHER_AUTH_RP_DISPLAY_NAME"); v != "" {
		cfg.PublisherAuth.RPDisplayName = strings.TrimSpace(v)
	}
	if v := os.Getenv(envPrefix + "PUBLISHER_AUTH_RP_ORIGINS_JSON"); v != "" {
		var origins []string
		if err := json.Unmarshal([]byte(v), &origins); err == nil {
			cfg.PublisherAuth.RPOrigins = origins
		}
	}
	if v := os.Getenv(envPrefix + "PUBLISHER_AUTH_SESSION_COOKIE_NAME"); v != "" {
		cfg.PublisherAuth.SessionCookieName = strings.TrimSpace(v)
	}
	if v := os.Getenv(envPrefix + "PUBLISHER_AUTH_CEREMONY_COOKIE_NAME"); v != "" {
		cfg.PublisherAuth.CeremonyCookieName = strings.TrimSpace(v)
	}
	if v := os.Getenv(envPrefix + "PUBLISHER_AUTH_ENS_RPC_URL"); v != "" {
		cfg.PublisherAuth.ENSRPCURL = strings.TrimSpace(v)
	}
	if v := os.Getenv(envPrefix + "PUBLISHER_AUTH_DOMAIN_RESOLVER_URL"); v != "" {
		cfg.PublisherAuth.DomainResolverURL = strings.TrimSpace(v)
	}
	if v := os.Getenv(envPrefix + "PUBLISHER_AUTH_DOMAIN_RESOLVER_API_KEY"); v != "" {
		cfg.PublisherAuth.DomainResolverAPIKey = strings.TrimSpace(v)
	}
}

func (c *Config) validate() error {
	// Minimum bar for boot: DB, mutating API key, and hex signing material (kid is filled in Load after this).
	if c.Database.URL == "" {
		return fmt.Errorf("database url is required (yaml database.url or DP1_FEED_DATABASE_URL)")
	}
	if c.Auth.APIKey == "" {
		return fmt.Errorf("api key is required (yaml auth.api_key or DP1_FEED_API_KEY)")
	}
	seenTokens := make(map[string]struct{}, len(c.Auth.PublisherTokens))
	for i, entry := range c.Auth.PublisherTokens {
		if strings.TrimSpace(entry.Token) == "" {
			return fmt.Errorf("publisher token %d is missing auth.token", i)
		}
		if strings.TrimSpace(entry.PublisherKey) == "" {
			return fmt.Errorf("publisher token %d is missing auth.publisher_key", i)
		}
		if _, ok := seenTokens[entry.Token]; ok {
			return fmt.Errorf("duplicate publisher auth token configured")
		}
		seenTokens[entry.Token] = struct{}{}
	}
	if strings.TrimSpace(c.Playlist.SigningKeyHex) == "" {
		return fmt.Errorf("signing key is required (yaml playlist.signing_key_hex or DP1_FEED_SIGNING_KEY_HEX)")
	}
	if c.PublisherAuth.SessionTTL <= 0 {
		return fmt.Errorf("publisher auth session ttl must be positive")
	}
	if c.PublisherAuth.CeremonyTTL <= 0 {
		return fmt.Errorf("publisher auth ceremony ttl must be positive")
	}
	if strings.TrimSpace(c.PublisherAuth.SessionCookieName) == "" {
		return fmt.Errorf("publisher auth session cookie name is required")
	}
	if strings.TrimSpace(c.PublisherAuth.CeremonyCookieName) == "" {
		return fmt.Errorf("publisher auth ceremony cookie name is required")
	}
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

// FinalizePublisherAuth fills derived publisher auth defaults from the public base URL.
func (c *Config) FinalizePublisherAuth() error {
	base := strings.TrimSpace(c.Playlist.PublicBaseURL)
	if base == "" {
		host := c.Server.Host
		if host == "" || host == "0.0.0.0" {
			host = "localhost"
		}
		base = fmt.Sprintf("http://%s:%d", host, c.Server.Port)
	}
	u, err := url.Parse(base)
	if err != nil {
		return fmt.Errorf("public_base_url parse: %w", err)
	}
	if strings.TrimSpace(c.PublisherAuth.RPID) == "" {
		c.PublisherAuth.RPID = u.Hostname()
	}
	if len(c.PublisherAuth.RPOrigins) == 0 {
		c.PublisherAuth.RPOrigins = []string{fmt.Sprintf("%s://%s", u.Scheme, u.Host)}
	}
	return nil
}
