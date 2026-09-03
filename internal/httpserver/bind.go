package httpserver

// Strict request decoding and verbatim document responses for the document endpoints.

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"

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
	// encoding/json matches member names case-insensitively even with DisallowUnknownFields, so
	// "Summary" would silently bind to summary. Check exact spellings first.
	if err := checkExactMembers(raw, reflect.TypeOf(dst)); err != nil {
		return nil, err
	}
	if err := binding.JSON.BindBody(raw, dst); err != nil {
		return nil, err
	}
	return raw, nil
}

// checkExactMembers walks the JSON in raw alongside the Go type it will decode into and rejects any
// object member whose name is not, byte for byte, a JSON tag of the struct at that position. It
// recurses through pointers, slices, and nested structs; maps, interfaces, and json.RawMessage fields
// are opaque (their contents are the client's, not modeled). The error text mirrors encoding/json's.
func checkExactMembers(raw []byte, typ reflect.Type) error {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return err
	}
	return exactMembers(v, typ)
}

var rawMessageType = reflect.TypeOf(json.RawMessage(nil))

func exactMembers(v any, typ reflect.Type) error {
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	switch typ.Kind() {
	case reflect.Struct:
		if typ == rawMessageType {
			return nil
		}
		obj, ok := v.(map[string]any)
		if !ok {
			return nil // a type mismatch is the decoder's error to report
		}
		fields := jsonFields(typ)
		for name, val := range obj {
			f, known := fields[name]
			if !known {
				return fmt.Errorf("json: unknown field %q", name)
			}
			if err := exactMembers(val, f); err != nil {
				return err
			}
		}
	case reflect.Slice, reflect.Array:
		if typ == rawMessageType {
			return nil
		}
		arr, ok := v.([]any)
		if !ok {
			return nil
		}
		for _, item := range arr {
			if err := exactMembers(item, typ.Elem()); err != nil {
				return err
			}
		}
	}
	return nil
}

// jsonFields maps a struct's wire names to field types, ignoring "-" fields and using the Go name
// when a field has no json tag (encoding/json's rule).
func jsonFields(typ reflect.Type) map[string]reflect.Type {
	out := make(map[string]reflect.Type, typ.NumField())
	for i := 0; i < typ.NumField(); i++ {
		f := typ.Field(i)
		if !f.IsExported() {
			continue
		}
		name, _, _ := strings.Cut(f.Tag.Get("json"), ",")
		switch name {
		case "-":
			continue
		case "":
			name = f.Name
		}
		out[name] = f.Type
	}
	return out
}

// writeDocument writes stored document bytes to the wire unchanged. Re-encoding through the typed
// structs would drop present-but-empty members and re-type numbers, so the response would no longer
// verify against its own signatures (see store.PlaylistRecord).
func writeDocument(c *gin.Context, status int, raw json.RawMessage) {
	c.Data(status, "application/json; charset=utf-8", raw)
}

// writeDocumentList writes a list envelope whose item documents are emitted byte-for-byte as stored.
// gin's c.JSON HTML-escapes `<`, `>`, and `&` inside json.RawMessage values, which would make list
// bodies diverge from the stored bytes and from single-resource GET — breaking the "served as stored"
// contract that lets every signature verify against the response carrying it. Encoding with HTML
// escaping disabled preserves the (already compact) jsonb bytes.
func writeDocumentList(c *gin.Context, docs []json.RawMessage, nextCursor string) {
	if docs == nil {
		docs = []json.RawMessage{}
	}
	env := ListResponse[json.RawMessage]{Items: docs, Cursor: nextCursor, HasMore: nextCursor != ""}
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(env); err != nil {
		writeError(c.Writer, http.StatusInternalServerError, "internal_error", "response encoding failed")
		return
	}
	c.Data(http.StatusOK, "application/json; charset=utf-8", bytes.TrimRight(buf.Bytes(), "\n"))
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
