package httpserver

import (
	"reflect"
	"strings"
	"testing"

	"github.com/display-protocol/dp1-feed-v2/internal/models"
)

// Strict decoding is responsible for the members this repository declares, and stops there.
//
// The boundary matters in both directions and had drifted in both. An unknown member on one of our own
// envelopes must be a 400 — that is what the documented contract promises and what stops a client
// believing it sent a field the feed ignored. An unknown member nested inside a dp1-go type must not be,
// because OpenAPI publishes those sub-objects permissively and deliberately does not restate schemas this
// repository does not own; rejecting them made the server stricter than its own published contract, so a
// generated client could build a body the feed refused. dp1-go validates that content on the next step.
func TestCheckExactMembers_strictnessStopsAtOurOwnTypes(t *testing.T) {
	t.Parallel()

	for _, tc := range []struct {
		name      string
		body      string
		wantField string // non-empty = must be rejected, naming this member
	}{
		{
			name:      "unknown member on our own envelope is rejected",
			body:      `{"dpVersion":"1.0.0","title":"t","items":[],"nope":1}`,
			wantField: "nope",
		},
		{
			name:      "misspelled member on our own envelope is rejected",
			body:      `{"dpVersion":"1.0.0","title":"t","items":[],"coverimage":"x"}`,
			wantField: "coverimage",
		},
		{
			// Governed by dp1-go's schema, which validates it immediately after decoding. The document is
			// stored verbatim either way, so the only question is which layer judges the member.
			name: "unknown member inside a dp1-go sub-object is left to dp1-go",
			body: `{"dpVersion":"1.0.0","title":"t","items":[],"defaults":{"license":"open","nope":1}}`,
		},
		{
			name: "unknown member inside a dp1-go slice element is left to dp1-go",
			body: `{"dpVersion":"1.0.0","title":"t","items":[{"source":"https://a.test/x","nope":1}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			var req models.PlaylistCreateRequest
			err := checkExactMembers([]byte(tc.body), reflect.TypeOf(req))
			if tc.wantField == "" {
				if err != nil {
					t.Fatalf("nested dp1-go members are not ours to reject, got %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("an unknown member on our own type must be rejected")
			}
			if !strings.Contains(err.Error(), tc.wantField) {
				t.Fatalf("error should name the offending member %q, got %v", tc.wantField, err)
			}
		})
	}
}
