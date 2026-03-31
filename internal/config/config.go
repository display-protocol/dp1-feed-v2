// Package config loads YAML defaults merged with environment overrides (secrets and ops).
// Design: non-secret defaults live in config/config.yaml; production sets DP1_FEED_* env vars.
package config

import (
	"crypto/ed25519"
	"fmt"
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
	Server     ServerConfig     `yaml:"server"`
	Database   DatabaseConfig   `yaml:"database"`
	Auth       AuthConfig       `yaml:"auth"`
	Sentry     SentryConfig     `yaml:"sentry"`
	Logging    LoggingConfig    `yaml:"logging"`
	Extensions ExtensionsConfig `yaml:"extensions"`
	Playlist   PlaylistConfig   `yaml:"playlist"`
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
	APIKey string `yaml:"api_key"`
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
}

func (c *Config) validate() error {
	// Minimum bar for boot: DB, mutating API key, and hex signing material (kid is filled in Load after this).
	if c.Database.URL == "" {
		return fmt.Errorf("database url is required (yaml database.url or DP1_FEED_DATABASE_URL)")
	}
	if c.Auth.APIKey == "" {
		return fmt.Errorf("api key is required (yaml auth.api_key or DP1_FEED_API_KEY)")
	}
	if strings.TrimSpace(c.Playlist.SigningKeyHex) == "" {
		return fmt.Errorf("signing key is required (yaml playlist.signing_key_hex or DP1_FEED_SIGNING_KEY_HEX)")
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
