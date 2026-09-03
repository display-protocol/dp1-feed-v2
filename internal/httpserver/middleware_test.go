package httpserver

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap/zaptest"
)

var ginModeOnce sync.Once

func setGinTestMode() {
	ginModeOnce.Do(func() {
		gin.SetMode(gin.TestMode)
	})
}

func TestRequestDeadline(t *testing.T) {
	setGinTestMode()
	router := gin.New()
	timeout := 100 * time.Millisecond
	router.POST("/test", RequestDeadline(timeout), func(c *gin.Context) {
		deadline, ok := c.Request.Context().Deadline()
		if !ok {
			t.Fatal("request context has no deadline")
		}
		remaining := time.Until(deadline)
		if remaining <= 0 || remaining > timeout {
			t.Fatalf("request deadline remaining = %s, want within (0, %s]", remaining, timeout)
		}
		c.Status(http.StatusNoContent)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusNoContent)
	}
}

func TestRequestDeadlineCancelsDownstreamWork(t *testing.T) {
	setGinTestMode()
	router := gin.New()
	router.POST("/test", RequestDeadline(10*time.Millisecond), func(c *gin.Context) {
		<-c.Request.Context().Done()
		if !errors.Is(c.Request.Context().Err(), context.DeadlineExceeded) {
			t.Fatalf("request context error = %v, want deadline exceeded", c.Request.Context().Err())
		}
		c.Status(http.StatusGatewayTimeout)
	})

	req := httptest.NewRequest(http.MethodPost, "/test", nil)
	w := httptest.NewRecorder()
	router.ServeHTTP(w, req)
	if w.Code != http.StatusGatewayTimeout {
		t.Fatalf("status = %d, want %d", w.Code, http.StatusGatewayTimeout)
	}
}

// TestRequireSignatures covers the presence-only gate on mutating routes: a non-empty top-level
// "signatures" array passes through (and the handler can still read the body), everything else is 401.
func TestRequireSignatures(t *testing.T) {
	setGinTestMode()
	log := zaptest.NewLogger(t)

	t.Run("valid_signatures_in_body_passes_and_body_readable", func(t *testing.T) {
		router := gin.New()
		called := false
		var seen []byte
		router.POST("/test", RequireSignatures(log), func(c *gin.Context) {
			called = true
			b, err := io.ReadAll(c.Request.Body)
			if err != nil {
				t.Fatalf("handler could not read body: %v", err)
			}
			seen = b
			c.Status(http.StatusOK)
		})

		body := `{"title":"test","signatures":[{"kid":"did:key:abc","alg":"ed25519","sig":"xyz"}]}`
		req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if !called {
			t.Error("handler was not called")
		}
		if string(seen) != body {
			t.Errorf("handler saw body %q, want %q", seen, body)
		}
	})

	t.Run("put_with_signatures_passes", func(t *testing.T) {
		router := gin.New()
		called := false
		router.PUT("/playlists/:id", RequireSignatures(log), func(c *gin.Context) {
			called = true
			c.Status(http.StatusOK)
		})

		body := `{"dpVersion":"1.1.0","title":"t","items":[],"signatures":[{"kid":"did:key:abc","alg":"ed25519","sig":"x"}]}`
		req := httptest.NewRequest(http.MethodPut, "/playlists/11111111-1111-1111-1111-111111111111", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d: %s", w.Code, w.Body.String())
		}
		if !called {
			t.Error("handler was not called")
		}
	})

	t.Run("delete_with_signed_intent_passes", func(t *testing.T) {
		router := gin.New()
		called := false
		router.DELETE("/playlists/:id", RequireSignatures(log), func(c *gin.Context) {
			called = true
			c.Status(http.StatusNoContent)
		})

		body := `{"action":"delete","target":{"type":"playlist","id":"x","slug":"y"},"created":"2026-01-01T00:00:00Z","signatures":[{"kid":"did:key:abc","alg":"ed25519","sig":"x"}]}`
		req := httptest.NewRequest(http.MethodDelete, "/playlists/11111111-1111-1111-1111-111111111111", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)

		if w.Code != http.StatusNoContent {
			t.Errorf("expected 204, got %d: %s", w.Code, w.Body.String())
		}
		if !called {
			t.Error("handler was not called")
		}
	})

	rejects := []struct {
		name string
		body string
	}{
		{"empty_signatures_array", `{"title":"test","signatures":[]}`},
		{"no_signatures", `{"title":"test"}`},
		{"null_signatures", `{"title":"test","signatures":null}`},
		{"invalid_json", `{invalid json`},
		{"empty_body", ``},
	}
	for _, tc := range rejects {
		t.Run("rejects_"+tc.name, func(t *testing.T) {
			router := gin.New()
			called := false
			router.POST("/test", RequireSignatures(log), func(c *gin.Context) {
				called = true
				c.Status(http.StatusOK)
			})

			req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewReader([]byte(tc.body)))
			req.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()

			router.ServeHTTP(w, req)

			if w.Code != http.StatusUnauthorized {
				t.Errorf("expected 401, got %d: %s", w.Code, w.Body.String())
			}
			if called {
				t.Error("handler should not be called when signatures are absent")
			}
		})
	}
}
