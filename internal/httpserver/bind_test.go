package httpserver

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/display-protocol/dp1-feed-v2/internal/models"
)

// Strict decoding is recursive, and these cases go through decodeDocument rather than checkExactMembers.
//
// That distinction is the point. decodeDocument runs two mechanisms: checkExactMembers, which catches
// case-variant spellings encoding/json would otherwise bind, and gin's binding with the process-wide
// EnableDecoderDisallowUnknownFields, which rejects unknown members at every depth. Exercising only the
// first gives a false reading of what a request does — a nested member can look accepted there while the
// real path rejects it. Anything asserting this boundary has to run the whole decode.
func TestDecodeDocument_rejectsUnknownMembersAtEveryDepth(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		body      string
		wantField string // "" = must decode cleanly
	}{
		{
			name:      "unknown member at the top level",
			body:      `{"dpVersion":"1.0.0","title":"t","items":[],"nope":1}`,
			wantField: "nope",
		},
		{
			// encoding/json binds member names case-insensitively even with DisallowUnknownFields, so this
			// one is caught by checkExactMembers rather than by gin.
			name:      "case-variant spelling of a known member",
			body:      `{"dpVersion":"1.0.0","title":"t","items":[],"coverImage":"a","CoverImage":"b"}`,
			wantField: "CoverImage",
		},
		{
			// The ff-cli#107 shape: a document is signed over its exact bytes, so a member dropped here
			// would surface much later as a signature failure with no obvious cause.
			name:      "unknown member inside a nested DP-1 object",
			body:      `{"dpVersion":"1.0.0","title":"t","items":[],"defaults":{"license":"open","nope":1}}`,
			wantField: "nope",
		},
		{
			name:      "unknown member inside a playlist item",
			body:      `{"dpVersion":"1.0.0","title":"t","items":[{"source":"https://a.test/x","created":"2026-01-01T00:00:00Z"}]}`,
			wantField: "created",
		},
		{
			// Raw JSON values are the documented exception: nothing inspects them here, so arbitrary
			// members inside one must survive decoding untouched.
			name: "arbitrary members inside an opaque raw value are permitted",
			body: `{"dpVersion":"1.0.0","title":"t","items":[{"source":"https://a.test/x","inlineManifest":{"anything":{"nested":true}}}]}`,
		},
		{
			name: "a well-formed document with no unknown members",
			body: `{"dpVersion":"1.0.0","title":"t","items":[{"source":"https://a.test/x"}],"defaults":{"license":"open"}}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var req models.PlaylistCreateRequest
			err := decodeDocument([]byte(tc.body), &req)
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("body should decode cleanly, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("unknown member %q must be rejected rather than dropped", tc.wantField)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Fatalf("error should name the offending member %q, got %v", tc.wantField, err)
			}
		})
	}
}

// The PUT envelope carries two independently signed halves, so the boundary has to hold inside each. A
// member dropped from either would change bytes that some signature covers.
func TestDecodeSignedReplace_rejectsUnknownMembersInBothHalves(t *testing.T) {
	t.Parallel()

	const validDoc = `{"dpVersion":"1.0.0","title":"t","items":[{"source":"https://a.test/x"}]}`
	const validIntent = `{"action":"replace","target":{"type":"playlist","id":"x","slug":"y"},` +
		`"payloadHash":"h","created":"2026-01-01T00:00:00Z","signatures":[]}`

	for _, tc := range []struct {
		name, body, wantField string
	}{
		{
			name:      "unknown member on the envelope itself",
			body:      `{"document":` + validDoc + `,"authorization":` + validIntent + `,"nope":1}`,
			wantField: "nope",
		},
		{
			name:      "unknown member in the document half",
			body:      `{"document":{"dpVersion":"1.0.0","title":"t","items":[],"nope":1},"authorization":` + validIntent + `}`,
			wantField: "nope",
		},
		{
			name:      "unknown member nested in the document half",
			body:      `{"document":{"dpVersion":"1.0.0","title":"t","items":[{"source":"https://a.test/x","nope":1}]},"authorization":` + validIntent + `}`,
			wantField: "nope",
		},
		{
			name:      "unknown member in the authorization half",
			body:      `{"document":` + validDoc + `,"authorization":{"action":"replace","nope":1}}`,
			wantField: "nope",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			// The envelope keeps both halves as raw bytes so signatures stay intact, so each half is
			// decoded in its own right — the same sequence bindSignedReplace performs.
			var wrapper models.SignedReplaceRequest
			err := decodeDocument([]byte(tc.body), &wrapper)
			if err == nil {
				var doc models.PlaylistCreateRequest
				if err = decodeDocument(wrapper.Document, &doc); err == nil {
					var intent models.SignedIntent
					err = decodeDocument(wrapper.Authorization, &intent)
				}
			}
			if err == nil {
				t.Fatalf("unknown member %q must be rejected somewhere in the replace envelope", tc.wantField)
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Fatalf("error should name the offending member %q, got %v", tc.wantField, err)
			}
		})
	}
}

// The request models are the effective schema for signed submissions, so a document using only members
// dp1-go describes must decode. This is the other half of internal/models/coverage_test.go: that test
// proves the models are a superset of the document structs, this one proves the decoder honors it.
func TestDecodeDocument_acceptsEveryModelledMember(t *testing.T) {
	t.Parallel()

	raw, err := json.Marshal(map[string]any{
		"dpVersion": "1.0.0",
		"title":     "t",
		"items":     []any{map[string]any{"source": "https://a.test/x"}},
		"defaults":  map[string]any{"license": "open", "display": map[string]any{"scaling": "fit"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	var req models.PlaylistCreateRequest
	if err := decodeDocument(raw, &req); err != nil {
		t.Fatalf("a document using only modeled members must decode, got %v", err)
	}
}
