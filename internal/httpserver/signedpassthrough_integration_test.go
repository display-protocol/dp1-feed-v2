//go:build integration

package httpserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/display-protocol/dp1-go/playlist"
	dp1sign "github.com/display-protocol/dp1-go/sign"
	"github.com/google/uuid"
)

// These tests pin the signed-document contract end to end (HTTP → executor → dp1svc → Postgres →
// HTTP): the feed verifies, co-signs, stores, and serves the client's bytes without editing them.
// Every assertion that matters is made by dp1-go's own verifier over the bytes the feed actually
// serves, so a regression to a rebuild-then-hash flow (feral-file/ff-cli#107) fails here rather than
// in a partner's verifier.

// signWithNewKey signs unsigned with a fresh Ed25519 key the way a publishing tool does: sign the JCS
// digest of the bytes, then splice "signatures" into the same object so every other member stays
// byte-for-byte as authored.
func signWithNewKey(t *testing.T, unsigned []byte, role string) (signed []byte, kid string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kid, err = dp1sign.Ed25519DIDKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return signWith(t, priv, unsigned, role), kid
}

// signWith appends one signature with the given role to the unsigned document bytes.
func signWith(t *testing.T, priv ed25519.PrivateKey, unsigned []byte, role string) []byte {
	t.Helper()
	sig, err := dp1sign.SignMultiEd25519(unsigned, priv, role, "2026-01-02T03:04:05Z")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(unsigned, &doc); err != nil {
		t.Fatal(err)
	}
	doc["signatures"], err = json.Marshal([]playlist.Signature{sig})
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
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

// TestIntegration_SignedPlaylist_VerbatimRoundTrip publishes a document containing exactly the members
// the old rebuild used to change — a present-but-empty string (curators[].name), a non-UTC RFC3339
// offset, and an integer above 2^53 — then asserts the curator and feed signatures both verify over the
// POST response and the GET response, and that those members come back unaltered.
func TestIntegration_SignedPlaylist_VerbatimRoundTrip(t *testing.T) {
	srv := newIntegrationServer(t)

	id := uuid.MustParse("dddddddd-2222-4333-8444-555555555555")
	const slug = "signed-verbatim"
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kid, err := dp1sign.Ed25519DIDKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	// created carries a +07:00 offset: the feed must not normalise it to UTC, which would change the
	// signed bytes and orphan the curator signature.
	unsigned := []byte(`{"dpVersion":"1.1.0","id":"` + id.String() + `","slug":"` + slug + `",` +
		`"title":"Signed verbatim","created":"2026-01-02T03:04:05+07:00",` +
		`"curators":[{"name":"","key":"` + kid + `"}],` +
		`"items":[{"id":"a1a1a1a1-2222-4333-8444-555555555555","source":"https://cdn.example.com/a.html","duration":10,"override":{"seed":12345678901234567890}}]}`)
	signed := signWith(t, priv, unsigned, playlist.RoleCurator)

	createdRaw := mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(signed), http.StatusCreated)
	sigs := mustVerifyAll(t, "POST response", createdRaw)
	if len(sigs) != 2 || sigs[0].Kid != kid || sigs[1].Role != playlist.RoleFeed {
		t.Fatalf("POST signatures: want [curator, feed], got %+v", sigs)
	}
	// Both signers attest the same payload (DP-1 §7.1.1).
	if sigs[0].PayloadHash != sigs[1].PayloadHash {
		t.Fatalf("payload_hash differs: curator=%s feed=%s", sigs[0].PayloadHash, sigs[1].PayloadHash)
	}

	gotRaw := mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists/"+slug, nil, http.StatusOK)
	mustVerifyAll(t, "GET response", gotRaw)

	// The members the old rebuild rewrote are served as sent. jsonb reorders keys, so compare values.
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
	if string(got["created"]) != `"2026-01-02T03:04:05+07:00"` {
		t.Fatalf("created rewritten (offset must survive): %s", got["created"])
	}
	var items []map[string]json.RawMessage
	if err := json.Unmarshal(got["items"], &items); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(items[0]["override"]), "12345678901234567890") {
		t.Fatalf("large integer not preserved: %s", items[0]["override"])
	}
}

// TestIntegration_SignedPlaylist_UnknownFieldRejected is the ff-cli#107 trigger: a signed document
// carrying a member the request model does not describe is rejected up front with the field named,
// instead of being silently stripped and failing signature verification later.
func TestIntegration_SignedPlaylist_UnknownFieldRejected(t *testing.T) {
	srv := newIntegrationServer(t)

	unsigned := []byte(`{"dpVersion":"1.1.0","id":"eeeeeeee-2222-4333-8444-555555555555","slug":"unknown-field",` +
		`"title":"ff-cli shaped","created":"2026-01-02T03:04:05Z",` +
		`"curators":[{"key":"KID"}],` +
		`"items":[{"id":"b1b1b1b1-2222-4333-8444-555555555555","source":"https://cdn.example.com/a.html","created":"2026-01-01T00:00:00Z"}]}`)
	signed, kid := signWithNewKey(t, unsigned, playlist.RoleCurator)
	signed = []byte(strings.Replace(string(signed), "KID", kid, 1))

	body := doRaw(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(signed), http.StatusBadRequest)
	var resp ErrorResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "bad_request" || resp.Message != `json: unknown field "created"` {
		t.Fatalf("want bad_request naming the unknown field, got %+v", resp)
	}
}

// TestIntegration_SignedPlaylist_ItemIDRequired: the item index keys rows by items[].id, and a signed
// document cannot be edited to add one, so the feed refuses it up front rather than minting an id that
// would orphan the signature.
func TestIntegration_SignedPlaylist_ItemIDRequired(t *testing.T) {
	srv := newIntegrationServer(t)

	unsigned := []byte(`{"dpVersion":"1.1.0","id":"abcdef01-2222-4333-8444-555555555555","slug":"no-item-id",` +
		`"title":"No item id","created":"2026-01-02T03:04:05Z",` +
		`"curators":[{"key":"KID"}],` +
		`"items":[{"source":"https://cdn.example.com/a.html"}]}`)
	signed, kid := signWithNewKey(t, unsigned, playlist.RoleCurator)
	signed = []byte(strings.Replace(string(signed), "KID", kid, 1))

	body := doRaw(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(signed), http.StatusBadRequest)
	var resp ErrorResponse
	if err := json.Unmarshal(body, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "bad_request" || !strings.Contains(resp.Message, "items[0]") {
		t.Fatalf("want bad_request naming the item, got %+v", resp)
	}
}

// TestIntegration_SignedPlaylist_PutIdentityValidated: a PUT must carry the stored identity. A document
// whose id differs is refused with 400 (validated, not silently substituted); the matching document
// replaces the row and its signatures still verify over the served bytes.
func TestIntegration_SignedPlaylist_PutIdentityValidated(t *testing.T) {
	srv := newIntegrationServer(t)

	id := uuid.MustParse("ffffffff-2222-4333-8444-555555555555")
	const slug = "signed-put-identity"
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kid, err := dp1sign.Ed25519DIDKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	body := func(docID, title string) []byte {
		return []byte(`{"dpVersion":"1.1.0","id":"` + docID + `","slug":"` + slug + `","created":"2026-01-02T03:04:05Z",` +
			`"curators":[{"name":"Curator","key":"` + kid + `"}],` +
			`"items":[{"id":"aaaaaaaa-2222-4333-8444-555555555555","source":"https://cdn.example.com/a.html"}],` +
			`"title":"` + title + `"}`)
	}

	mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(signWith(t, priv, body(id.String(), "v1"), playlist.RoleCurator)), http.StatusCreated)

	// Different id in the signed document → identity mismatch, refused.
	other := uuid.MustParse("abababab-2222-4333-8444-555555555555")
	raw := doRaw(t, srv, http.MethodPut, "/api/v1/playlists/"+slug, json.RawMessage(signWith(t, priv, body(other.String(), "v2"), playlist.RoleCurator)), http.StatusBadRequest)
	var resp ErrorResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Error != "bad_request" {
		t.Fatalf("signed PUT with different id: want bad_request, got %+v", resp)
	}

	// Same identity replaces the document; signatures verify over the response and the later GET.
	replaced := mustDoRaw(t, srv, http.MethodPut, "/api/v1/playlists/"+slug, json.RawMessage(signWith(t, priv, body(id.String(), "v2"), playlist.RoleCurator)), http.StatusOK)
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
