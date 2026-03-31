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
