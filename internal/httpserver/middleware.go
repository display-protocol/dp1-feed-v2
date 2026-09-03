package httpserver

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const authHeader = "Authorization"

// RequestDeadline makes the handler budget visible to persistence and outbound
// notification calls. The HTTP server's WriteTimeout is only a socket deadline.
func RequestDeadline(timeout time.Duration) gin.HandlerFunc {
	return func(c *gin.Context) {
		ctx, cancel := context.WithTimeout(c.Request.Context(), timeout)
		defer cancel()
		c.Request = c.Request.WithContext(ctx)
		c.Next()
	}
}

// AuthMode distinguishes API key (ops) vs signature-based (user) authentication paths.
type AuthMode int

const (
	// AuthModeAPIKey indicates request used API key authentication (ops path).
	AuthModeAPIKey AuthMode = iota
	// AuthModeSignature indicates request used cryptographic signature authentication (user path).
	AuthModeSignature
)

const authModeKey = "auth_mode"

// SetAuthMode stores the authentication mode in the Gin context for executor access.
func SetAuthMode(c *gin.Context, mode AuthMode) {
	c.Set(authModeKey, mode)
}

// GetAuthMode retrieves the authentication mode from the Gin context; defaults to AuthModeAPIKey if not set.
func GetAuthMode(c *gin.Context) AuthMode {
	if val, exists := c.Get(authModeKey); exists {
		if mode, ok := val.(AuthMode); ok {
			return mode
		}
	}
	return AuthModeAPIKey
}

// APIKeyAuth requires Authorization: Bearer <secret> for mutating routes.
// Compares the full header value in constant time to reduce timing leakage of the API key length/prefix.
func APIKeyAuth(secret string, log *zap.Logger) gin.HandlerFunc {
	want := []byte("Bearer " + secret)
	return func(c *gin.Context) {
		got := []byte(c.GetHeader(authHeader))
		if len(got) != len(want) || subtle.ConstantTimeCompare(got, want) != 1 {
			log.Warn("unauthorized", zap.String("path", c.Request.URL.Path))
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized", Message: "invalid or missing API key"})
			return
		}
		SetAuthMode(c, AuthModeAPIKey)
		c.Next()
	}
}

// SignatureOrAPIKeyAuth accepts either API key (ops path) or valid signatures in request body (user path).
// Used for POST (create) and PUT/PATCH (replace/update) on playlists, playlist-groups, and channels:
// requests with a non-empty signatures[] array may omit the API key; the executor verifies signatures.
//
// Authentication flow:
//   - Path A (Ops): Has Authorization: Bearer header → validate API key → set AuthModeAPIKey
//   - Path B (User): No Authorization header but has signatures[] in body → set AuthModeSignature
//   - Reject: No Authorization header and no signatures in body
func SignatureOrAPIKeyAuth(secret string, log *zap.Logger) gin.HandlerFunc {
	want := []byte("Bearer " + secret)
	return func(c *gin.Context) {
		// Check if API key is present
		got := []byte(c.GetHeader(authHeader))
		if len(got) > 0 {
			// Path A: API key authentication
			if len(got) == len(want) && subtle.ConstantTimeCompare(got, want) == 1 {
				SetAuthMode(c, AuthModeAPIKey)
				c.Next()
				return
			}
			// Invalid API key
			log.Warn("unauthorized: invalid API key", zap.String("path", c.Request.URL.Path))
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized", Message: "invalid or missing API key"})
			return
		}

		// No API key header; check if request has signatures in body (Path B: signature authentication)
		// Peek at request body to check for signatures[] array
		var bodyCheck struct {
			Signatures []interface{} `json:"signatures"`
		}

		// Read and restore the body so the handler can read it again. The replacement must be a real
		// io.Reader that reports io.EOF: handlers read the body to EOF (bindDocument → GetRawData), and a
		// reader that returns (0, nil) at the end would make io.ReadAll spin forever.
		body, err := c.GetRawData()
		if err != nil {
			log.Warn("unauthorized: cannot read request body", zap.String("path", c.Request.URL.Path), zap.Error(err))
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized", Message: "missing authentication: provide API key or signatures"})
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))

		// Decode only the first JSON value, not the whole body: json.Unmarshal rejects a valid object
		// followed by trailing bytes, which would make a signed-but-trailing request a misleading 401.
		// Using a Decoder lets the handler's bindDocument return the documented 400 for trailing content.
		if err := json.NewDecoder(bytes.NewReader(body)).Decode(&bodyCheck); err != nil {
			log.Warn("unauthorized: invalid JSON body", zap.String("path", c.Request.URL.Path), zap.Error(err))
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized", Message: "missing authentication: provide API key or signatures"})
			return
		}

		// Path B: signature-based authentication
		if len(bodyCheck.Signatures) > 0 {
			SetAuthMode(c, AuthModeSignature)
			c.Next()
			return
		}

		// Neither API key nor signatures present
		log.Warn("unauthorized: no API key or signatures", zap.String("path", c.Request.URL.Path))
		c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized", Message: "missing authentication: provide API key or signatures"})
	}
}

// ZapLogger emits basic request logs (method, path, status, latency).
func ZapLogger(log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		start := time.Now()
		c.Next()
		log.Info("http",
			zap.String("method", c.Request.Method),
			zap.String("path", c.Request.URL.Path),
			zap.Int("status", c.Writer.Status()),
			zap.Duration("latency", time.Since(start)),
		)
	}
}
