//go:build integration

package httpserver

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/display-protocol/dp1-go/playlist"
	dp1sign "github.com/display-protocol/dp1-go/sign"
	"github.com/google/uuid"
)

// These tests pin the signed-document contract end to end (HTTP → executor → dp1svc → Postgres →
// HTTP): the feed verifies, co-signs, stores, and serves the client's bytes without editing them.
// Every assertion that matters is made by dp1-go's own verifier over the bytes the feed actually
// serves, so a regression to a rebuild-then-hash flow (feral-file/ff-cli#107) fails here, not in a
// partner's verifier.

// curatorSigned returns unsigned document bytes with a curator signature appended, produced the way a
// publishing tool does it: sign the JCS digest of the bytes, then add "signatures" to the same object.
// The signed bytes are built with map[string]json.RawMessage so every other member stays verbatim.
func curatorSigned(t *testing.T, unsigned []byte) (signed []byte, kid string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kid, err = dp1sign.Ed25519DIDKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := dp1sign.SignMultiEd25519(unsigned, priv, playlist.RoleCurator, "2020-01-02T03:04:05Z")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(unsigned, &doc); err != nil {
		t.Fatal(err)
	}
	sigs, err := json.Marshal([]playlist.Signature{sig})
	if err != nil {
		t.Fatal(err)
	}
	doc["signatures"] = sigs
	signed, err = json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return signed, kid
}

// mustVerifyAll fails unless every signature on raw verifies against raw itself.
func mustVerifyAll(t *testing.T, label string, raw []byte) []playlist.Signature {
	t.Helper()
	ok, failed, err := dp1sign.VerifyPlaylistSignatures(raw)
	if err != nil || !ok {
		t.Fatalf("%s: signatures do not verify over served bytes: ok=%v failed=%+v err=%v body=%s", label, ok, failed, err, raw)
	}
	var pl playlist.Playlist
	if err := json.Unmarshal(raw, &pl); err != nil {
		t.Fatal(err)
	}
	return pl.Signatures
}

// TestIntegration_SignedPlaylist_VerbatimRoundTrip: a document with every kind of member the old
// rebuild changed — a present-but-empty string (curators[].name, which the schema leaves
// unconstrained and the channels spec explicitly allows to be empty), a non-canonical RFC3339
// spelling, an integer above 2^53 — publishes, and both the curator and feed signatures verify over
// the POST response and over the GET response. That is the round-trip assertion from ff-cli#107.
func TestIntegration_SignedPlaylist_VerbatimRoundTrip(t *testing.T) {
	srv := newIntegrationServer(t)

	id := uuid.MustParse("dddddddd-2222-4333-8444-555555555555")
	const slug = "signed-verbatim"
	unsigned := []byte(`{"dpVersion":"1.1.0","id":"` + id.String() + `","slug":"` + slug + `",` +
		`"title":"Signed verbatim","created":"2020-01-02T03:04:05+00:00",` +
		`"curators":[{"name":"","key":"KID"}],` +
		`"items":[{"id":"a1a1a1a1-2222-4333-8444-555555555555","source":"https://cdn.example.com/a.html","duration":10,"override":{"seed":12345678901234567890}}]}`)
	// The curator key must appear in curators[] for the feed's curator check; splice it in before signing.
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kid, err := dp1sign.Ed25519DIDKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	unsigned = []byte(replaceOnce(string(unsigned), "KID", kid))
	sig, err := dp1sign.SignMultiEd25519(unsigned, priv, playlist.RoleCurator, "2020-01-02T03:04:05Z")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(unsigned, &doc); err != nil {
		t.Fatal(err)
	}
	doc["signatures"], _ = json.Marshal([]playlist.Signature{sig})
	signed, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	createdRaw := mustDoRawUnauthenticated(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(signed), http.StatusCreated)
	sigs := mustVerifyAll(t, "POST response", createdRaw)
	if len(sigs) != 2 || sigs[0].Kid != kid || sigs[1].Role != playlist.RoleFeed {
		t.Fatalf("POST signatures: want [curator, feed], got %+v", sigs)
	}
	// Both signers attest the same payload (DP-1 §7.1.1 example).
	if sigs[0].PayloadHash != sigs[1].PayloadHash {
		t.Fatalf("payload_hash differs: curator=%s feed=%s", sigs[0].PayloadHash, sigs[1].PayloadHash)
	}

	gotRaw := mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists/"+slug, nil, http.StatusOK)
	mustVerifyAll(t, "GET response", gotRaw)

	// The members the old path rewrote are served as sent. jsonb reorders keys, so compare values.
	var got map[string]json.RawMessage
	if err := json.Unmarshal(gotRaw, &got); err != nil {
		t.Fatal(err)
	}
	var curators []map[string]json.RawMessage
	if err := json.Unmarshal(got["curators"], &curators); err != nil {
		t.Fatal(err)
	}
	if name, ok := curators[0]["name"]; !ok || string(name) != `""` {
		t.Fatalf("curators[0].name: present-but-empty value dropped: %s", got["curators"])
	}
	if string(got["created"]) != `"2020-01-02T03:04:05+00:00"` {
		t.Fatalf("created rewritten: %s", got["created"])
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(got["items"], &items); err != nil {
		t.Fatal(err)
	}
	if string(items[0]["override"]) != `{"seed": 12345678901234567890}` && string(items[0]["override"]) != `{"seed":12345678901234567890}` {
		t.Fatalf("large integer not preserved: %s", items[0]["override"])
	}
}

// TestIntegration_SignedPlaylist_UnknownFieldRejected: the ff-cli#107 trigger. A signed document
// carrying items[].created — a member the request model does not describe — is rejected up front with
// the field named, instead of being silently stripped and failing signature verification later.
func TestIntegration_SignedPlaylist_UnknownFieldRejected(t *testing.T) {
	srv := newIntegrationServer(t)

	unsigned := []byte(`{"dpVersion":"1.1.0","id":"eeeeeeee-2222-4333-8444-555555555555","slug":"unknown-field",` +
		`"title":"ff-cli shaped","created":"2020-01-02T03:04:05Z",` +
		`"items":[{"source":"https://cdn.example.com/a.html","created":"2026-01-01T00:00:00Z"}]}`)
	signed, _ := curatorSigned(t, unsigned)

	body := doRaw(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(signed), http.StatusBadRequest, false)
	var resp ErrorResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "bad_request" || resp.Message != `json: unknown field "created"` {
		t.Fatalf("want bad_request naming the unknown field, got %+v", resp)
	}

	// The API-key path is strict too: a member the feed would have discarded is an error, not a no-op.
	apiKeyBody := []byte(`{"dpVersion":"1.1.0","title":"ops","items":[{"source":"https://cdn.example.com/a.html"}],"extra":1}`)
	body = doRaw(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(apiKeyBody), http.StatusBadRequest, true)
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Message != `json: unknown field "extra"` {
		t.Fatalf("API-key path should reject unknown fields, got %+v", resp)
	}
}

// TestIntegration_SignedPlaylist_ItemIDRequired: the feed's item index keys rows by items[].id. The
// API-key path assigns missing ids; a signed document cannot be edited, so it is refused up front with a
// clear 400 rather than failing inside the store.
func TestIntegration_SignedPlaylist_ItemIDRequired(t *testing.T) {
	srv := newIntegrationServer(t)

	unsigned := []byte(`{"dpVersion":"1.1.0","id":"abcdef01-2222-4333-8444-555555555555","slug":"no-item-id",` +
		`"title":"No item id","created":"2020-01-02T03:04:05Z",` +
		`"items":[{"source":"https://cdn.example.com/a.html"}]}`)
	signed, _ := curatorSigned(t, unsigned)

	body := doRaw(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(signed), http.StatusBadRequest, false)
	var resp ErrorResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "bad_request" || resp.Message != "signed playlists must give every item a UUID id: items[0]" {
		t.Fatalf("want bad_request naming the item, got %+v", resp)
	}
}

// TestIntegration_SignedPlaylist_ImmutableToOps: once a document carries a curator signature, the
// API-key path and PATCH refuse to edit it (409), because the edit would orphan that signature. A
// signed PUT that changes the document's identity is a 400; a signed PUT with the same identity replaces it.
func TestIntegration_SignedPlaylist_ImmutableToOps(t *testing.T) {
	srv := newIntegrationServer(t)

	id := uuid.MustParse("ffffffff-2222-4333-8444-555555555555")
	const slug = "signed-immutable"
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kid, err := dp1sign.Ed25519DIDKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	sign := func(unsigned []byte) []byte {
		sig, err := dp1sign.SignMultiEd25519(unsigned, priv, playlist.RoleCurator, "2020-01-02T03:04:05Z")
		if err != nil {
			t.Fatal(err)
		}
		var doc map[string]json.RawMessage
		if err := json.Unmarshal(unsigned, &doc); err != nil {
			t.Fatal(err)
		}
		doc["signatures"], _ = json.Marshal([]playlist.Signature{sig})
		out, err := json.Marshal(doc)
		if err != nil {
			t.Fatal(err)
		}
		return out
	}
	base := `"dpVersion":"1.1.0","id":"` + id.String() + `","slug":"` + slug + `","created":"2020-01-02T03:04:05Z",` +
		`"curators":[{"name":"Curator","key":"` + kid + `"}],` +
		`"items":[{"id":"aaaaaaaa-2222-4333-8444-555555555555","source":"https://cdn.example.com/a.html"}]`

	mustDoRawUnauthenticated(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(sign([]byte(`{`+base+`,"title":"v1"}`))), http.StatusCreated)

	// PATCH is API-key only, and refuses a curator-signed document.
	doRaw(t, srv, http.MethodPatch, "/api/v1/playlists/"+slug, map[string]any{"title": "ops edit"}, http.StatusUnauthorized, false)
	body := doRaw(t, srv, http.MethodPatch, "/api/v1/playlists/"+slug, map[string]any{"title": "ops edit"}, http.StatusConflict, true)
	var resp ErrorResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "conflict" {
		t.Fatalf("PATCH on signed document: want conflict, got %+v", resp)
	}

	// An API-key PUT (no signatures) is refused the same way.
	opsPut := map[string]any{"dpVersion": "1.1.0", "title": "ops replace", "items": []map[string]any{{"source": "https://cdn.example.com/b.html"}}}
	doRaw(t, srv, http.MethodPut, "/api/v1/playlists/"+slug, opsPut, http.StatusConflict, true)

	// A signed PUT that changes identity (different id) is rejected as a mismatch.
	otherID := uuid.MustParse("abababab-2222-4333-8444-555555555555")
	mismatch := `"dpVersion":"1.1.0","id":"` + otherID.String() + `","slug":"` + slug + `","created":"2020-01-02T03:04:05Z",` +
		`"curators":[{"name":"Curator","key":"` + kid + `"}],` +
		`"items":[{"id":"aaaaaaaa-2222-4333-8444-555555555555","source":"https://cdn.example.com/a.html"}],"title":"v2"`
	body = doRaw(t, srv, http.MethodPut, "/api/v1/playlists/"+slug, json.RawMessage(sign([]byte(`{`+mismatch+`}`))), http.StatusBadRequest, false)
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "bad_request" {
		t.Fatalf("signed PUT with different id: want bad_request, got %+v", resp)
	}

	// A signed PUT with the same identity replaces the document, and the new signatures verify.
	replaced := mustDoRawUnauthenticated(t, srv, http.MethodPut, "/api/v1/playlists/"+slug, json.RawMessage(sign([]byte(`{`+base+`,"title":"v2"}`))), http.StatusOK)
	mustVerifyAll(t, "signed PUT response", replaced)
	gotRaw := mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists/"+slug, nil, http.StatusOK)
	mustVerifyAll(t, "GET after signed PUT", gotRaw)
	var got playlist.Playlist
	if err := json.Unmarshal(gotRaw, &got); err != nil {
		t.Fatal(err)
	}
	if got.Title != "v2" {
		t.Fatalf("title after signed PUT: %q", got.Title)
	}
}

// TestIntegration_APIKeyPlaylist_StillMutable: compatibility for existing rows. A document the feed
// authored (feed signature only) remains editable via PATCH, and the feed's signature on the result verifies.
func TestIntegration_APIKeyPlaylist_StillMutable(t *testing.T) {
	srv := newIntegrationServer(t)

	post := map[string]any{"dpVersion": "1.1.0", "slug": "ops-owned", "title": "ops v1", "items": []map[string]any{{"source": "https://cdn.example.com/a.html"}}}
	createdRaw := mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", post, http.StatusCreated)
	mustVerifyAll(t, "API-key POST response", createdRaw)

	patched := mustDoRaw(t, srv, http.MethodPatch, "/api/v1/playlists/ops-owned", map[string]any{"title": "ops v2"}, http.StatusOK)
	sigs := mustVerifyAll(t, "API-key PATCH response", patched)
	if len(sigs) != 1 || sigs[0].Role != playlist.RoleFeed {
		t.Fatalf("feed-authored document should carry exactly the feed signature, got %+v", sigs)
	}
	gotRaw := mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists/ops-owned", nil, http.StatusOK)
	mustVerifyAll(t, "GET after API-key PATCH", gotRaw)
}

// TestIntegration_SlugImmutableOnUpdate: slug is assigned at creation and does not change on update.
// This keeps a document's own slug and its row address in agreement (no drift for a curator re-signing
// the served document) and keeps same-origin playlist URLs embedded in signed group/channel documents
// from being orphaned by a rename.
func TestIntegration_SlugImmutableOnUpdate(t *testing.T) {
	srv := newIntegrationServer(t)

	post := map[string]any{
		"dpVersion": "1.1.0", "slug": "stable", "title": "v1",
		"items": []map[string]any{{"id": "b2b2b2b2-2222-4333-8444-555555555555", "source": "https://cdn.example.com/a.html"}},
	}
	mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", post, http.StatusCreated)

	// An API-key PATCH that supplies a different slug does not move the row; the slug stays as created.
	patched := mustDoRaw(t, srv, http.MethodPatch, "/api/v1/playlists/stable", map[string]any{"slug": "renamed", "title": "v2"}, http.StatusOK)
	var pl playlist.Playlist
	if err := json.Unmarshal(patched, &pl); err != nil {
		t.Fatal(err)
	}
	if pl.Slug != "stable" || pl.Title != "v2" {
		t.Fatalf("slug must be immutable and other fields updatable: slug=%q title=%q", pl.Slug, pl.Title)
	}
	mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists/stable", nil, http.StatusOK)
	doRaw(t, srv, http.MethodGet, "/api/v1/playlists/renamed", nil, http.StatusNotFound, false)

	// The same holds for an API-key PUT: the row keeps its creation slug regardless of the body slug/title.
	put := map[string]any{"dpVersion": "1.1.0", "slug": "another", "title": "v3", "items": []map[string]any{{"source": "https://cdn.example.com/b.html"}}}
	replaced := mustDoRaw(t, srv, http.MethodPut, "/api/v1/playlists/stable", put, http.StatusOK)
	if err := json.Unmarshal(replaced, &pl); err != nil {
		t.Fatal(err)
	}
	if pl.Slug != "stable" {
		t.Fatalf("PUT slug = %q, want stable", pl.Slug)
	}
}

// signer is a curator/publisher key pair for the group and channel round-trips below.
type signer struct {
	priv ed25519.PrivateKey
	kid  string
}

func newSigner(t *testing.T) signer {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kid, err := dp1sign.Ed25519DIDKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return signer{priv: priv, kid: kid}
}

// sign appends one signature with the given role to the unsigned document bytes.
func (s signer) sign(t *testing.T, unsigned []byte, role string) []byte {
	t.Helper()
	sig, err := dp1sign.SignMultiEd25519(unsigned, s.priv, role, "2020-01-02T03:04:05Z")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(unsigned, &doc); err != nil {
		t.Fatal(err)
	}
	doc["signatures"], _ = json.Marshal([]playlist.Signature{sig})
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// seedLocalPlaylist creates an API-key playlist and returns the same-origin URL groups/channels reference.
func seedLocalPlaylist(t *testing.T, srv *Server, slug string) string {
	t.Helper()
	post := map[string]any{
		"dpVersion": "1.1.0", "slug": slug, "title": "member " + slug,
		"items": []map[string]any{{"id": uuid.New().String(), "source": "https://cdn.example.com/" + slug + ".html"}},
	}
	mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", post, http.StatusCreated)
	return "http://example.com/api/v1/playlists/" + slug
}

// TestIntegration_SignedPlaylistGroup_RoundTrip: the group document takes the same signed path as
// playlists — verified over received bytes, feed co-signed, stored and served verbatim, immutable to
// API-key edits, replaceable by a signed PUT with the same identity.
func TestIntegration_SignedPlaylistGroup_RoundTrip(t *testing.T) {
	srv := newIntegrationServer(t)
	member := seedLocalPlaylist(t, srv, "group-member")
	curator := newSigner(t)

	id := uuid.MustParse("c0c0c0c0-2222-4333-8444-555555555555")
	const slug = "signed-group"
	doc := func(title string) []byte {
		return []byte(`{"id":"` + id.String() + `","slug":"` + slug + `","title":"` + title + `","created":"2020-01-02T03:04:05Z",` +
			`"playlists":["` + member + `"],"curator":"` + curator.kid + `"}`)
	}

	createdRaw := mustDoRawUnauthenticated(t, srv, http.MethodPost, "/api/v1/playlist-groups", json.RawMessage(curator.sign(t, doc("v1"), playlist.RoleCurator)), http.StatusCreated)
	ok, failed, err := dp1sign.VerifyPlaylistGroupSignatures(createdRaw)
	if err != nil || !ok {
		t.Fatalf("POST response signatures: ok=%v failed=%+v err=%v", ok, failed, err)
	}
	gotRaw := mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlist-groups/"+slug, nil, http.StatusOK)
	if ok, failed, err := dp1sign.VerifyPlaylistGroupSignatures(gotRaw); err != nil || !ok {
		t.Fatalf("GET response signatures: ok=%v failed=%+v err=%v", ok, failed, err)
	}

	doRaw(t, srv, http.MethodPatch, "/api/v1/playlist-groups/"+slug, map[string]any{"title": "ops"}, http.StatusConflict, true)
	doRaw(t, srv, http.MethodPut, "/api/v1/playlist-groups/"+slug, map[string]any{"title": "ops", "playlists": []string{member}}, http.StatusConflict, true)

	replaced := mustDoRawUnauthenticated(t, srv, http.MethodPut, "/api/v1/playlist-groups/"+slug, json.RawMessage(curator.sign(t, doc("v2"), playlist.RoleCurator)), http.StatusOK)
	if ok, failed, err := dp1sign.VerifyPlaylistGroupSignatures(replaced); err != nil || !ok {
		t.Fatalf("signed PUT response signatures: ok=%v failed=%+v err=%v", ok, failed, err)
	}
	var got map[string]json.RawMessage
	if err := json.Unmarshal(mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlist-groups/"+slug, nil, http.StatusOK), &got); err != nil {
		t.Fatal(err)
	}
	if string(got["title"]) != `"v2"` {
		t.Fatalf("title after signed PUT: %s", got["title"])
	}
}

// TestIntegration_SignedChannel_RoundTrip is the channel counterpart (publisher-signed; channels
// require slug and version in the document).
func TestIntegration_SignedChannel_RoundTrip(t *testing.T) {
	srv := newIntegrationServer(t)
	member := seedLocalPlaylist(t, srv, "channel-member")
	publisher := newSigner(t)

	id := uuid.MustParse("d0d0d0d0-2222-4333-8444-555555555555")
	const slug = "signed-channel"
	doc := func(title string) []byte {
		return []byte(`{"id":"` + id.String() + `","slug":"` + slug + `","title":"` + title + `","version":"1.0.0","created":"2020-01-02T03:04:05Z",` +
			`"playlists":["` + member + `"],"publisher":{"name":"Publisher","key":"` + publisher.kid + `"}}`)
	}

	createdRaw := mustDoRawUnauthenticated(t, srv, http.MethodPost, "/api/v1/channels", json.RawMessage(publisher.sign(t, doc("v1"), "publisher")), http.StatusCreated)
	ok, failed, err := dp1sign.VerifyChannelSignatures(createdRaw)
	if err != nil || !ok {
		t.Fatalf("POST response signatures: ok=%v failed=%+v err=%v", ok, failed, err)
	}
	gotRaw := mustDoRaw(t, srv, http.MethodGet, "/api/v1/channels/"+slug, nil, http.StatusOK)
	if ok, failed, err := dp1sign.VerifyChannelSignatures(gotRaw); err != nil || !ok {
		t.Fatalf("GET response signatures: ok=%v failed=%+v err=%v", ok, failed, err)
	}

	doRaw(t, srv, http.MethodPatch, "/api/v1/channels/"+slug, map[string]any{"title": "ops"}, http.StatusConflict, true)
	doRaw(t, srv, http.MethodPut, "/api/v1/channels/"+slug, map[string]any{"title": "ops", "playlists": []string{member}}, http.StatusConflict, true)

	// Identity mismatch on a signed PUT (different id) is a 400.
	other := []byte(`{"id":"` + uuid.New().String() + `","slug":"` + slug + `","title":"v2","version":"1.0.0","created":"2020-01-02T03:04:05Z",` +
		`"playlists":["` + member + `"],"publisher":{"name":"Publisher","key":"` + publisher.kid + `"}}`)
	doRaw(t, srv, http.MethodPut, "/api/v1/channels/"+slug, json.RawMessage(publisher.sign(t, other, "publisher")), http.StatusBadRequest, false)

	replaced := mustDoRawUnauthenticated(t, srv, http.MethodPut, "/api/v1/channels/"+slug, json.RawMessage(publisher.sign(t, doc("v2"), "publisher")), http.StatusOK)
	if ok, failed, err := dp1sign.VerifyChannelSignatures(replaced); err != nil || !ok {
		t.Fatalf("signed PUT response signatures: ok=%v failed=%+v err=%v", ok, failed, err)
	}
}

// TestIntegration_SlugConflict: creating a document with a slug another row of the same kind already
// holds is a client conflict (409), not an internal error. (Slug is immutable after creation, so a
// move-into-a-taken-slug cannot occur; see TestIntegration_SlugImmutableOnUpdate.)
func TestIntegration_SlugConflict(t *testing.T) {
	srv := newIntegrationServer(t)

	playlistDoc := func(slug string) map[string]any {
		return map[string]any{"dpVersion": "1.1.0", "slug": slug, "title": slug, "items": []map[string]any{{"source": "https://cdn.example.com/a.html"}}}
	}
	mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", playlistDoc("taken"), http.StatusCreated)
	body := doRaw(t, srv, http.MethodPost, "/api/v1/playlists", playlistDoc("taken"), http.StatusConflict, true)
	var resp ErrorResponse
	if err := json.Unmarshal(body, &resp); err != nil || resp.Error != "conflict" {
		t.Fatalf("POST duplicate playlist slug: want conflict, got %s", body)
	}

	member := seedLocalPlaylist(t, srv, "conflict-member")
	membered := func(slug string) map[string]any {
		return map[string]any{"slug": slug, "title": slug, "playlists": []string{member}}
	}
	mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlist-groups", membered("g-taken"), http.StatusCreated)
	doRaw(t, srv, http.MethodPost, "/api/v1/playlist-groups", membered("g-taken"), http.StatusConflict, true)

	mustDoRaw(t, srv, http.MethodPost, "/api/v1/channels", membered("c-taken"), http.StatusCreated)
	doRaw(t, srv, http.MethodPost, "/api/v1/channels", membered("c-taken"), http.StatusConflict, true)
}

// TestIntegration_ListServesStoredBytes: list responses emit each document's bytes as stored, so a
// value containing JSON-significant characters (`<`, `&`) is not HTML-escaped the way gin's c.JSON
// would, and matches the single-resource GET representation.
func TestIntegration_ListServesStoredBytes(t *testing.T) {
	srv := newIntegrationServer(t)
	const slug = "escape-me"
	post := map[string]any{
		"dpVersion": "1.1.0", "slug": slug, "title": "a <b> & c",
		"items": []map[string]any{{"source": "https://cdn.example.com/a.html"}},
	}
	mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", post, http.StatusCreated)

	getRaw := mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists/"+slug, nil, http.StatusOK)
	if !bytes.Contains(getRaw, []byte(`"a <b> & c"`)) {
		t.Fatalf("single GET escaped the title: %s", getRaw)
	}
	listRaw := mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists?limit=100", nil, http.StatusOK)
	if !bytes.Contains(listRaw, []byte(`"a <b> & c"`)) {
		t.Fatalf("list did not serve the title as stored: %s", listRaw)
	}
	// The literal `<` and `&` are correct; gin's c.JSON would instead emit the escaped < / &.
	if bytes.Contains(listRaw, []byte(`\u003c`)) || bytes.Contains(listRaw, []byte(`\u0026`)) {
		t.Fatalf("list HTML-escaped document bytes (must match stored): %s", listRaw)
	}
}

// TestIntegration_LegacySignaturePreservedAndGuarded: a signed submission carrying both a v1.1
// signatures[] entry and a v1.0.x top-level signature keeps both, and the API-key path treats the
// legacy signature as foreign.
func TestIntegration_LegacySignaturePreservedAndGuarded(t *testing.T) {
	srv := newIntegrationServer(t)
	curator := newSigner(t)
	id := uuid.MustParse("e1e1e1e1-2222-4333-8444-555555555555")
	const slug = "legacy-signed"
	unsigned := []byte(`{"dpVersion":"1.1.0","id":"` + id.String() + `","slug":"` + slug + `","title":"Legacy","created":"2020-01-02T03:04:05Z",` +
		`"curators":[{"name":"Curator","key":"` + curator.kid + `"}],` +
		`"items":[{"id":"e2e2e2e2-2222-4333-8444-555555555555","source":"https://cdn.example.com/a.html"}]}`)
	legacy, err := dp1sign.SignLegacyEd25519(unsigned, curator.priv)
	if err != nil {
		t.Fatal(err)
	}
	signed := curator.sign(t, unsigned, playlist.RoleCurator)
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(signed, &doc); err != nil {
		t.Fatal(err)
	}
	doc["signature"], _ = json.Marshal(legacy)
	both, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}

	createdRaw := mustDoRawUnauthenticated(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(both), http.StatusCreated)
	mustVerifyAll(t, "POST with legacy signature", createdRaw)
	var pl playlist.Playlist
	if err := json.Unmarshal(mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists/"+slug, nil, http.StatusOK), &pl); err != nil {
		t.Fatal(err)
	}
	if pl.Signature != legacy {
		t.Fatalf("legacy signature not preserved: %q", pl.Signature)
	}
	doRaw(t, srv, http.MethodPatch, "/api/v1/playlists/"+slug, map[string]any{"title": "ops"}, http.StatusConflict, true)
}

func replaceOnce(s, old, repl string) string {
	for i := 0; i+len(old) <= len(s); i++ {
		if s[i:i+len(old)] == old {
			return s[:i] + repl + s[i+len(old):]
		}
	}
	return s
}

// TestIntegration_WriteResponseMatchesGet: a write response returns the persisted (jsonb) bytes, so it
// is byte-identical to a subsequent GET. Before, the write returned the pre-store signed bytes whose
// key order differed from jsonb's, so POST and GET disagreed byte-for-byte despite being JCS-equivalent.
func TestIntegration_WriteResponseMatchesGet(t *testing.T) {
	srv := newIntegrationServer(t)

	// API-key create: exercises the JSONB key reordering that made the two representations differ.
	post := map[string]any{
		"dpVersion": "1.1.0", "slug": "persisted", "title": "Persisted",
		"summary": "s", "coverImage": "https://cdn.example.com/c.png",
		"items": []map[string]any{{"id": "c3c3c3c3-2222-4333-8444-555555555555", "source": "https://cdn.example.com/a.html"}},
	}
	createResp := mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", post, http.StatusCreated)
	getResp := mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists/persisted", nil, http.StatusOK)
	if !bytes.Equal(createResp, getResp) {
		t.Fatalf("POST response must be byte-identical to GET:\n POST=%s\n GET =%s", createResp, getResp)
	}

	patchResp := mustDoRaw(t, srv, http.MethodPatch, "/api/v1/playlists/persisted", map[string]any{"title": "Persisted v2"}, http.StatusOK)
	getResp2 := mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists/persisted", nil, http.StatusOK)
	if !bytes.Equal(patchResp, getResp2) {
		t.Fatalf("PATCH response must be byte-identical to GET:\n PATCH=%s\n GET  =%s", patchResp, getResp2)
	}
}

// TestIntegration_SignedTrailingBytesIs400: a signed body followed by trailing content is a
// client-correctable 400 (bindDocument), not a misleading 401 from the auth middleware's signature peek.
func TestIntegration_SignedTrailingBytesIs400(t *testing.T) {
	srv := newIntegrationServer(t)
	unsigned := []byte(`{"dpVersion":"1.1.0","id":"f0f0f0f0-2222-4333-8444-555555555555","slug":"trailing",` +
		`"title":"Trailing","created":"2020-01-02T03:04:05Z",` +
		`"items":[{"id":"f1f1f1f1-2222-4333-8444-555555555555","source":"https://cdn.example.com/a.html"}]}`)
	signed, _ := curatorSigned(t, unsigned)
	trailing := append(append([]byte(nil), signed...), []byte(`{"garbage":true}`)...)

	// Send the trailing bytes verbatim (the JSON helpers would reject them before transport). No API key:
	// the request takes the signature path, whose first JSON value carries signatures[].
	req := httptest.NewRequest(http.MethodPost, "/api/v1/playlists", bytes.NewReader(trailing))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.engine.ServeHTTP(rec, req)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("signed body with trailing content: status=%d want 400 body=%s", rec.Code, rec.Body.String())
	}
	var resp ErrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "bad_request" {
		t.Fatalf("want bad_request, got %+v", resp)
	}
}

// TestIntegration_RegistryRejectsUnknownField: strict decoding applies to the registry PUT too, so its
// OpenAPI schemas declare additionalProperties: false. An unknown member is a 400, not a silent drop.
func TestIntegration_RegistryRejectsUnknownField(t *testing.T) {
	srv := newIntegrationServer(t)
	chURL := "http://example.com/api/v1/channels/aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"

	// Unknown member on the publisher object.
	pubExtra := map[string]any{"publishers": []map[string]any{{"name": "P", "channel_urls": []string{chURL}, "extra": 1}}}
	body := doRaw(t, srv, http.MethodPut, "/api/v1/registry/channels", pubExtra, http.StatusBadRequest, true)
	var resp ErrorResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "bad_request" {
		t.Fatalf("registry publisher unknown field: want bad_request, got %+v", resp)
	}

	// Unknown member on the top-level registry object.
	topExtra := map[string]any{"publishers": []map[string]any{{"name": "P", "channel_urls": []string{chURL}}}, "extra": 1}
	doRaw(t, srv, http.MethodPut, "/api/v1/registry/channels", topExtra, http.StatusBadRequest, true)
}
