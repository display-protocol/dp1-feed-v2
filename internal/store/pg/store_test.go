package pg

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// A document may repeat a reference, and resolution shares one resolved value across every position it
// occupies — those positions alias a single Raw slice, so the resolved set stays inside
// max_resolved_bytes. Building insert parameters per position undid that, because string(p.Raw)
// allocates: a group naming one large playlist 1000 times passed the resolution budget and then produced
// ~1000 distinct copies here (and again on the wire) before the CTE's DISTINCT ON could discard them.
//
// The assertion is on the number of bodies built, which is what carries the bytes.
func TestUniquePlaylistParams_repeatedReferenceBuildsOneBody(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	raw := json.RawMessage(`{"id":"22222222-2222-2222-2222-222222222222","slug":"pl","pad":"` + strings.Repeat("x", 4096) + `"}`)

	const positions = 1000
	playlists := make([]store.IngestedPlaylist, 0, positions)
	for range positions {
		// Same value at every position, exactly as resolvePlaylistURIs produces for a repeated URI.
		playlists = append(playlists, store.IngestedPlaylist{ID: id, Slug: "pl", Raw: raw, SourceURI: "https://a.test/p.json"})
	}

	ids, slugs, bodies, err := uniquePlaylistParams(playlists)
	if err != nil {
		t.Fatalf("uniquePlaylistParams: %v", err)
	}
	if len(ids) != 1 || len(slugs) != 1 || len(bodies) != 1 {
		t.Fatalf("a repeated reference must build one row, got ids=%d slugs=%d bodies=%d", len(ids), len(slugs), len(bodies))
	}
	if ids[0] != id || slugs[0] != "pl" || bodies[0] != string(raw) {
		t.Fatalf("wrong row built: id=%s slug=%q body=%d bytes", ids[0], slugs[0], len(bodies[0]))
	}
}

// Deduplication must not reorder or drop distinct playlists: the first occurrence of each id wins and
// relative order is preserved, since membership positions are written from the full slice separately.
func TestUniquePlaylistParams_keepsFirstOccurrenceInOrder(t *testing.T) {
	t.Parallel()

	a := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	b := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	mk := func(id uuid.UUID, slug, body string) store.IngestedPlaylist {
		return store.IngestedPlaylist{ID: id, Slug: slug, Raw: json.RawMessage(body)}
	}
	playlists := []store.IngestedPlaylist{
		mk(a, "a-first", `{"n":1}`),
		mk(b, "b", `{"n":2}`),
		mk(a, "a-second", `{"n":3}`), // duplicate id, different payload: first wins
	}

	ids, slugs, bodies, err := uniquePlaylistParams(playlists)
	if err != nil {
		t.Fatalf("uniquePlaylistParams: %v", err)
	}
	if len(ids) != 2 || ids[0] != a || ids[1] != b {
		t.Fatalf("want [a b] in first-seen order, got %v", ids)
	}
	if slugs[0] != "a-first" || bodies[0] != `{"n":1}` {
		t.Fatalf("first occurrence must win, got slug=%q body=%q", slugs[0], bodies[0])
	}
}

// An empty body is rejected before anything is sent, including when it is the duplicate occurrence.
func TestUniquePlaylistParams_rejectsEmptyBody(t *testing.T) {
	t.Parallel()

	id := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	playlists := []store.IngestedPlaylist{{ID: id, Slug: "pl", Raw: nil}}
	if _, _, _, err := uniquePlaylistParams(playlists); err == nil {
		t.Fatal("want an error for an empty ingested playlist body, got nil")
	}
}
