package httpserver

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"
)

// writeJSONIndividualGET JSON-encodes body and serves it via writeBytesIndividualGET.
// Intended for single-resource GET handlers only (not list GETs).
func writeJSONIndividualGET(c *gin.Context, body any) error {
	b, err := json.Marshal(body)
	if err != nil {
		return err
	}
	writeBytesIndividualGET(c, b)
	return nil
}

// writeBytesIndividualGET serves b unchanged, sets a strong ETag over those exact UTF-8 bytes (quoted
// SHA-256 hex digest), and returns 304 Not Modified with an empty body when If-None-Match matches.
// Documents are served from their stored bytes (see writeDocument), so the ETag is over what the
// client receives and any other representation of the same document would be a different tag.
func writeBytesIndividualGET(c *gin.Context, b []byte) {
	serveIndividualGET(c, b, strongETagFromJSONBytes(b))
}

// writeBytesIndividualGETWithRevision serves b unchanged under a strong ETag over b plus an out-of-band
// revision. GET /channels/{id} uses it: a channel's signed bytes are served verbatim and list members by
// URL only, so they cannot reflect a member playlist being replaced or deleted — store.ChannelRecord.
// MembersDigest carries that state and is folded into the tag. The tag is still a strong validator (it
// changes whenever the bytes change); it additionally changes when member state does, so a client holding
// the old tag gets 200 with a byte-identical body, which is the signal to re-check the members.
func writeBytesIndividualGETWithRevision(c *gin.Context, b []byte, revision string) {
	serveIndividualGET(c, b, strongETagFromJSONBytesAndRevision(b, revision))
}

func serveIndividualGET(c *gin.Context, b []byte, etag string) {
	c.Header("ETag", etag)
	if ifNoneMatchNotModified(c.Request, etag) {
		c.Status(http.StatusNotModified)
		// Gin defers flushing status until first write; 304 has no body, so flush explicitly so clients
		// and httptest see the correct status code.
		c.Writer.WriteHeaderNow()
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", b)
}

func strongETagFromJSONBytes(b []byte) string {
	sum := sha256.Sum256(b)
	return `"` + hex.EncodeToString(sum[:]) + `"`
}

// strongETagFromJSONBytesAndRevision is the quoted hex SHA-256 of b, a NUL byte, and revision. NUL is a
// safe domain separator because JSON text (and jsonb output) never contains a raw 0x00, so no two
// (b, revision) pairs share a preimage. Deliberately not collapsed to strongETagFromJSONBytes when
// revision is empty: the tag of a resource that carries a revision must never equal the tag the same
// bytes would have without one.
func strongETagFromJSONBytesAndRevision(b []byte, revision string) string {
	h := sha256.New()
	h.Write(b)
	h.Write([]byte{0})
	h.Write([]byte(revision))
	return `"` + hex.EncodeToString(h.Sum(nil)) + `"`
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
