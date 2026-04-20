package httpserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// writeJSONIndividualGET JSON-encodes body, sets a strong ETag over the exact UTF-8 response bytes
// (quoted SHA-256 hex digest), and returns 304 Not Modified with an empty body when If-None-Match
// matches. Intended for single-resource GET handlers only (not list or registry aggregate GETs).
func writeJSONIndividualGET(c *gin.Context, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	etag := strongETagFromJSONBytes(b)
	c.Header("ETag", etag)
	if ifNoneMatchNotModified(c.Request, etag) {
		c.Status(http.StatusNotModified)
		// Gin defers flushing status until first write; 304 has no body, so flush explicitly so clients
		// and httptest see the correct status code.
		c.Writer.WriteHeaderNow()
		return nil
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", b)
	return nil
}

func strongETagFromJSONBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// ifNoneMatchNotModified reports whether If-None-Match matches etag such that GET should return 304.
func ifNoneMatchNotModified(r *http.Request, etag string) bool {
	inm := r.Header.Get("If-None-Match")
	if inm == "" {
		return false
	}
	if strings.TrimSpace(inm) == "*" {
		// Representation exists: "*" does not short-circuit to 304 for GET (RFC 9110).
		return false
	}
	for part := range strings.SplitSeq(inm, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		part = strings.TrimPrefix(part, "W/")
		part = strings.TrimSpace(part)
		if part == etag {
			return true
		}
	}
	return false
}
