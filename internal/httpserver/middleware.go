package httpserver

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
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

// LimitBody caps the inbound request body so a client cannot force the server to buffer an unbounded
// payload before authentication (the signature middleware reads the whole body). It wraps the body in an
// http.MaxBytesReader; a read past the limit fails, and RequireSignatures / handlers surface that as 413.
// A non-positive max disables the cap.
func LimitBody(maxBytes int64) gin.HandlerFunc {
	return func(c *gin.Context) {
		if maxBytes > 0 && c.Request.Body != nil {
			c.Request.Body = http.MaxBytesReader(c.Writer, c.Request.Body, maxBytes)
		}
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
			// A body larger than the configured cap (LimitBody) surfaces here as a MaxBytesError before we
			// buffer it all; report it as 413 rather than a generic auth failure.
			var maxErr *http.MaxBytesError
			if errors.As(err, &maxErr) {
				log.Warn("request body too large", zap.String("path", c.Request.URL.Path), zap.Int64("limit", maxErr.Limit))
				c.AbortWithStatusJSON(http.StatusRequestEntityTooLarge, ErrorResponse{Error: "payload_too_large", Message: "request body exceeds the configured size limit"})
				return
			}
			log.Warn("unauthorized: cannot read request body", zap.String("path", c.Request.URL.Path), zap.Error(err))
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized", Message: "missing authentication: request body must carry signatures"})
			return
		}
		c.Request.Body = io.NopCloser(bytes.NewReader(body))

		carries, decodeErr := bodyCarriesSignatures(body)
		if decodeErr != nil {
			// Malformed JSON is a client error, not an authentication failure: strict decoding promises a
			// 400 naming the problem, and signing an unparseable body could never satisfy this check.
			log.Warn("bad request: malformed body", zap.String("path", c.Request.URL.Path), zap.Error(decodeErr))
			c.AbortWithStatusJSON(http.StatusBadRequest, ErrorResponse{Error: "bad_request", Message: decodeErr.Error()})
			return
		}
		if !carries {
			log.Warn("unauthorized: no signatures", zap.String("path", c.Request.URL.Path))
			c.AbortWithStatusJSON(http.StatusUnauthorized, ErrorResponse{Error: "unauthorized", Message: "missing authentication: request body must carry signatures"})
			return
		}
		c.Next()
	}
}

// bodyCarriesSignatures reports whether a mutating body carries signatures anywhere the API accepts them.
// This is presence only — authenticity and authority are the executor's job; the point is to reject an
// unsigned request before any handler or database work.
//
// Two shapes are legitimate: a bare signed document or delete-intent (POST, DELETE) carries `signatures`
// at the top level, while a PUT carries a `document` and the `authorization` intent that permits
// replacing the resource with it, each signed in its own right.
// bodyCarriesSignatures reports whether the body presents signatures, and separately whether it is even
// well-formed JSON.
//
// The two answers must not be conflated. A malformed body — bad syntax, or more than one JSON value —
// cannot present signatures, but reporting that as "unauthenticated" tells the client the wrong thing:
// the API documents such a body as a 400 from strict decoding, and no amount of signing would fix it. It
// previously returned 401 here, because a failed Unmarshal was indistinguishable from an unsigned body.
//
// Unknown members are deliberately NOT rejected here. That is the route decoder's job, against the
// concrete request type; this only needs syntax and signature presence, and duplicating the strict rules
// pre-auth would mean maintaining them twice.
func bodyCarriesSignatures(body []byte) (carries bool, err error) {
	var envelope struct {
		Signatures    []json.RawMessage `json:"signatures"`
		Document      json.RawMessage   `json:"document"`
		Authorization json.RawMessage   `json:"authorization"`
	}
	// An empty body is not malformed, it is simply unsigned: there is nothing to decode and nothing to
	// report as a syntax problem. Reporting it as a decode failure would turn the plainest possible
	// authentication failure — a mutating request sent with no credentials at all — into a 400.
	if len(bytes.TrimSpace(body)) == 0 {
		return false, nil
	}
	dec := json.NewDecoder(bytes.NewReader(body))
	if err := dec.Decode(&envelope); err != nil {
		return false, err
	}
	if dec.More() {
		return false, errTrailingBody
	}
	if len(envelope.Signatures) > 0 {
		return true, nil
	}
	// Both halves of a replace must be signed; one without the other authorizes nothing.
	return hasSignatures(envelope.Document) && hasSignatures(envelope.Authorization), nil
}

func hasSignatures(raw json.RawMessage) bool {
	if len(raw) == 0 {
		return false
	}
	var doc struct {
		Signatures []json.RawMessage `json:"signatures"`
	}
	if err := json.Unmarshal(raw, &doc); err != nil {
		return false
	}
	return len(doc.Signatures) > 0
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
