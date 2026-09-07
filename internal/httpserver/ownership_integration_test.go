//go:build integration

package httpserver

import (
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/display-protocol/dp1-go/extension/channels"
	"github.com/display-protocol/dp1-go/playlist"
	dp1sign "github.com/display-protocol/dp1-go/sign"
	"github.com/google/uuid"
)

// These tests pin role-aware ownership end to end with real Ed25519 signatures (HTTP → executor →
// dp1-go verifier → Postgres): who a document's owners are, that the signature's role — not only its
// kid — decides whether it authorizes, and that the owner set can grow but not shrink on PUT.

// signer is one party co-signing a document: its key and the role it signs under.
type signer struct {
	priv ed25519.PrivateKey
	role string
}

// newSigner generates a fresh identity signing under role and returns it with its did:key.
func newSigner(t *testing.T, role string) (signer, string) {
	t.Helper()
	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	kid, err := dp1sign.Ed25519DIDKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	return signer{priv: priv, role: role}, kid
}

// signWithAll splices one signature entry per signer into unsigned, each over the same JCS digest,
// leaving every other member byte-for-byte as authored (see signWith).
func signWithAll(t *testing.T, unsigned []byte, signers ...signer) []byte {
	t.Helper()
	sigs := make([]playlist.Signature, 0, len(signers))
	for _, s := range signers {
		sig, err := dp1sign.SignMultiEd25519(unsigned, s.priv, s.role, "2026-01-02T03:04:05Z")
		if err != nil {
			t.Fatal(err)
		}
		sigs = append(sigs, sig)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(unsigned, &doc); err != nil {
		t.Fatal(err)
	}
	var err error
	doc["signatures"], err = json.Marshal(sigs)
	if err != nil {
		t.Fatal(err)
	}
	out, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// mustErrorContaining decodes an error response and asserts its code and that the message names what
// the client must fix.
func mustErrorContaining(t *testing.T, raw []byte, wantCode, wantSubstr string) {
	t.Helper()
	var resp ErrorResponse
	if err := json.Unmarshal(raw, &resp); err != nil {
		t.Fatalf("decode error response: %v body=%s", err, raw)
	}
	if resp.Error != wantCode || !strings.Contains(resp.Message, wantSubstr) {
		t.Fatalf("want %s mentioning %q, got %+v", wantCode, wantSubstr, resp)
	}
}

// TestIntegration_Ownership_DeclaredCuratorMustSignAsCurator: a key listed in curators[] whose only
// signature carries the licensor role is not acting as the author. The signature verifies
// cryptographically, so this is exactly the case a kid-only check would wrongly accept.
func TestIntegration_Ownership_DeclaredCuratorMustSignAsCurator(t *testing.T) {
	srv := newIntegrationServer(t)

	asLicensor, kid := newSigner(t, playlist.RoleLicensor)
	unsigned := []byte(`{"dpVersion":"1.1.0","id":"0a0a0a0a-1111-4333-8444-555555555555","slug":"licensor-not-curator",` +
		`"title":"role matters","created":"2026-01-02T03:04:05Z",` +
		`"curators":[{"name":"Rights holder","key":"` + kid + `"}],` +
		`"items":[{"id":"0a0a0a0a-2222-4333-8444-555555555555","source":"https://cdn.example.com/a.html"}]}`)

	raw := doRaw(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(signWithAll(t, unsigned, asLicensor)), http.StatusBadRequest)
	mustErrorContaining(t, raw, "signature_verification_failed", `"curator" role is required`)

	// The same key signing as curator is the author.
	asCurator := signer{priv: asLicensor.priv, role: playlist.RoleCurator}
	created := mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(signWithAll(t, unsigned, asCurator)), http.StatusCreated)
	mustVerifyAll(t, "curator-signed create", created)
}

// TestIntegration_Ownership_CoreOnlyPlaylist: DP-1 core has no curators[]; the curator-role signer owns
// the document and a licensor co-signer does not, for create and for the delete intent alike.
func TestIntegration_Ownership_CoreOnlyPlaylist(t *testing.T) {
	srv := newIntegrationServer(t)

	curator, _ := newSigner(t, playlist.RoleCurator)
	licensor, _ := newSigner(t, playlist.RoleLicensor)
	id := uuid.MustParse("0b0b0b0b-1111-4333-8444-555555555555")
	const slug = "core-only"
	unsigned := []byte(`{"dpVersion":"1.1.0","id":"` + id.String() + `","slug":"` + slug + `",` +
		`"title":"core only","created":"2026-01-02T03:04:05Z",` +
		`"items":[{"id":"0b0b0b0b-2222-4333-8444-555555555555","source":"https://cdn.example.com/a.html"}]}`)

	// Only a licensor signature: nobody owns it, so it cannot be created.
	raw := doRaw(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(signWithAll(t, unsigned, licensor)), http.StatusBadRequest)
	mustErrorContaining(t, raw, "signature_verification_failed", "no valid curator signature")

	// Curator + licensor: created; both signatures are preserved and verify over the served bytes.
	created := mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(signWithAll(t, unsigned, licensor, curator)), http.StatusCreated)
	if sigs := mustVerifyAll(t, "core-only create", created); len(sigs) != 3 {
		t.Fatalf("want licensor + curator + feed signatures, got %+v", sigs)
	}

	// The licensor co-signed the content but is not an owner: its delete intent is refused.
	raw = doRaw(t, srv, http.MethodDelete, "/api/v1/playlists/"+slug, signedDeleteBody(t, licensor.priv, "playlist", id.String(), slug), http.StatusForbidden)
	mustErrorContaining(t, raw, "forbidden", "not signed by an owner")

	mustDoRaw(t, srv, http.MethodDelete, "/api/v1/playlists/"+slug, signedDeleteBody(t, curator.priv, "playlist", id.String(), slug), http.StatusNoContent)
}

// TestIntegration_Ownership_OwnerSetGrowsWithConsent: on PUT a curator may be added when the new key
// co-signs as curator; adding without that signature, or dropping the original owner, is refused.
func TestIntegration_Ownership_OwnerSetGrowsWithConsent(t *testing.T) {
	srv := newIntegrationServer(t)

	first, firstKid := newSigner(t, playlist.RoleCurator)
	second, secondKid := newSigner(t, playlist.RoleCurator)
	id := uuid.MustParse("0c0c0c0c-1111-4333-8444-555555555555")
	const slug = "co-owned"
	body := func(title string, curatorKids ...string) []byte {
		entities := make([]string, 0, len(curatorKids))
		for _, k := range curatorKids {
			entities = append(entities, `{"name":"Curator","key":"`+k+`"}`)
		}
		return []byte(`{"dpVersion":"1.1.0","id":"` + id.String() + `","slug":"` + slug + `","created":"2026-01-02T03:04:05Z",` +
			`"curators":[` + strings.Join(entities, ",") + `],` +
			`"items":[{"id":"0c0c0c0c-2222-4333-8444-555555555555","source":"https://cdn.example.com/a.html"}],` +
			`"title":"` + title + `"}`)
	}
	put := func(doc []byte, wantStatus int) []byte {
		return doRaw(t, srv, http.MethodPut, "/api/v1/playlists/"+slug,
			signedReplaceEnvelope(t, first.priv, "playlist", id.String(), slug, json.RawMessage(doc)), wantStatus)
	}

	mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(signWithAll(t, body("v1", firstKid), first)), http.StatusCreated)

	// Listing the second curator without its signature: attribution without consent, refused.
	raw := put(signWithAll(t, body("v2", firstKid, secondKid), first), http.StatusForbidden)
	mustErrorContaining(t, raw, "forbidden", secondKid)

	// Both sign as curator: the owner set grows.
	replaced := put(signWithAll(t, body("v2", firstKid, secondKid), first, second), http.StatusOK)
	mustVerifyAll(t, "co-owned PUT", replaced)

	// The second owner may now authorize on its own — and may not drop the first.
	raw = doRaw(t, srv, http.MethodPut, "/api/v1/playlists/"+slug,
		signedReplaceEnvelope(t, second.priv, "playlist", id.String(), slug, json.RawMessage(signWithAll(t, body("v3", secondKid), second))), http.StatusForbidden)
	mustErrorContaining(t, raw, "forbidden", firstKid)

	replaced = doRaw(t, srv, http.MethodPut, "/api/v1/playlists/"+slug,
		signedReplaceEnvelope(t, second.priv, "playlist", id.String(), slug, json.RawMessage(signWithAll(t, body("v3", firstKid, secondKid), second))), http.StatusOK)
	mustVerifyAll(t, "second-owner PUT", replaced)
	var got playlist.Playlist
	if err := json.Unmarshal(replaced, &got); err != nil {
		t.Fatal(err)
	}
	if got.Title != "v3" {
		t.Fatalf("title after second-owner PUT: %q", got.Title)
	}
}

// TestIntegration_Ownership_ChannelPublisherRole walks a channel through POST, PUT and DELETE over HTTP
// with real signatures: the declared publisher owns it only when signing as `publisher`, for the
// document and for the intent alike. Channel `curators` are attribution and never authorize.
func TestIntegration_Ownership_ChannelPublisherRole(t *testing.T) {
	srv := newIntegrationServer(t)

	// A member playlist to reference, owned by its own curator.
	curator, curatorKid := newSigner(t, playlist.RoleCurator)
	plUnsigned := []byte(`{"dpVersion":"1.1.0","id":"0d0d0d0d-1111-4333-8444-555555555555","slug":"channel-member",` +
		`"title":"member","created":"2026-01-02T03:04:05Z",` +
		`"curators":[{"name":"Curator","key":"` + curatorKid + `"}],` +
		`"items":[{"id":"0d0d0d0d-2222-4333-8444-555555555555","source":"https://cdn.example.com/a.html"}]}`)
	mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(signWithAll(t, plUnsigned, curator)), http.StatusCreated)

	publisher, publisherKid := newSigner(t, channels.RolePublisher)
	id := uuid.MustParse("0e0e0e0e-1111-4333-8444-555555555555")
	const slug = "publisher-role"
	channelDoc := func(title string) []byte {
		return []byte(`{"id":"` + id.String() + `","slug":"` + slug + `","title":"` + title + `","version":"1.0.0",` +
			`"created":"2026-01-02T03:04:05Z",` +
			`"publisher":{"name":"Gallery","key":"` + publisherKid + `"},` +
			`"curators":[{"name":"Curator","key":"` + curatorKid + `"}],` +
			`"playlists":["http://example.com/api/v1/playlists/channel-member"]}`)
	}

	// The publisher key signing as curator is not the owner; the curator signing as curator never is.
	asCurator := signer{priv: publisher.priv, role: playlist.RoleCurator}
	raw := doRaw(t, srv, http.MethodPost, "/api/v1/channels", json.RawMessage(signWithAll(t, channelDoc("v1"), asCurator, curator)), http.StatusBadRequest)
	mustErrorContaining(t, raw, "signature_verification_failed", `"publisher" role is required`)

	mustDoRaw(t, srv, http.MethodPost, "/api/v1/channels", json.RawMessage(signWithAll(t, channelDoc("v1"), publisher, curator)), http.StatusCreated)

	// Replace: publisher-role intent from the publisher key.
	replaced := mustDoRaw(t, srv, http.MethodPut, "/api/v1/channels/"+slug,
		signedReplaceEnvelope(t, publisher.priv, "channel", id.String(), slug, json.RawMessage(signWithAll(t, channelDoc("v2"), publisher))), http.StatusOK)
	var got channels.Channel
	if err := json.Unmarshal(replaced, &got); err != nil {
		t.Fatal(err)
	}
	if got.Title != "v2" {
		t.Fatalf("title after publisher PUT: %q", got.Title)
	}
	if ok, failed, err := dp1sign.VerifyChannelSignatures(replaced); err != nil || !ok {
		t.Fatalf("channel signatures do not verify over served bytes: ok=%v failed=%+v err=%v", ok, failed, err)
	}

	// Delete: the publisher key signing the intent as curator is refused; as publisher it succeeds.
	raw = doRaw(t, srv, http.MethodDelete, "/api/v1/channels/"+slug,
		signedDeleteBodyAs(t, publisher.priv, playlist.RoleCurator, "channel", id.String(), slug), http.StatusForbidden)
	mustErrorContaining(t, raw, "forbidden", `"publisher" role is required`)
	raw = doRaw(t, srv, http.MethodDelete, "/api/v1/channels/"+slug,
		signedDeleteBody(t, curator.priv, "channel", id.String(), slug), http.StatusForbidden)
	mustErrorContaining(t, raw, "forbidden", "not signed by an owner")
	mustDoRaw(t, srv, http.MethodDelete, "/api/v1/channels/"+slug, signedDeleteBody(t, publisher.priv, "channel", id.String(), slug), http.StatusNoContent)
}

// TestIntegration_Ownership_RelabeledRoleIsNotDetected pins the documented limit rather than a
// guarantee: `role` sits in the signature entry, outside the signed bytes, so a relayer can rewrite a
// declared curator's `licensor` entry to `curator` and the feed — like every DP-1 verifier — cannot
// tell. The role check is spec conformance and honest-mistake detection, not a cryptographic boundary;
// this test exists so that anyone who tightens the claim in the docs has to argue with it first.
func TestIntegration_Ownership_RelabeledRoleIsNotDetected(t *testing.T) {
	srv := newIntegrationServer(t)

	asLicensor, kid := newSigner(t, playlist.RoleLicensor)
	unsigned := []byte(`{"dpVersion":"1.1.0","id":"0f0f0f0f-1111-4333-8444-555555555555","slug":"relabeled",` +
		`"title":"relabel","created":"2026-01-02T03:04:05Z",` +
		`"curators":[{"name":"Rights holder","key":"` + kid + `"}],` +
		`"items":[{"id":"0f0f0f0f-2222-4333-8444-555555555555","source":"https://cdn.example.com/a.html"}]}`)
	signedAsLicensor := signWithAll(t, unsigned, asLicensor)

	// As signed: refused, the licensor entry does not authorize.
	doRaw(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(signedAsLicensor), http.StatusBadRequest)

	// Relabeled by a relayer, bytes of `sig` untouched: still verifies, and now authorizes.
	relabeled := strings.Replace(string(signedAsLicensor), `"role":"licensor"`, `"role":"curator"`, 1)
	if relabeled == string(signedAsLicensor) {
		t.Fatal("test setup: expected a licensor role entry to relabel")
	}
	if ok, _, err := dp1sign.VerifyPlaylistSignatures([]byte(relabeled)); err != nil || !ok {
		t.Fatalf("relabeled document should still verify (role is unsigned): ok=%v err=%v", ok, err)
	}
	mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", json.RawMessage(relabeled), http.StatusCreated)
}
