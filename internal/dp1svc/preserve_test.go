package dp1svc_test

import (
	"bytes"
	"crypto/ed25519"
	"crypto/rand"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/display-protocol/dp1-go/playlist"
	dp1sign "github.com/display-protocol/dp1-go/sign"

	"github.com/display-protocol/dp1-feed-v2/internal/dp1svc"
)

const preserveSeedHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func newPreserveService(t *testing.T) *dp1svc.Service {
	t.Helper()
	priv, err := dp1svc.Ed25519PrivateKeyFromHex(preserveSeedHex)
	if err != nil {
		t.Fatal(err)
	}
	kid, err := dp1sign.Ed25519DIDKey(priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := dp1svc.New(preserveSeedHex, kid)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

// TestSignPlaylist_preservesEveryOtherMember pins the signer's contract: the output differs from the
// input only in "signatures". Members that a typed or map[string]any round-trip would alter — an
// integer above 2^53, a present-but-empty string, an unknown key, HTML-significant characters, and a
// legacy top-level "signature" — all come back byte-for-byte, and a curator signature made before the
// feed signed still verifies over the output alongside the feed's.
func TestSignPlaylist_preservesEveryOtherMember(t *testing.T) {
	t.Parallel()
	svc := newPreserveService(t)

	pub, priv, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	curatorKid, err := dp1sign.Ed25519DIDKey(pub)
	if err != nil {
		t.Fatal(err)
	}
	unsigned := []byte(`{"dpVersion":"1.1.0","title":"a <b> & c","summary":"","items":[{"source":"https://x","override":{"seed":12345678901234567890}}],"future":{"k":[1,2.50,"x"]}}`)
	legacy, err := dp1sign.SignLegacyEd25519(unsigned, priv)
	if err != nil {
		t.Fatal(err)
	}
	curator, err := dp1sign.SignMultiEd25519(unsigned, priv, playlist.RoleCurator, "2020-01-02T03:04:05Z")
	if err != nil {
		t.Fatal(err)
	}
	var doc map[string]json.RawMessage
	if err := json.Unmarshal(unsigned, &doc); err != nil {
		t.Fatal(err)
	}
	doc["signature"], _ = json.Marshal(legacy)
	doc["signatures"], _ = json.Marshal([]playlist.Signature{curator})
	// Encode the input without HTML escaping so the "<b> &" assertion below tests the signer, not
	// this test's own encoder.
	var inBuf bytes.Buffer
	enc := json.NewEncoder(&inBuf)
	enc.SetEscapeHTML(false)
	if err := enc.Encode(doc); err != nil {
		t.Fatal(err)
	}
	in := bytes.TrimSpace(inBuf.Bytes())

	out, err := svc.SignPlaylist(in, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}

	for _, want := range []string{
		`12345678901234567890`, `"summary":""`, `"future":{"k":[1,2.50,"x"]}`, `"title":"a <b> & c"`,
		`"signature":"` + legacy + `"`,
	} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("signed output lost %s:\n%s", want, out)
		}
	}

	ok, failed, err := dp1sign.VerifyPlaylistSignatures(out)
	if err != nil || !ok {
		t.Fatalf("signatures over signed output: ok=%v failed=%+v err=%v", ok, failed, err)
	}
	var signed playlist.Playlist
	if err := json.Unmarshal(out, &signed); err != nil {
		t.Fatal(err)
	}
	if len(signed.Signatures) != 2 || signed.Signatures[0].Kid != curatorKid || signed.Signatures[1].Kid != svc.Kid() {
		t.Fatalf("want [curator, feed] signatures, got %+v", signed.Signatures)
	}
	if signed.Signatures[0].PayloadHash != signed.Signatures[1].PayloadHash {
		t.Fatal("curator and feed must attest the same payload_hash")
	}
	if err := dp1sign.VerifyLegacyEd25519(out, signed.Signature, pub); err != nil {
		t.Fatalf("legacy signature no longer verifies over signed output: %v", err)
	}
}

// TestSignPlaylist_replacesOnlyItsOwnKid: re-signing a document that already carries this feed's
// signature refreshes that one entry and leaves every foreign entry in place.
func TestSignPlaylist_replacesOnlyItsOwnKid(t *testing.T) {
	t.Parallel()
	svc := newPreserveService(t)
	in := []byte(`{"dpVersion":"1.1.0","title":"t","items":[{"source":"https://x"}]}`)

	once, err := svc.SignPlaylist(in, time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	twice, err := svc.SignPlaylist(once, time.Date(2026, 1, 2, 0, 0, 0, 0, time.UTC))
	if err != nil {
		t.Fatal(err)
	}
	var pl playlist.Playlist
	if err := json.Unmarshal(twice, &pl); err != nil {
		t.Fatal(err)
	}
	if len(pl.Signatures) != 1 || pl.Signatures[0].Kid != svc.Kid() || pl.Signatures[0].Ts != "2026-01-02T00:00:00Z" {
		t.Fatalf("re-sign should replace the feed entry only, got %+v", pl.Signatures)
	}
}
