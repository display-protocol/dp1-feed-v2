package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"

	"github.com/display-protocol/dp1-feed-v2/internal/config"
)

// newRoutedEngine builds the real routing table. The handler is never invoked by these cases — they all
// miss every registered route — so a zero Handler is enough and no store or executor is needed.
func newRoutedEngine(t *testing.T) *gin.Engine {
	t.Helper()
	setGinTestMode()
	r := gin.New()
	RegisterRoutes(r, &Handler{}, &config.Config{}, zap.NewNop())
	return r
}

// Unmatched requests answer in the API's error envelope rather than the framework's plain-text default.
//
// This is the one client-facing behavior this change introduces, and it exists because removing an
// endpoint should not hand callers a different response shape than every other error. It also serves as
// the durable record that the curated channel registry is gone: a future contributor re-registering a
// /registry route fails this test rather than silently reviving a removed surface.
func TestRegisterRoutes_unmatchedRequestsUseTheErrorEnvelope(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name, method, path string
	}{
		{"removed channel registry", http.MethodGet, "/api/v1/registry/channels"},
		{"removed registry write", http.MethodPut, "/api/v1/registry/channels"},
		{"unknown path under the API prefix", http.MethodGet, "/api/v1/nope"},
		{"unknown path outside it", http.MethodGet, "/nope"},
		// PATCH was removed with the signatures-only change; it has no route on any resource.
		{"removed PATCH verb", http.MethodPatch, "/api/v1/playlists/11111111-1111-1111-1111-111111111111"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := newRoutedEngine(t)
			req := httptest.NewRequest(tc.method, tc.path, nil)
			w := httptest.NewRecorder()
			r.ServeHTTP(w, req)

			if w.Code != http.StatusNotFound {
				t.Fatalf("want 404, got %d: %s", w.Code, w.Body.String())
			}
			if ct := w.Header().Get("Content-Type"); !strings.Contains(ct, "application/json") {
				t.Fatalf("want a JSON content type, got %q (body %s)", ct, w.Body.String())
			}
			var resp ErrorResponse
			if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
				t.Fatalf("body should be the error envelope, got %s", w.Body.String())
			}
			if resp.Error != "not_found" {
				t.Fatalf("want the not_found code, got %q", resp.Error)
			}
			if resp.Message == "" {
				t.Fatal("error envelope should carry a message")
			}
		})
	}
}

// Routes that do exist must not be swallowed by the NoRoute handler.
func TestRegisterRoutes_registeredRoutesStillMatch(t *testing.T) {
	t.Parallel()
	r := newRoutedEngine(t)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/channels", nil)
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	// Extensions are off in this config, so the channel routes answer 404 with extensions_disabled — a
	// registered route reached, not a routing miss. The distinction is the point: NoRoute must not
	// capture paths the table already covers.
	var resp ErrorResponse
	if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
		t.Fatalf("expected the error envelope, got %s", w.Body.String())
	}
	if resp.Error != "extensions_disabled" {
		t.Fatalf("registered route should answer extensions_disabled, got %q (%s)", resp.Error, w.Body.String())
	}
}
