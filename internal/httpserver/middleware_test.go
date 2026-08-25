package httpserver

import (
	"bytes"
	"context"
	"errors"
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

func TestSignatureOrAPIKeyAuth(t *testing.T) {
	setGinTestMode()
	log := zaptest.NewLogger(t)
	secret := "test-secret-key"

	t.Run("valid_api_key", func(t *testing.T) {
		router := gin.New()
		called := false
		router.POST("/test", SignatureOrAPIKeyAuth(secret, log), func(c *gin.Context) {
			called = true
			mode := GetAuthMode(c)
			if mode != AuthModeAPIKey {
				t.Errorf("expected AuthModeAPIKey, got %v", mode)
			}
			c.Status(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewReader([]byte(`{"title":"test"}`)))
		req.Header.Set("Authorization", "Bearer test-secret-key")
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

	t.Run("invalid_api_key", func(t *testing.T) {
		router := gin.New()
		called := false
		router.POST("/test", SignatureOrAPIKeyAuth(secret, log), func(c *gin.Context) {
			called = true
			c.Status(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewReader([]byte(`{"title":"test"}`)))
		req.Header.Set("Authorization", "Bearer wrong-key")
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", w.Code)
		}
		if called {
			t.Error("handler should not be called with invalid key")
		}
	})

	t.Run("valid_signatures_in_body", func(t *testing.T) {
		router := gin.New()
		called := false
		router.POST("/test", SignatureOrAPIKeyAuth(secret, log), func(c *gin.Context) {
			called = true
			mode := GetAuthMode(c)
			if mode != AuthModeSignature {
				t.Errorf("expected AuthModeSignature, got %v", mode)
			}
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
	})

	t.Run("empty_signatures_array", func(t *testing.T) {
		router := gin.New()
		router.POST("/test", SignatureOrAPIKeyAuth(secret, log), func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		body := `{"title":"test","signatures":[]}`
		req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", w.Code)
		}
	})

	t.Run("no_auth_header_no_signatures", func(t *testing.T) {
		router := gin.New()
		router.POST("/test", SignatureOrAPIKeyAuth(secret, log), func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		body := `{"title":"test"}`
		req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewReader([]byte(body)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", w.Code)
		}
	})

	t.Run("invalid_json_body", func(t *testing.T) {
		router := gin.New()
		router.POST("/test", SignatureOrAPIKeyAuth(secret, log), func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodPost, "/test", bytes.NewReader([]byte(`{invalid json`)))
		req.Header.Set("Content-Type", "application/json")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", w.Code)
		}
	})

	t.Run("put_valid_signatures_in_body", func(t *testing.T) {
		router := gin.New()
		called := false
		router.PUT("/playlists/:id", SignatureOrAPIKeyAuth(secret, log), func(c *gin.Context) {
			called = true
			mode := GetAuthMode(c)
			if mode != AuthModeSignature {
				t.Errorf("expected AuthModeSignature, got %v", mode)
			}
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
}

func TestAPIKeyAuth(t *testing.T) {
	setGinTestMode()
	log := zaptest.NewLogger(t)
	secret := "test-secret"

	t.Run("valid_key", func(t *testing.T) {
		router := gin.New()
		called := false
		router.POST("/test", APIKeyAuth(secret, log), func(c *gin.Context) {
			called = true
			mode := GetAuthMode(c)
			if mode != AuthModeAPIKey {
				t.Errorf("expected AuthModeAPIKey, got %v", mode)
			}
			c.Status(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodPost, "/test", nil)
		req.Header.Set("Authorization", "Bearer test-secret")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)

		if w.Code != http.StatusOK {
			t.Errorf("expected 200, got %d", w.Code)
		}
		if !called {
			t.Error("handler was not called")
		}
	})

	t.Run("invalid_key", func(t *testing.T) {
		router := gin.New()
		router.POST("/test", APIKeyAuth(secret, log), func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodPost, "/test", nil)
		req.Header.Set("Authorization", "Bearer wrong")
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", w.Code)
		}
	})

	t.Run("missing_key", func(t *testing.T) {
		router := gin.New()
		router.POST("/test", APIKeyAuth(secret, log), func(c *gin.Context) {
			c.Status(http.StatusOK)
		})

		req := httptest.NewRequest(http.MethodPost, "/test", nil)
		w := httptest.NewRecorder()

		router.ServeHTTP(w, req)

		if w.Code != http.StatusUnauthorized {
			t.Errorf("expected 401, got %d", w.Code)
		}
	})
}

func TestAuthModeContext(t *testing.T) {
	setGinTestMode()

	t.Run("set_and_get_api_key", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		SetAuthMode(c, AuthModeAPIKey)
		mode := GetAuthMode(c)
		if mode != AuthModeAPIKey {
			t.Errorf("expected AuthModeAPIKey, got %v", mode)
		}
	})

	t.Run("set_and_get_signature", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		SetAuthMode(c, AuthModeSignature)
		mode := GetAuthMode(c)
		if mode != AuthModeSignature {
			t.Errorf("expected AuthModeSignature, got %v", mode)
		}
	})

	t.Run("default_mode", func(t *testing.T) {
		c, _ := gin.CreateTestContext(httptest.NewRecorder())
		mode := GetAuthMode(c)
		if mode != AuthModeAPIKey {
			t.Errorf("expected default AuthModeAPIKey, got %v", mode)
		}
	})
}
