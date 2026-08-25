package config

import (
	"crypto/ed25519"
	"os"
	"path/filepath"
	"strings"
	"testing"

	dp1sign "github.com/display-protocol/dp1-go/sign"

	"github.com/display-protocol/dp1-feed-v2/internal/dp1svc"
)

// 32-byte Ed25519 seed as 64 hex chars (matches dev config style).
const testSeedHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"
const testWebhookPrivateKeyHex = "0000000000000000000000000000000000000000000000000000000000000001"

func TestConfig_Address(t *testing.T) {
	c := &Config{
		Server: ServerConfig{Host: "127.0.0.1", Port: 9999},
	}
	if got, want := c.Address(), "127.0.0.1:9999"; got != want {
		t.Fatalf("Address() = %q, want %q", got, want)
	}
}

func TestLoad_minimalYAML_derivesSigningKid(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	content := strings.TrimSpace(`
database:
  url: postgres://user:pass@localhost:5432/db?sslmode=disable
auth:
  api_key: integration-test-key
playlist:
  signing_key_hex: "` + testSeedHex + `"
`)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}

	if cfg.Database.URL == "" || cfg.Auth.APIKey == "" {
		t.Fatalf("expected database url and api key from yaml")
	}
	if cfg.Playlist.SigningKeyHex != testSeedHex {
		t.Fatalf("signing key hex mismatch")
	}

	priv, err := dp1svc.Ed25519PrivateKeyFromHex(testSeedHex)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("public key type")
	}
	wantKid, err := dp1sign.Ed25519DIDKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Playlist.SigningKid != wantKid {
		t.Fatalf("SigningKid = %q, want %q", cfg.Playlist.SigningKid, wantKid)
	}
	if !strings.HasPrefix(cfg.Playlist.SigningKid, "did:key:") {
		t.Fatalf("SigningKid should be did:key: %q", cfg.Playlist.SigningKid)
	}
}

func TestLoad_envOverrides(t *testing.T) {
	t.Setenv("DP1_FEED_DATABASE_URL", "postgres://from-env/db")
	t.Setenv("DP1_FEED_API_KEY", "env-api-key")
	t.Setenv("DP1_FEED_SIGNING_KEY_HEX", testSeedHex)
	t.Setenv("DP1_FEED_SERVER_HOST", "10.0.0.1")
	t.Setenv("DP1_FEED_SERVER_PORT", "12345")
	t.Setenv("DP1_FEED_LOG_DEBUG", "true")
	t.Setenv("DP1_FEED_EXTENSIONS_ENABLED", "0")
	t.Setenv("DP1_FEED_PUBLIC_BASE_URL", "https://example.com/")
	t.Setenv("DP1_FEED_WEBHOOK_PRIVATE_KEY_HEX", testWebhookPrivateKeyHex)
	t.Setenv("DP1_FEED_NOTIFICATION_CLIENTS", `[{"name":"catalog","url":"https://catalog.example/webhooks/v1/channels"}]`)

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Database.URL != "postgres://from-env/db" {
		t.Fatalf("DATABASE_URL override: got %q", cfg.Database.URL)
	}
	if cfg.Auth.APIKey != "env-api-key" {
		t.Fatalf("API_KEY override")
	}
	if cfg.Server.Host != "10.0.0.1" || cfg.Server.Port != 12345 {
		t.Fatalf("server override: host=%q port=%d", cfg.Server.Host, cfg.Server.Port)
	}
	if !cfg.Logging.Debug {
		t.Fatalf("LOG_DEBUG override")
	}
	if cfg.Extensions.Enabled {
		t.Fatalf("EXTENSIONS_ENABLED=0 should disable extensions")
	}
	if cfg.Playlist.PublicBaseURL != "https://example.com" {
		t.Fatalf("PUBLIC_BASE_URL should trim trailing slash: got %q", cfg.Playlist.PublicBaseURL)
	}
	if len(cfg.Notifications.Clients) != 1 || cfg.Notifications.Clients[0].Name != "catalog" {
		t.Fatalf("notification clients override: %#v", cfg.Notifications.Clients)
	}
	if !strings.HasPrefix(cfg.Notifications.PublicKey, "p256:") {
		t.Fatalf("derived webhook public key = %q", cfg.Notifications.PublicKey)
	}
}

func TestLoad_notificationClientsFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	content := strings.TrimSpace(`
database:
  url: postgres://user:pass@localhost:5432/db?sslmode=disable
auth:
  api_key: integration-test-key
playlist:
  signing_key_hex: "` + testSeedHex + `"
  public_base_url: https://feed.example
notifications:
  timeout: 7s
  private_key_hex: "` + testWebhookPrivateKeyHex + `"
  clients:
    - name: catalog
      url: https://catalog.example/webhooks/v1/channels
`)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Notifications.Timeout.String() != "7s" {
		t.Fatalf("notification timeout = %s", cfg.Notifications.Timeout)
	}
	if len(cfg.Notifications.Clients) != 1 {
		t.Fatalf("notification clients = %#v", cfg.Notifications.Clients)
	}
	client := cfg.Notifications.Clients[0]
	if client.Name != "catalog" || client.URL != "https://catalog.example/webhooks/v1/channels" {
		t.Fatalf("notification client = %#v", client)
	}
	if !strings.HasPrefix(cfg.Notifications.PublicKey, "p256:") {
		t.Fatalf("derived webhook public key = %q", cfg.Notifications.PublicKey)
	}
}

func TestLoad_exampleConfigRejectsNotificationsWithLoopbackPublicBase(t *testing.T) {
	t.Setenv("DP1_FEED_SIGNING_KEY_HEX", testSeedHex)
	t.Setenv("DP1_FEED_WEBHOOK_PRIVATE_KEY_HEX", testWebhookPrivateKeyHex)
	t.Setenv("DP1_FEED_NOTIFICATION_CLIENTS", `[{"name":"catalog","url":"https://catalog.example/webhooks/v1/channels"}]`)

	path := filepath.Join("..", "..", "config", "config.yaml.example")
	_, err := Load(path)
	if err == nil || !strings.Contains(err.Error(), "must not use a loopback host") {
		t.Fatalf("Load error = %v, want loopback public base error", err)
	}
}

func TestLoad_invalidNotificationClientsEnv(t *testing.T) {
	t.Setenv("DP1_FEED_DATABASE_URL", "postgres://x")
	t.Setenv("DP1_FEED_API_KEY", "k")
	t.Setenv("DP1_FEED_SIGNING_KEY_HEX", testSeedHex)
	t.Setenv("DP1_FEED_NOTIFICATION_CLIENTS", "not-json")

	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "notification clients") {
		t.Fatalf("Load error = %v, want notification clients error", err)
	}
}

func TestLoad_invalidNotificationClient(t *testing.T) {
	t.Setenv("DP1_FEED_DATABASE_URL", "postgres://x")
	t.Setenv("DP1_FEED_API_KEY", "k")
	t.Setenv("DP1_FEED_SIGNING_KEY_HEX", testSeedHex)
	t.Setenv("DP1_FEED_PUBLIC_BASE_URL", "https://feed.example")
	t.Setenv("DP1_FEED_NOTIFICATION_CLIENTS", `[{"name":"catalog","url":"https://catalog.example/webhooks/v1/channels"}]`)

	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "webhook private key is required") {
		t.Fatalf("Load error = %v, want missing private key", err)
	}
}

func TestLoad_invalidWebhookPrivateKey(t *testing.T) {
	t.Setenv("DP1_FEED_DATABASE_URL", "postgres://x")
	t.Setenv("DP1_FEED_API_KEY", "k")
	t.Setenv("DP1_FEED_SIGNING_KEY_HEX", testSeedHex)
	t.Setenv("DP1_FEED_PUBLIC_BASE_URL", "https://feed.example")

	t.Setenv("DP1_FEED_WEBHOOK_PRIVATE_KEY_HEX", "not-a-private-key")

	_, err := Load("")
	if err == nil || !strings.Contains(err.Error(), "webhook private key") {
		t.Fatalf("Load error = %v, want invalid webhook private key", err)
	}
}

func TestValidate_notificationConfiguration(t *testing.T) {
	t.Parallel()

	valid := defaultConfig()
	valid.Database.URL = "postgres://x"
	valid.Auth.APIKey = "k"
	valid.Playlist.SigningKeyHex = testSeedHex
	valid.Playlist.PublicBaseURL = "https://feed.example"
	valid.Notifications.PrivateKeyHex = testWebhookPrivateKeyHex
	valid.Notifications.Clients = []NotificationClientConfig{{
		Name: "catalog",
		URL:  "https://catalog.example/webhooks/v1/channels",
	}}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{
			name: "unsupported webhook scheme",
			mutate: func(cfg *Config) {
				cfg.Notifications.Clients[0].URL = "ftp://catalog.example/webhooks"
			},
			wantErr: "absolute HTTP(S) URL",
		},
		{
			name: "webhook credentials",
			mutate: func(cfg *Config) {
				cfg.Notifications.Clients[0].URL = "https://user:token@catalog.example/webhooks"
			},
			wantErr: "must not contain credentials",
		},
		{
			name: "public base query",
			mutate: func(cfg *Config) {
				cfg.Playlist.PublicBaseURL = "https://feed.example?tenant=x"
			},
			wantErr: "must not contain a query or fragment",
		},
		{
			name: "public base fragment",
			mutate: func(cfg *Config) {
				cfg.Playlist.PublicBaseURL = "https://feed.example#channels"
			},
			wantErr: "must not contain a query or fragment",
		},
		{
			name: "public base empty query",
			mutate: func(cfg *Config) {
				cfg.Playlist.PublicBaseURL = "https://feed.example?"
			},
			wantErr: "must not contain a query or fragment",
		},
		{
			name: "public base empty fragment",
			mutate: func(cfg *Config) {
				cfg.Playlist.PublicBaseURL = "https://feed.example#"
			},
			wantErr: "must not contain a query or fragment",
		},
		{
			name: "public base credentials",
			mutate: func(cfg *Config) {
				cfg.Playlist.PublicBaseURL = "https://user:password@feed.example"
			},
			wantErr: "must not contain credentials",
		},
		{
			name: "public base unspecified IPv4",
			mutate: func(cfg *Config) {
				cfg.Playlist.PublicBaseURL = "http://0.0.0.0:8787"
			},
			wantErr: "must not use an unspecified host",
		},
		{
			name: "public base unspecified IPv6",
			mutate: func(cfg *Config) {
				cfg.Playlist.PublicBaseURL = "http://[::]:8787"
			},
			wantErr: "must not use an unspecified host",
		},
		{
			name: "non-positive fetch timeout",
			mutate: func(cfg *Config) {
				cfg.Playlist.FetchTimeout = 0
			},
			wantErr: "fetch timeout must be positive",
		},
		{
			name: "write timeout does not reserve notification time",
			mutate: func(cfg *Config) {
				cfg.Server.WriteTimeout = cfg.Playlist.FetchTimeout + cfg.Notifications.Timeout
			},
			wantErr: "write timeout must exceed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cfg := *valid
			cfg.Notifications.Clients = append([]NotificationClientConfig(nil), valid.Notifications.Clients...)
			tt.mutate(&cfg)
			err := cfg.validate()
			if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validate error = %v, want %q", err, tt.wantErr)
			}
		})
	}
}

func TestLoad_corsAllowOriginsEnv(t *testing.T) {
	t.Setenv("DP1_FEED_DATABASE_URL", "postgres://x")
	t.Setenv("DP1_FEED_API_KEY", "k")
	t.Setenv("DP1_FEED_SIGNING_KEY_HEX", testSeedHex)
	t.Setenv("DP1_FEED_CORS_ALLOW_ORIGINS", " https://app.example.com , https://other.example.com ")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.CORS.AllowOrigins) != 2 {
		t.Fatalf("CORS allow_origins: got %#v", cfg.CORS.AllowOrigins)
	}
	if cfg.CORS.AllowOrigins[0] != "https://app.example.com" || cfg.CORS.AllowOrigins[1] != "https://other.example.com" {
		t.Fatalf("CORS allow_origins mismatch: %#v", cfg.CORS.AllowOrigins)
	}
}

func TestLoad_corsAllowOriginsFromYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	content := strings.TrimSpace(`
database:
  url: postgres://user:pass@localhost:5432/db?sslmode=disable
auth:
  api_key: integration-test-key
cors:
  allow_origins:
    - "https://alpha.example"
    - "https://bravo.example"
playlist:
  signing_key_hex: "` + testSeedHex + `"
`)
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if len(cfg.CORS.AllowOrigins) != 2 {
		t.Fatalf("expected 2 origins, got %#v", cfg.CORS.AllowOrigins)
	}
	if cfg.CORS.AllowOrigins[0] != "https://alpha.example" || cfg.CORS.AllowOrigins[1] != "https://bravo.example" {
		t.Fatalf("CORS allow_origins mismatch: %#v", cfg.CORS.AllowOrigins)
	}
}

func TestLoad_serverPort_invalidEnvIgnored(t *testing.T) {
	t.Setenv("DP1_FEED_DATABASE_URL", "postgres://x")
	t.Setenv("DP1_FEED_API_KEY", "k")
	t.Setenv("DP1_FEED_SIGNING_KEY_HEX", testSeedHex)
	t.Setenv("DP1_FEED_SERVER_PORT", "not-a-number")

	cfg, err := Load("")
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 8787 {
		t.Fatalf("invalid SERVER_PORT should leave default port: got %d", cfg.Server.Port)
	}
}

func TestLoad_validateErrors(t *testing.T) {
	dir := t.TempDir()

	t.Run("missing_database_url", func(t *testing.T) {
		path := filepath.Join(dir, "no-db.yaml")
		yaml := strings.TrimSpace(`
database:
  url: ""
auth:
  api_key: k
playlist:
  signing_key_hex: "` + testSeedHex + `"
`)
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "database url is required") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("missing_api_key", func(t *testing.T) {
		path := filepath.Join(dir, "no-api.yaml")
		yaml := strings.TrimSpace(`
database:
  url: postgres://x
playlist:
  signing_key_hex: "` + testSeedHex + `"
`)
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "api key is required") {
			t.Fatalf("unexpected error: %v", err)
		}
	})

	t.Run("missing_signing_key", func(t *testing.T) {
		path := filepath.Join(dir, "no-sign.yaml")
		yaml := strings.TrimSpace(`
database:
  url: postgres://x
auth:
  api_key: k
`)
		if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
			t.Fatal(err)
		}
		_, err := Load(path)
		if err == nil {
			t.Fatal("expected error")
		}
		if !strings.Contains(err.Error(), "signing key is required") {
			t.Fatalf("unexpected error: %v", err)
		}
	})
}

func TestLoad_badYAML(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "bad.yaml")
	if err := os.WriteFile(path, []byte("database: [\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "config yaml") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_missingFile(t *testing.T) {
	_, err := Load(filepath.Join(t.TempDir(), "nope.yaml"))
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "config read") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_invalidSigningKey(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	yaml := strings.TrimSpace(`
database:
  url: postgres://x
auth:
  api_key: k
playlist:
  signing_key_hex: "deadbeef"
`)
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := Load(path)
	if err == nil {
		t.Fatal("expected error")
	}
	if !strings.Contains(err.Error(), "playlist signing key") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestLoad_yamlMergesWithDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cfg.yaml")
	yaml := strings.TrimSpace(`
database:
  url: postgres://x
auth:
  api_key: k
playlist:
  signing_key_hex: "` + testSeedHex + `"
server:
  port: 1111
`)
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	cfg, err := Load(path)
	if err != nil {
		t.Fatalf("Load: %v", err)
	}
	if cfg.Server.Port != 1111 {
		t.Fatalf("yaml server.port override")
	}
	// Not set in yaml — should remain default from defaultConfig().
	if cfg.Server.Host != "0.0.0.0" {
		t.Fatalf("expected default host, got %q", cfg.Server.Host)
	}
	if cfg.Extensions.Enabled != true {
		t.Fatalf("expected default extensions enabled")
	}
	if cfg.Playlist.FetchMaxBodyBytes != 4<<20 {
		t.Fatalf("expected default fetch max body bytes")
	}
}
