//go:build integration

package httpserver

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/display-protocol/dp1-go/playlist"
	dp1sign "github.com/display-protocol/dp1-go/sign"
	"github.com/google/uuid"

	"github.com/display-protocol/dp1-feed-v2/internal/fetcher"
)

// serveSignedPlaylist stands up an origin serving doc, curator-signed by priv, and returns its URL.
func serveSignedPlaylist(t *testing.T, priv []byte, doc playlist.Playlist) string {
	t.Helper()
	unsigned, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := dp1sign.SignMultiEd25519(unsigned, priv, playlist.RoleCurator, doc.Created)
	if err != nil {
		t.Fatal(err)
	}
	doc.Signatures = []playlist.Signature{sig}
	body, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	t.Cleanup(origin.Close)
	return origin.URL + "/playlist.json"
}

// TestIntegration_IngestCannotHijackAnotherOwnersPlaylist is the attack that reference-only ingestion
// closes. Creating a group is open, so before this change an attacker could host a document carrying a
// victim's playlist id and have the feed upsert it — overwriting the victim's body, slug, owner set and
// item index. Ingestion may now only *link* a playlist that already exists.
func TestIntegration_IngestCannotHijackAnotherOwnersPlaylist(t *testing.T) {
	srv := newIntegrationServerWithFetcher(t, fetcher.NewHTTPFetcher(10*time.Second, 4<<20))

	// Owner A publishes a playlist through the front door.
	apriv, akid := newCuratorKeypair(t)
	victimID := uuid.MustParse("6a000000-1111-4222-8333-444444444444")
	victimItem := uuid.MustParse("6b000000-1111-4222-8333-444444444444")
	victim := playlist.Playlist{
		DPVersion: "1.1.0",
		ID:        victimID.String(),
		Slug:      "owner-a-playlist",
		Title:     "Owner A's playlist",
		Created:   "2020-01-02T03:04:05Z",
		Curators:  curatorEntities(akid),
		Items: []playlist.PlaylistItem{{
			ID:     victimItem.String(),
			Source: "https://cdn.example.com/owner-a.html",
		}},
	}
	mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", signedPlaylistBody(t, apriv, victim), http.StatusCreated)
	// Baseline from a GET, not from the create response: the create response is the freshly signed bytes,
	// while a GET returns them after the jsonb round trip (which reorders keys — JCS-neutral, but not
	// byte-identical). Comparing GET to GET keeps this a strict byte comparison of what is stored.
	stored := mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists/"+victim.Slug, nil, http.StatusOK)

	// Attacker B hosts a different document reusing the victim's id, and references it from a group it
	// creates. The spoof is validly self-signed — by B's own key — so signature checks alone would pass it.
	bpriv, bkid := newCuratorKeypair(t)
	spoofItem := uuid.MustParse("6c000000-1111-4222-8333-444444444444")
	spoof := playlist.Playlist{
		DPVersion: "1.1.0",
		ID:        victimID.String(), // the victim's id
		Slug:      "hijacked",
		Title:     "Hijacked by B",
		Created:   "2020-01-02T03:04:05Z",
		Curators:  curatorEntities(bkid), // B claims ownership
		Items: []playlist.PlaylistItem{{
			ID:     spoofItem.String(),
			Source: "https://attacker.example/spoof.html",
		}},
	}
	spoofURL := serveSignedPlaylist(t, bpriv, spoof)

	groupBody := signedGroupBody(t, bpriv, bkid,
		uuid.MustParse("6d000000-1111-4222-8333-444444444444").String(),
		"attacker-group",
		"Attacker's group",
		"2020-01-02T03:04:05Z",
		[]string{spoofURL},
	)
	mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlist-groups", groupBody, http.StatusCreated)

	// The victim's playlist is byte-for-byte what it was: ingestion linked it, it did not rewrite it.
	after := mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists/"+victim.Slug, nil, http.StatusOK)
	if string(after) != string(stored) {
		t.Fatalf("ingest modified the victim's playlist:\n before=%s\n after =%s", stored, after)
	}

	// The spoof's slug never became a route, and its item never entered the index.
	doRaw(t, srv, http.MethodGet, "/api/v1/playlists/hijacked", nil, http.StatusNotFound)

	var itemPage ListResponse[playlist.PlaylistItem]
	mustDoJSON(t, srv, http.MethodGet, "/api/v1/playlist-items", nil, http.StatusOK, &itemPage)
	for _, it := range itemPage.Items {
		if it.ID == spoofItem.String() {
			t.Fatalf("spoof item entered the index: %+v", it)
		}
	}
	var sawVictimItem bool
	for _, it := range itemPage.Items {
		if it.ID == victimItem.String() {
			sawVictimItem = true
		}
	}
	if !sawVictimItem {
		t.Fatalf("victim item missing from index: %+v", itemPage.Items)
	}
}

// A remote playlist the feed does not already hold is created by the ingest, so it must clear the same
// bar as POST — an unsigned document must not be published here on a referencing party's say-so.
func TestIntegration_IngestRejectsUnsignedRemotePlaylist(t *testing.T) {
	srv := newIntegrationServerWithFetcher(t, fetcher.NewHTTPFetcher(10*time.Second, 4<<20))

	unsigned := playlist.Playlist{
		DPVersion: "1.1.0",
		ID:        uuid.MustParse("6e000000-1111-4222-8333-444444444444").String(),
		Slug:      "unsigned-remote",
		Title:     "Unsigned remote",
		Created:   "2020-01-02T03:04:05Z",
		Items: []playlist.PlaylistItem{{
			ID:     uuid.MustParse("6f000000-1111-4222-8333-444444444444").String(),
			Source: "https://cdn.example.com/unsigned.html",
		}},
	}
	body, err := json.Marshal(unsigned)
	if err != nil {
		t.Fatal(err)
	}
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
	defer origin.Close()

	gpriv, gkid := newCuratorKeypair(t)
	groupBody := signedGroupBody(t, gpriv, gkid,
		uuid.MustParse("70000000-1111-4222-8333-444444444444").String(),
		"unsigned-remote-group",
		"Group over an unsigned remote playlist",
		"2020-01-02T03:04:05Z",
		[]string{origin.URL + "/playlist.json"},
	)
	doRaw(t, srv, http.MethodPost, "/api/v1/playlist-groups", groupBody, http.StatusBadRequest)

	// Nothing was published as a side effect of the rejected request.
	doRaw(t, srv, http.MethodGet, "/api/v1/playlists/"+unsigned.Slug, nil, http.StatusNotFound)
}
