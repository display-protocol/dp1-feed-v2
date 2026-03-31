package httpserver

import (
	"crypto/subtle"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

const authHeader = "Authorization"

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
		c.Next()
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
