package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.uber.org/mock/gomock"
	"go.uber.org/zap/zaptest"

	"github.com/display-protocol/dp1-feed-v2/internal/config"
	"github.com/display-protocol/dp1-feed-v2/internal/mocks"
)

// newTestServer builds the production engine through New with a mock executor that expects no calls, so
// anything that reaches the executor fails the test. maxRequestBytes is passed through as configured
// (zero exercises the default).
func newTestServer(t *testing.T, maxRequestBytes int64) *Server {
	t.Helper()
	setGinTestMode()
	ctrl := gomock.NewController(t)
	cfg := &config.Config{
		Server: config.ServerConfig{
			ReadTimeout:          30 * time.Second,
			WriteTimeout:         30 * time.Second,
			IdleTimeout:          120 * time.Second,
			ResponseWriteReserve: time.Second,
			MaxRequestBytes:      maxRequestBytes,
		},
		Logging:    config.LoggingConfig{Debug: true},
		Extensions: config.ExtensionsConfig{Enabled: true},
	}
	return New(cfg, zaptest.NewLogger(t), mocks.NewMockExecutor(ctrl), "test")
}

// TestNew_CapsBodyBeforeSignatureCheck pins the order that closes the unauthenticated buffering hole:
// the body cap New installs on the http.Server handler must sit ahead of the per-route RequireSignatures
// that reads the whole body before any credential exists. If the cap ever moved behind that read — into
// gin after the routes, or off the served handler — an anonymous caller could again make the server
// buffer a body of their choosing, and this is the test that would notice: an unbounded, unsigned body
// must be answered 413 after at most cap+1 bytes on every mutating route, and never reach the executor.
func TestNew_CapsBodyBeforeSignatureCheck(t *testing.T) {
	const limit = 8192
	s := newTestServer(t, limit)

	routes := []struct{ method, path string }{
		{http.MethodPost, "/api/v1/playlists"},
		{http.MethodPut, "/api/v1/playlists/00000000-0000-4000-8000-000000000001"},
		{http.MethodDelete, "/api/v1/playlists/00000000-0000-4000-8000-000000000001"},
		{http.MethodPost, "/api/v1/playlist-groups"},
		{http.MethodPut, "/api/v1/playlist-groups/00000000-0000-4000-8000-000000000001"},
		{http.MethodDelete, "/api/v1/playlist-groups/00000000-0000-4000-8000-000000000001"},
		{http.MethodPost, "/api/v1/channels"},
		{http.MethodPut, "/api/v1/channels/00000000-0000-4000-8000-000000000001"},
		{http.MethodDelete, "/api/v1/channels/00000000-0000-4000-8000-000000000001"},
	}
	for _, rt := range routes {
		t.Run(rt.method+" "+rt.path, func(t *testing.T) {
			body := &endlessBody{budget: limit * 4}
			req := httptest.NewRequest(rt.method, rt.path, body)
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			s.srv.Handler.ServeHTTP(w, req)

			if w.Code != http.StatusRequestEntityTooLarge {
				t.Fatalf("want 413, got %d: %s", w.Code, w.Body.String())
			}
			if body.read > limit+1 {
				t.Fatalf("server pulled %d bytes of an unbounded unsigned body; cap is %d", body.read, limit)
			}
		})
	}
}

// TestNew_DefaultsBodyCap covers the fallback New applies when config leaves the cap unset: the default
// must be in force, not "no cap". An unbounded body must still stop at the default.
func TestNew_DefaultsBodyCap(t *testing.T) {
	s := newTestServer(t, 0)

	body := &endlessBody{budget: config.DefaultMaxRequestBytes * 4}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playlists", body)
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()

	s.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("want 413 under the default cap, got %d: %s", w.Code, w.Body.String())
	}
	if body.read > config.DefaultMaxRequestBytes+1 {
		t.Fatalf("server pulled %d bytes; default cap is %d", body.read, config.DefaultMaxRequestBytes)
	}
}

// TestNew_ReadsUnaffectedByBodyCap: the cap wraps every request, reads included, but a read never
// touches its body, so the wrapper must be inert there.
func TestNew_ReadsUnaffectedByBodyCap(t *testing.T) {
	s := newTestServer(t, 16)

	req := httptest.NewRequest(http.MethodGet, "/health", nil)
	w := httptest.NewRecorder()
	s.srv.Handler.ServeHTTP(w, req)

	if w.Code != http.StatusOK {
		t.Fatalf("want 200 from /health under a tiny cap, got %d: %s", w.Code, w.Body.String())
	}
}
