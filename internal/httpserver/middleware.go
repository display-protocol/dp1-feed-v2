package httpserver

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const authHeader = "Authorization"

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

		// Read and restore body so handlers can still bind it
		body, err := c.GetRawData()
		if err != nil {
			log.Warn("unauthorized: cannot read request body", zap.String("path", c.Request.URL.Path), zap.Error(err))
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized", Message: "missing authentication: provide API key or signatures"})
			return
		}
		c.Request.Body = &bodyReaderCloser{body: body}

		if err := json.Unmarshal(body, &bodyCheck); err != nil {
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

// bodyReaderCloser wraps a byte slice to implement io.ReadCloser for restoring request body.
type bodyReaderCloser struct {
	body   []byte
	offset int
}

func (b *bodyReaderCloser) Read(p []byte) (n int, err error) {
	if b.offset >= len(b.body) {
		return 0, nil
	}
	n = copy(p, b.body[b.offset:])
	b.offset += n
	return n, nil
}

func (b *bodyReaderCloser) Close() error {
	return nil
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
