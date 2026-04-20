package httpserver

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/display-protocol/dp1-feed-v2/internal/config"
)

func TestNewCORSMiddleware_wildcard(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		CORS: config.CORSConfig{},
	}

	r := gin.New()
	r.Use(newCORSMiddleware(cfg))
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "/x", nil)
	req.Header.Set("Origin", "https://any.example")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "*", w.Header().Get("Access-Control-Allow-Origin"))
}

func TestNewCORSMiddleware_allowlist_reflectsOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		CORS: config.CORSConfig{
			AllowOrigins: []string{"https://app.example.com"},
		},
	}

	r := gin.New()
	r.Use(newCORSMiddleware(cfg))
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "http://server.test/x", nil)
	req.Header.Set("Origin", "https://app.example.com")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusOK, w.Code)
	assert.Equal(t, "https://app.example.com", w.Header().Get("Access-Control-Allow-Origin"))
}

func TestNewCORSMiddleware_allowlist_forbiddenOrigin(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{
		CORS: config.CORSConfig{
			AllowOrigins: []string{"https://app.example.com"},
		},
	}

	r := gin.New()
	r.Use(newCORSMiddleware(cfg))
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodGet, "http://server.test/x", nil)
	req.Header.Set("Origin", "https://evil.example")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	assert.Equal(t, http.StatusForbidden, w.Code)
}

func TestNewCORSMiddleware_preflightAuthorizationHeader(t *testing.T) {
	gin.SetMode(gin.TestMode)
	cfg := &config.Config{CORS: config.CORSConfig{}}

	r := gin.New()
	r.Use(newCORSMiddleware(cfg))
	r.GET("/x", func(c *gin.Context) { c.Status(http.StatusOK) })

	req := httptest.NewRequest(http.MethodOptions, "http://server.test/x", nil)
	req.Header.Set("Origin", "https://client.example")
	req.Header.Set("Access-Control-Request-Method", "POST")
	req.Header.Set("Access-Control-Request-Headers", "authorization,content-type")
	w := httptest.NewRecorder()
	r.ServeHTTP(w, req)

	require.Equal(t, http.StatusNoContent, w.Code)
	allowHeaders := w.Header().Get("Access-Control-Allow-Headers")
	assert.Contains(t, allowHeaders, "Authorization")
}
