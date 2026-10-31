package httpserver

import (
	"encoding/json"
	"runtime"
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
			// Content Rating Extension members, declared by dp1-go's PlaylistItem since v0.6.2. That struct
			// decodes through its own UnmarshalJSON, which DisallowUnknownFields cannot see into, so
			// checkExactMembers is what still rejects a stray member inside an item; this case pins that
			// the two known members pass it.
			name: "content-rating members on a playlist item are known",
			body: `{"dpVersion":"1.0.0","title":"t","items":[{"source":"https://a.test/x","contentRating":"mature","contentReasons":["nudity"]}]}`,
		},
		{
			// Playlists extension §3.3 note `title` and `author`, declared by dp1-go's Note since v0.7.2. The
			// same Note type decodes the playlist note and an item's, and author is a full identity Entity.
			// Before the bump both were unknown members and the write was a 400 naming them.
			name: "note title and author on the playlist and on an item are known",
			body: `{"dpVersion":"1.0.0","title":"t","note":{"text":"x","title":"Why","author":{"name":"A","role":"curator"}},"items":[{"source":"https://a.test/x","note":{"text":"y","title":"On this work","author":{"name":"B"}}}]}`,
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

// seriesIDDocument wraps a provenance seriesId literal in the smallest document decodeDocument
// will walk into. The literal sits three levels down (items[].provenance.contract.seriesId),
// which is the point: nothing in this feed reads seriesId, so the only way it is ever parsed is
// as a side effect of binding the request, and the only guard against a hostile literal is the
// SDK's own.
func seriesIDDocument(literal string) string {
	return `{"dpVersion":"1.0.0","title":"t","items":[{"source":"https://a.test/x",` +
		`"provenance":{"type":"onChain","contract":{"seriesId":` + literal + `}}}]}`
}

// A seriesId literal must not buy more work than its own length.
//
// Why this test lives in the feed and not only in dp1-go: decodeDocument runs before any
// signature is verified (handlers.go binds, then Exec verifies), and the signature-presence
// middleware lets an unauthenticated request reach it. So every allocation the SDK's
// SeriesID.UnmarshalJSON makes is allocation an anonymous caller can command, bounded by
// nothing but MaxRequestBytes — and a 146-byte body is nowhere near that limit. dp1-go v0.7.0,
// which this branch originally bumped to, turned `1e-1000000` into roughly 2.9 MB: the digit
// bound clamped a negative exponent to zero as "it only shrinks the value", true of the value
// but not of the work, since big.Rat holds `m e -k` as a fraction over 10^k and materializes
// that k-digit denominator before IsInt can refuse the literal as fractional. v0.7.1 reads the
// exponent's magnitude and ignores its sign, so the literal is refused on sight.
//
// The assertion is on bytes allocated rather than on the error alone because both versions
// reject the document — v0.7.0 just pays millions of bytes to do it. An error-only test would
// have passed against the defect.
//
// Not parallel: TotalAlloc is process-wide, so a concurrent test's allocations would land in
// the measurement. The 1 MiB bound is deliberately loose against the ~2.9 MB regression and the
// near-zero cost of the fix, so background runtime allocation cannot tip it either way.
func TestDecodeDocument_seriesIDCannotAmplifyAllocation(t *testing.T) {
	const hostile = "1e-1000000"
	body := []byte(seriesIDDocument(hostile))

	var before, after runtime.MemStats
	runtime.ReadMemStats(&before)
	var req models.PlaylistCreateRequest
	err := decodeDocument(body, &req)
	runtime.ReadMemStats(&after)

	if err == nil {
		t.Fatalf("seriesId %s is fractional and must be rejected, not decoded", hostile)
	}
	if !strings.Contains(err.Error(), "seriesId") {
		t.Fatalf("error should name seriesId so the caller can fix the document, got %v", err)
	}

	const bound = 1 << 20
	if allocated := after.TotalAlloc - before.TotalAlloc; allocated > bound {
		t.Fatalf("decoding a %d-byte body allocated %d bytes, over the %d-byte bound: "+
			"the negative-exponent seriesId guard has regressed", len(body), allocated, bound)
	}
}

// The bound above must refuse amplification without refusing the identifiers SeriesID exists to
// carry. A guard that rejected these would be the other half of the same defect: the corpus this
// branch was built for publishes series identifiers too large for a JSON number to hold exactly.
func TestDecodeDocument_acceptsLegitimateSeriesIDSpellings(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct{ name, literal string }{
		{name: "plain integer", literal: "42"},
		{name: "negative exponent the mantissa absorbs", literal: "1500e-2"},
		{name: "positive exponent", literal: "5e2"},
		{name: "256-bit identifier as a decimal string", literal: `"115792089237316195423570985008687907853269984665640564039457584007913129639935"`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var req models.PlaylistCreateRequest
			if err := decodeDocument([]byte(seriesIDDocument(tc.literal)), &req); err != nil {
				t.Fatalf("seriesId %s is a legitimate identifier and must decode, got %v", tc.literal, err)
			}
		})
	}
}
