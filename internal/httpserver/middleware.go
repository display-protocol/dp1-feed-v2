package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"
	"go.uber.org/zap"
)

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

// RequireSignatures gates every mutating route (POST/PUT/DELETE). There is no API key: a mutating request
// must carry a non-empty top-level "signatures" array in its JSON body, which the executor then verifies
// cryptographically (POST/PUT over the document; DELETE over the signed delete-intent). This middleware
// only checks for presence — a cheap pre-filter so unsigned requests never reach handler/executor work;
// authenticity and authorization are the executor's job.
//
// The body is read once and restored as an io.NopCloser(bytes.Reader) so the handler can bind it again.
// The replacement must report io.EOF (handlers read to EOF); a reader returning (0, nil) at the end would
// make io.ReadAll spin forever.
func RequireSignatures(log *zap.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		body, err := c.GetRawData()
		if err != nil {
			log.Warn("unauthorized: cannot read request body", zap.String("path", c.Request.URL.Path), zap.Error(err))
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized", Message: "missing authentication: request body must carry signatures"})
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))

		var bodyCheck struct {
			Signatures []json.RawMessage `json:"signatures"`
		}
		if err := json.Unmarshal(body, &bodyCheck); err != nil {
			log.Warn("unauthorized: invalid JSON body", zap.String("path", c.Request.URL.Path), zap.Error(err))
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized", Message: "missing authentication: request body must carry signatures"})
			return
		}
		if len(bodyCheck.Signatures) == 0 {
			log.Warn("unauthorized: no signatures", zap.String("path", c.Request.URL.Path))
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized", Message: "missing authentication: request body must carry signatures"})
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
