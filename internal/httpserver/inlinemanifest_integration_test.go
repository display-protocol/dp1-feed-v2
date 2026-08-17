//go:build integration

package httpserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/display-protocol/dp1-go/extension/identity"
	"github.com/display-protocol/dp1-go/playlist"
	dp1sign "github.com/display-protocol/dp1-go/sign"
	"github.com/google/uuid"

	"github.com/display-protocol/dp1-feed-v2/internal/fetcher"
)

// inlineManifestFixture is a DP-1 Ref Manifest carried on an item (playlists extension §3.6).
// The thumbnail omits w/h (optional since dp1-go v0.6.0) and the artist carries a
// present-but-empty id: both are members a decode/re-encode round trip would quietly drop, and
// the manifest is covered by the playlist signature with no refHash counterpart.
const inlineManifestFixture = `{"refVersion":"1.0.0","id":"manifest-1","created":"2026-08-01T00:00:00Z","locale":"en","metadata":{"title":"Work","artists":[{"id":"","name":"Artist"}],"thumbnails":{"small":{"uri":"https://cdn.example.com/thumb.png"}}},"controls":{"display":{"scaling":"fit"}}}`

// TestIntegration_InlineManifestHTTPRoundTrip walks an inlineManifest through every surface that
// re-encodes a playlist — create, read, list, item index, and a PATCH that re-signs the stored
// document — and checks the signature still verifies at each stop.
func TestIntegration_InlineManifestHTTPRoundTrip(t *testing.T) {
	srv := newIntegrationServer(t)

	playlistID := uuid.MustParse("bbbbbbbb-2222-4333-8444-555555555555")
	itemID := uuid.MustParse("44444444-2222-4333-8444-555555555555")
	slug := "inline-manifest-round-trip"

	postBody := map[string]any{
		"dpVersion": "1.1.0",
		"id":        playlistID.String(),
		"created":   "2020-01-02T03:04:05Z",
		"slug":      slug,
		"title":     "Inline manifest round trip",
		"items": []map[string]any{
			{
				"id":             itemID.String(),
				"source":         "https://cdn.example.com/day-1.html",
				"inlineManifest": json.RawMessage(inlineManifestFixture),
			},
		},
	}

	createdRaw := mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", postBody, http.StatusCreated)
	assertPlaylistInlineManifest(t, "POST response", createdRaw, itemID)

	gotRaw := mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists/"+slug, nil, http.StatusOK)
	assertPlaylistInlineManifest(t, "GET playlist", gotRaw, itemID)

	var page ListResponse[playlist.Playlist]
	mustDoJSON(t, srv, http.MethodGet, "/api/v1/playlists", nil, http.StatusOK, &page)
	if len(page.Items) != 1 {
		t.Fatalf("playlist list: len=%d want 1", len(page.Items))
	}
	assertItemInlineManifest(t, "playlist list", page.Items[0].Items, itemID)

	// The item index is a separate copy of items[], written when the playlist body changes.
	var itemPage ListResponse[playlist.PlaylistItem]
	mustDoJSON(t, srv, http.MethodGet, "/api/v1/playlist-items", nil, http.StatusOK, &itemPage)
	assertItemInlineManifest(t, "playlist item list", itemPage.Items, itemID)

	var one playlist.PlaylistItem
	mustDoJSON(t, srv, http.MethodGet, "/api/v1/playlist-items/"+itemID.String(), nil, http.StatusOK, &one)
	assertItemInlineManifest(t, "GET playlist item", []playlist.PlaylistItem{one}, itemID)

	// PATCH rebuilds and re-signs the document from the stored one without resending items[].
	patchedRaw := mustDoRaw(t, srv, http.MethodPatch, "/api/v1/playlists/"+slug, map[string]any{"title": "Inline manifest patched"}, http.StatusOK)
	assertPlaylistInlineManifest(t, "PATCH response", patchedRaw, itemID)
}

// TestIntegration_InlineManifestRejectedWhenMalformed covers the validation the extension overlay
// adds: inlineManifest is checked against the unmodified ref-manifest schema, so a manifest the
// feed could not serve is refused at the API rather than stored.
func TestIntegration_InlineManifestRejectedWhenMalformed(t *testing.T) {
	srv := newIntegrationServer(t)

	postBody := map[string]any{
		"dpVersion": "1.1.0",
		"slug":      "inline-manifest-malformed",
		"title":     "Inline manifest malformed",
		"items": []map[string]any{
			{
				"source": "https://cdn.example.com/day-1.html",
				// refVersion is required by the ref-manifest schema.
				"inlineManifest": json.RawMessage(`{"id":"m","created":"2026-08-01T00:00:00Z","locale":"en"}`),
			},
		},
	}
	mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", postBody, http.StatusBadRequest)
}

// TestIntegration_InlineManifestCuratorSignedPublish covers the publishing path a curator tool
// uses: the client signs the document itself and POSTs it without an API key, and the feed
// re-builds that document from the request before checking the signature over it. Any item field
// the request model cannot carry is dropped in that rebuild and the curator signature fails — the
// exact failure inlineManifest would have hit before dp1-go v0.6.0. Here it must publish and keep
// both the curator signature and the feed's counter-signature.
func TestIntegration_InlineManifestCuratorSignedPublish(t *testing.T) {
	srv := newIntegrationServer(t)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	curatorKid, err := dp1sign.Ed25519DIDKey(pub)
	if err != nil {
		t.Fatal(err)
	}

	playlistID := uuid.MustParse("cccccccc-2222-4333-8444-555555555555")
	itemID := uuid.MustParse("55555555-2222-4333-8444-555555555555")
	slug := "inline-manifest-curator-published"
	created := "2020-01-02T03:04:05Z"

	// The curator signs exactly what the feed will rebuild from the request: same fields, same
	// item ids, same "created". This mirrors what a publishing tool must construct.
	doc := playlist.Playlist{
		DPVersion: "1.1.0",
		ID:        playlistID.String(),
		Slug:      slug,
		Title:     "Inline manifest curator published",
		Created:   created,
		Curators:  []identity.Entity{{Name: "Curator", Key: curatorKid}},
		Items: []playlist.PlaylistItem{
			{
				ID:             itemID.String(),
				Source:         "https://cdn.example.com/day-1.html",
				InlineManifest: json.RawMessage(inlineManifestFixture),
			},
		},
	}
	unsigned, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	curatorSig, err := dp1sign.SignMultiEd25519(unsigned, priv, playlist.RoleCurator, created)
	if err != nil {
		t.Fatal(err)
	}

	postBody := map[string]any{
		"dpVersion":  doc.DPVersion,
		"id":         doc.ID,
		"created":    doc.Created,
		"slug":       doc.Slug,
		"title":      doc.Title,
		"curators":   doc.Curators,
		"items":      doc.Items,
		"signatures": []playlist.Signature{curatorSig},
	}

	createdRaw := mustDoRawUnauthenticated(t, srv, http.MethodPost, "/api/v1/playlists", postBody, http.StatusCreated)
	assertPlaylistInlineManifest(t, "curator-signed POST response", createdRaw, itemID)

	var pl playlist.Playlist
	if err := json.Unmarshal(createdRaw, &pl); err != nil {
		t.Fatal(err)
	}
	var haveCurator, haveFeed bool
	for _, s := range pl.Signatures {
		switch {
		case s.Kid == curatorKid:
			haveCurator = true
		case s.Role == playlist.RoleFeed:
			haveFeed = true
		}
	}
	if !haveCurator || !haveFeed {
		t.Fatalf("signatures: curator=%v feed=%v (%+v)", haveCurator, haveFeed, pl.Signatures)
	}

	// The curator signature must also survive storage, not just the create response.
	gotRaw := mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists/"+slug, nil, http.StatusOK)
	assertPlaylistInlineManifest(t, "curator-signed GET playlist", gotRaw, itemID)
}

// TestIntegration_InlineManifestSurvivesLocalGroupIngest covers the local branch of group ingest:
// the URI points at this feed, so resolveOnePlaylistRef loads the stored document and
// upsertPlaylistsBatch writes it back over itself. Only playlists.body is rewritten here — the
// item index is left as CreatePlaylist built it, and is correct precisely because the body did not
// change. The remote branch, where the document is new to this feed, is
// TestIntegration_InlineManifestSurvivesRemoteGroupIngest.
func TestIntegration_InlineManifestSurvivesLocalGroupIngest(t *testing.T) {
	srv := newIntegrationServer(t)

	itemID := uuid.MustParse("66666666-2222-4333-8444-555555555555")
	slug := "inline-manifest-ingested"

	postBody := map[string]any{
		"dpVersion": "1.1.0",
		"slug":      slug,
		"title":     "Inline manifest ingested",
		"items": []map[string]any{
			{
				"id":             itemID.String(),
				"source":         "https://cdn.example.com/day-1.html",
				"inlineManifest": json.RawMessage(inlineManifestFixture),
			},
		},
	}
	mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", postBody, http.StatusCreated)

	// publicBaseURL of the integration server, so the executor resolves this from the DB rather
	// than over HTTP — the local-ingest branch of resolveOnePlaylistRef.
	groupBody := map[string]any{
		"dpVersion": "1.1.0",
		"title":     "Group over an inline-manifest playlist",
		"curator":   "Curator",
		"playlists": []string{"http://example.com/api/v1/playlists/" + slug},
	}
	mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlist-groups", groupBody, http.StatusCreated)

	gotRaw := mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists/"+slug, nil, http.StatusOK)
	assertPlaylistInlineManifest(t, "playlist after group ingest", gotRaw, itemID)

	var itemPage ListResponse[playlist.PlaylistItem]
	mustDoJSON(t, srv, http.MethodGet, "/api/v1/playlist-items", nil, http.StatusOK, &itemPage)
	assertItemInlineManifest(t, "item index after group ingest", itemPage.Items, itemID)
}

// TestIntegration_InlineManifestSurvivesRemoteGroupIngest covers the branch that matters for a
// third party's document: the URI is outside publicBaseURL, so the feed fetches it, re-validates
// it, and stores a playlist it did not author. That store step re-marshals the document, and the
// inline manifest is covered by the remote curator's signature with no refHash counterpart — so a
// member dropped in transit would invalidate a signature this feed cannot re-create.
//
// Scope note: the item index is deliberately not asserted here. upsertPlaylistsBatch rewrites
// playlists.body without rebuilding playlist_item_index, so a remotely ingested playlist has no
// index rows at all — its items are missing from GET /playlist-items entirely, manifest or not.
// That is a pre-existing store defect unrelated to inline manifests, tracked in #13; asserting
// today's behaviour here would only encode it.
func TestIntegration_InlineManifestSurvivesRemoteGroupIngest(t *testing.T) {
	remoteID := uuid.MustParse("77777777-2222-4333-8444-555555555555")
	itemID := uuid.MustParse("88888888-2222-4333-8444-555555555555")

	signer := newIntegrationSigner(t)
	remote := playlist.Playlist{
		DPVersion: "1.1.0",
		ID:        remoteID.String(),
		Slug:      "remote-inline-manifest",
		Title:     "Remote inline manifest",
		Created:   "2020-01-02T03:04:05Z",
		Items: []playlist.PlaylistItem{{
			ID:             itemID.String(),
			Source:         "https://cdn.example.com/remote.html",
			InlineManifest: json.RawMessage(inlineManifestFixture),
		}},
	}
	unsigned, err := json.Marshal(remote)
	if err != nil {
		t.Fatal(err)
	}
	signedRemote, err := signer.SignPlaylist(unsigned, time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(signedRemote)
	}))
	defer origin.Close()

	srv := newIntegrationServerWithFetcher(t, fetcher.NewHTTPFetcher(10*time.Second, 4<<20))

	groupBody := map[string]any{
		"dpVersion": "1.1.0",
		"title":     "Group over a remote inline-manifest playlist",
		"curator":   "Curator",
		"playlists": []string{origin.URL + "/remote.json"},
	}
	mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlist-groups", groupBody, http.StatusCreated)

	gotRaw := mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists/"+remote.Slug, nil, http.StatusOK)
	assertPlaylistInlineManifest(t, "remotely ingested playlist", gotRaw, itemID)
}

// assertPlaylistInlineManifest checks the manifest bytes on the served document and, because the
// same bytes are part of the signed payload, that every signature on that document still verifies.
func assertPlaylistInlineManifest(t *testing.T, label string, raw []byte, itemID uuid.UUID) {
	t.Helper()
	var pl playlist.Playlist
	if err := json.Unmarshal(raw, &pl); err != nil {
		t.Fatalf("%s: decode playlist: %v body=%s", label, err, raw)
	}
	assertItemInlineManifest(t, label, pl.Items, itemID)

	ok, failed, err := dp1sign.VerifyPlaylistSignatures(raw)
	if err != nil {
		t.Fatalf("%s: verify signatures: %v", label, err)
	}
	if !ok {
		t.Fatalf("%s: signature verification failed: %+v", label, failed)
	}
}

func assertItemInlineManifest(t *testing.T, label string, items []playlist.PlaylistItem, itemID uuid.UUID) {
	t.Helper()
	if len(items) != 1 {
		t.Fatalf("%s: item len=%d want 1", label, len(items))
	}
	item := items[0]
	if item.ID != itemID.String() {
		t.Fatalf("%s: item id=%q want %q", label, item.ID, itemID.String())
	}
	if got, want := canonicalJSON(t, item.InlineManifest), canonicalJSON(t, []byte(inlineManifestFixture)); got != want {
		t.Fatalf("%s: inlineManifest=%s want %s", label, got, want)
	}
}

// canonicalJSON re-encodes through map[string]any, which Go marshals with sorted keys, so
// comparisons ignore key order. Byte equality is the wrong assertion end to end: signing
// re-encodes the whole document through a map (see dp1svc.Service.SignPlaylist) and Postgres
// stores it as JSONB, so member order is normalized on the way through. Neither loses content,
// and JCS canonicalizes key order before hashing, so the signature is unaffected — what must
// survive is every member, including present-but-empty ones.
func canonicalJSON(t *testing.T, raw []byte) string {
	t.Helper()
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		t.Fatalf("canonical JSON: %v raw=%s", err, raw)
	}
	out, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("canonical JSON: %v", err)
	}
	return string(out)
}
