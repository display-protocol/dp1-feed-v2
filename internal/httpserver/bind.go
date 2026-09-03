package httpserver

// Strict request decoding and verbatim document responses for the document endpoints.

import (
	"encoding/json"
	"errors"
	"net/http"

	"github.com/gin-gonic/gin"
	"github.com/gin-gonic/gin/binding"
)

// Request bodies are decoded strictly: a JSON member that no request field models is a 400, never
// silently dropped. On the signed-document path a dropped member changes the bytes the client signed
// and orphans its signature (DP-1 §7.1 signs the entire document); on the API-key path it is data the
// client sent and the feed would discard without telling anyone. Either way: reject, and name the field.
//
// This is a process-wide gin setting, so it also applies to the registry PUT — intentionally.
//
// Coupling to be aware of: "unknown" means unknown to the request models in internal/models and the
// dp1-go structs they embed, not to the DP-1 JSON Schema (whose core leaves additionalProperties open).
// The feed is therefore stricter than the spec, by decision. The request models must stay a superset of
// the dp1-go document structs, or a dp1-go upgrade that adds a schema member would turn into 400s for
// every signed client; internal/models/coverage_test.go pins that superset.
func init() {
	binding.EnableDecoderDisallowUnknownFields = true
}

// errTrailingBody is returned when the body holds more than one JSON value; the decoder would stop
// after the first and the signer would later fail on the remainder as an internal error.
var errTrailingBody = errors.New("request body must be exactly one JSON document")

// bindDocument decodes the body into dst (strict decode + `binding:"required"` validation) and returns
// the exact bytes received. The executor uses those bytes verbatim on the signed-document path and
// only reads dst; it never rebuilds a signed document from the decoded fields.
func bindDocument(c *gin.Context, dst any) (json.RawMessage, error) {
	raw, err := c.GetRawData()
	if err != nil {
		return nil, err
	}
	if !json.Valid(raw) {
		return nil, errTrailingBody
	}
	if err := binding.JSON.BindBody(raw, dst); err != nil {
		return nil, err
	}
	return raw, nil
}

// writeDocument writes stored document bytes to the wire unchanged. Re-encoding through the typed
// structs would drop present-but-empty members and re-type numbers, so the response would no longer
// verify against its own signatures (see store.PlaylistRecord).
func writeDocument(c *gin.Context, status int, raw json.RawMessage) {
	c.Data(status, "application/json; charset=utf-8", raw)
}

// documents projects a page of records onto their stored bytes for list envelopes.
func documents[T any](recs []T, raw func(*T) json.RawMessage) []json.RawMessage {
	out := make([]json.RawMessage, 0, len(recs))
	for i := range recs {
		out = append(out, raw(&recs[i]))
	}
	return out
}

// created is the shared 201 response for the three document POST handlers.
func created(c *gin.Context, raw json.RawMessage) {
	writeDocument(c, http.StatusCreated, raw)
}
