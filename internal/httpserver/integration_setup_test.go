//go:build integration

package httpserver

import (
	"bytes"
	"context"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"github.com/display-protocol/dp1-go/playlist"
	dp1sign "github.com/display-protocol/dp1-go/sign"
	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/display-protocol/dp1-feed-v2/internal/config"
	"github.com/display-protocol/dp1-feed-v2/internal/dp1svc"
	"github.com/display-protocol/dp1-feed-v2/internal/executor"
	"github.com/display-protocol/dp1-feed-v2/internal/store"
	"github.com/display-protocol/dp1-feed-v2/internal/store/pg/pgtest"
)

// Shared harness for the HTTP integration tests in this package: one Postgres container for the
// whole package (TestMain), truncated between tests by the provider's Cleanup.

const integrationSeedHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

var integrationTestProvider store.TestProvider

func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	integrationTestProvider, err = pgtest.NewProvider(ctx)
	if err != nil {
		fmt.Fprintf(os.Stderr, "httpserver integration setup: %v\n", err)
		os.Exit(1)
	}
	code := m.Run()
	integrationTestProvider.Close()
	os.Exit(code)
}

// newIntegrationServer builds a server backed by the shared Postgres provider with extensions
// enabled, and registers the per-test database cleanup.
func newIntegrationServer(t *testing.T) *Server {
	t.Helper()
	t.Cleanup(func() {
		integrationTestProvider.Cleanup(t)
	})

	priv, err := dp1svc.Ed25519PrivateKeyFromHex(integrationSeedHex)
	if err != nil {
		t.Fatal(err)
	}
	pub, ok := priv.Public().(ed25519.PublicKey)
	if !ok {
		t.Fatal("unexpected public key type")
	}
	kid, err := dp1sign.Ed25519DIDKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	dp1Service, err := dp1svc.New(integrationSeedHex, kid)
	if err != nil {
		t.Fatal(err)
	}

	cfg := &config.Config{
		Server: config.ServerConfig{
			ReadTimeout:  30 * time.Second,
			WriteTimeout: 30 * time.Second,
			IdleTimeout:  120 * time.Second,
		},
		Auth:       config.AuthConfig{APIKey: "integration-api-key"},
		Logging:    config.LoggingConfig{Debug: true},
		Extensions: config.ExtensionsConfig{Enabled: true},
		Playlist: config.PlaylistConfig{
			SigningKeyHex: integrationSeedHex,
			SigningKid:    kid,
			PublicBaseURL: "http://example.com",
		},
		CORS: config.CORSConfig{},
	}

	gin.SetMode(gin.TestMode)
	exec := executor.New(integrationTestProvider.NewStore(), dp1Service, true, nil, cfg.Playlist.PublicBaseURL)
	return New(cfg, zap.NewNop(), exec, "test")
}

func mustDoPlaylistJSON(t *testing.T, srv *Server, method, path string, body any, wantStatus int) playlist.Playlist {
	t.Helper()
	var out playlist.Playlist
	mustDoJSON(t, srv, method, path, body, wantStatus, &out)
	return out
}

func mustDoJSON(t *testing.T, srv *Server, method, path string, body any, wantStatus int, out any) {
	t.Helper()
	raw := mustDoRaw(t, srv, method, path, body, wantStatus)
	if out == nil {
		return
	}
	if err := json.Unmarshal(raw, out); err != nil {
		t.Fatalf("%s %s: decode response: %v body=%s", method, path, err, raw)
	}
}

// mustDoRaw returns the undecoded response body, which callers need when the exact bytes matter
// (signature verification, raw-JSON fields such as inlineManifest).
func mustDoRaw(t *testing.T, srv *Server, method, path string, body any, wantStatus int) []byte {
	t.Helper()
	return doRaw(t, srv, method, path, body, wantStatus, true)
}

// mustDoRawUnauthenticated sends no API key, which is how a curator publishes: a body carrying
// signatures[] takes the signature-based path in SignatureOrAPIKeyAuth instead.
func mustDoRawUnauthenticated(t *testing.T, srv *Server, method, path string, body any, wantStatus int) []byte {
	t.Helper()
	return doRaw(t, srv, method, path, body, wantStatus, false)
}

func doRaw(t *testing.T, srv *Server, method, path string, body any, wantStatus int, withAPIKey bool) []byte {
	t.Helper()
	var payload *bytes.Reader
	if body == nil {
		payload = bytes.NewReader(nil)
	} else {
		raw, err := json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
		payload = bytes.NewReader(raw)
	}

	req := httptest.NewRequest(method, path, payload)
	req.Header.Set("Content-Type", "application/json")
	if withAPIKey && (method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete) {
		req.Header.Set("Authorization", "Bearer integration-api-key")
	}
	rec := httptest.NewRecorder()
	srv.engine.ServeHTTP(rec, req)
	if rec.Code != wantStatus {
		t.Fatalf("%s %s: status=%d want %d body=%s", method, path, rec.Code, wantStatus, rec.Body.String())
	}
	return rec.Body.Bytes()
}
