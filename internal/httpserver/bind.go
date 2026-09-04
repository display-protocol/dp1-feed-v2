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

	"github.com/display-protocol/dp1-feed-v2/internal/models"
)

// Request bodies are decoded strictly: a JSON member that no request field models is a 400, never
// silently dropped. Every write is signature-authorized, and a dropped member changes the bytes the
// client signed and orphans its signature (DP-1 §7.1 signs the entire document). So: reject, and name
// the field, rather than silently discarding data the client sent.
//
// This is a process-wide gin setting.
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
// the exact bytes received. The executor verifies, co-signs and stores those bytes verbatim and only
// reads dst (identity projections, owner checks); it never rebuilds the document from decoded fields.
func bindDocument(c *gin.Context, dst any) (json.RawMessage, error) {
	raw, err := c.GetRawData()
	if err != nil {
		return nil, err
	}
	if err := decodeDocument(raw, dst); err != nil {
		return nil, err
	}
	return raw, nil
}

// decodeDocument strictly decodes raw into dst without reading the request: an unknown or misspelled
// member, or more than one JSON value, is an error rather than a silent drop.
func decodeDocument(raw []byte, dst any) error {
	if !json.Valid(raw) {
		return errTrailingBody
	}
	// encoding/json matches member names case-insensitively even with DisallowUnknownFields, so
	// "Summary" would silently bind to summary. Check exact spellings first.
	if err := checkExactMembers(raw, reflect.TypeOf(dst)); err != nil {
		return err
	}
	return binding.JSON.BindBody(raw, dst)
}

// bindSignedReplace decodes a PUT body: the document to install plus the signed intent authorizing it.
//
// Both halves are kept as the exact bytes received. The document bytes are what the executor verifies,
// co-signs and stores verbatim; the authorization bytes are what the intent's own signature covers. A
// re-encode of either would change what was signed, so neither is ever rebuilt from decoded fields.
// Each half is decoded strictly in its own right, so an unknown member inside the document or the
// intent is still a 400 naming the field.
func bindSignedReplace(c *gin.Context, doc any) (json.RawMessage, *models.SignedIntent, error) {
	raw, err := c.GetRawData()
	if err != nil {
		return nil, nil, err
	}
	if !json.Valid(raw) {
		return nil, nil, errTrailingBody
	}
	var wrapper models.SignedReplaceRequest
	if err := checkExactMembers(raw, reflect.TypeOf(&wrapper)); err != nil {
		return nil, nil, err
	}
	if err := json.Unmarshal(raw, &wrapper); err != nil {
		return nil, nil, err
	}
	if len(wrapper.Document) == 0 {
		return nil, nil, errors.New(`request body must contain a "document" object`)
	}
	if len(wrapper.Authorization) == 0 {
		return nil, nil, errors.New(`request body must contain an "authorization" object (the signed mutation intent)`)
	}
	if err := decodeDocument(wrapper.Document, doc); err != nil {
		return nil, nil, err
	}
	var intent models.SignedIntent
	if err := decodeDocument(wrapper.Authorization, &intent); err != nil {
		return nil, nil, err
	}
	intent.Raw = wrapper.Authorization
	return wrapper.Document, &intent, nil
}

// localModulePrefix identifies types declared in this repository, which are the ones strict decoding is
// responsible for. Anything outside it is governed by its own published schema (see exactMembers).
const localModulePrefix = "github.com/display-protocol/dp1-feed-v2/"

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
		// Strictness stops at the boundary this API owns.
		//
		// The request envelopes and their top-level members are declared here, so a misspelled or unknown
		// member there is this API's to reject — that is what the documented 400 promises, and what stops a
		// client thinking it sent a field the feed silently ignored. Everything nested inside is a dp1-go
		// type whose member set belongs to the DP-1 schemas, which dp1-go itself validates on the very next
		// step.
		//
		// Recursing past this line was incidental rather than designed: it happened because the request
		// structs reuse dp1-go's types. It also made the published contract a lie in the other direction —
		// OpenAPI declares these sub-objects permissively and deliberately does not restate schemas this
		// repository does not own, so a generated client could construct a body the server rejected. And it
		// bought nothing: documents are stored verbatim, so an unknown nested member is preserved either
		// way; the only question is which layer judges it, and the answer is the schema that defines it.
		if !strings.HasPrefix(typ.PkgPath(), localModulePrefix) {
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
