package dp1svc

import (
	"crypto/ed25519"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	dp1 "github.com/display-protocol/dp1-go"
	"github.com/display-protocol/dp1-go/playlist"
	"github.com/display-protocol/dp1-go/sign"
)

// minimalSignedPlaylistV11 returns core playlist JSON that satisfies the v1.1 schema (requires signatures or legacy signature).
func minimalSignedPlaylistV11(t *testing.T) []byte {
	t.Helper()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	pl := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "Hello",
		Items:     []playlist.PlaylistItem{{Source: "https://example.com/a"}},
	}
	raw, err := json.Marshal(pl)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := sign.SignMultiEd25519(raw, priv, playlist.RoleCurator, "2025-06-01T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	pl.Signatures = []playlist.Signature{sig}
	out, err := json.Marshal(pl)
	if err != nil {
		t.Fatal(err)
	}
	return out
}

const testSeedHex = "0123456789abcdef0123456789abcdef0123456789abcdef0123456789abcdef"

func TestVerifyPlaylistSignatures(t *testing.T) {
	t.Parallel()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	kid, err := sign.Ed25519DIDKey(priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}

	svc, err := New(testSeedHex, kid)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("valid_signature", func(t *testing.T) {
		t.Parallel()
		signed := minimalSignedPlaylistV11(t)
		ok, failed, err := svc.VerifyPlaylistSignatures(signed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ok {
			t.Fatalf("expected ok=true, got failed: %v", failed)
		}
		if len(failed) != 0 {
			t.Fatalf("expected no failed signatures, got: %v", failed)
		}
	})

	t.Run("invalid_json", func(t *testing.T) {
		t.Parallel()
		_, _, err := svc.VerifyPlaylistSignatures([]byte(`{invalid`))
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})

	t.Run("no_signatures", func(t *testing.T) {
		t.Parallel()
		pl := playlist.Playlist{
			DPVersion: "1.1.0",
			Title:     "Test",
			Items:     []playlist.PlaylistItem{{Source: "https://example.com"}},
		}
		raw, _ := json.Marshal(pl)
		_, _, err := svc.VerifyPlaylistSignatures(raw)
		if err == nil || !errors.Is(err, sign.ErrNoSignatures) {
			t.Fatalf("expected ErrNoSignatures, got: %v", err)
		}
	})
}

func TestVerifyPlaylistGroupSignatures(t *testing.T) {
	t.Parallel()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	kid, err := sign.Ed25519DIDKey(priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}

	svc, err := New(testSeedHex, kid)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("valid_signature", func(t *testing.T) {
		t.Parallel()
		pg := map[string]any{
			"dpVersion": "1.1.0",
			"title":     "Test Group",
			"playlists": []string{"https://example.com/pl/1"},
		}
		raw, _ := json.Marshal(pg)
		sig, _ := sign.SignMultiEd25519(raw, priv, "curator", "2025-06-01T12:00:00Z")
		pg["signatures"] = []any{sig}
		signed, _ := json.Marshal(pg)

		ok, failed, err := svc.VerifyPlaylistGroupSignatures(signed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ok {
			t.Fatalf("expected ok=true, got failed: %v", failed)
		}
	})

	t.Run("invalid_json", func(t *testing.T) {
		t.Parallel()
		_, _, err := svc.VerifyPlaylistGroupSignatures([]byte(`{invalid`))
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})
}

func TestVerifyChannelSignatures(t *testing.T) {
	t.Parallel()
	_, priv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	kid, err := sign.Ed25519DIDKey(priv.Public().(ed25519.PublicKey))
	if err != nil {
		t.Fatal(err)
	}

	svc, err := New(testSeedHex, kid)
	if err != nil {
		t.Fatal(err)
	}

	t.Run("valid_signature", func(t *testing.T) {
		t.Parallel()
		ch := map[string]any{
			"title":     "Test Channel",
			"version":   "1.0.0",
			"playlists": []string{"https://example.com/pl/1"},
		}
		raw, _ := json.Marshal(ch)
		sig, _ := sign.SignMultiEd25519(raw, priv, "publisher", "2025-06-01T12:00:00Z")
		ch["signatures"] = []any{sig}
		signed, _ := json.Marshal(ch)

		ok, failed, err := svc.VerifyChannelSignatures(signed)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !ok {
			t.Fatalf("expected ok=true, got failed: %v", failed)
		}
	})

	t.Run("invalid_json", func(t *testing.T) {
		t.Parallel()
		_, _, err := svc.VerifyChannelSignatures([]byte(`{invalid`))
		if err == nil {
			t.Fatal("expected error for invalid JSON")
		}
	})
}

func TestEd25519PrivateKeyFromHex(t *testing.T) {
	t.Parallel()
	seed, err := hex.DecodeString(testSeedHex)
	if err != nil {
		t.Fatal(err)
	}
	want := ed25519.NewKeyFromSeed(seed)

	t.Run("32_byte_seed", func(t *testing.T) {
		t.Parallel()
		priv, err := Ed25519PrivateKeyFromHex(testSeedHex)
		if err != nil {
			t.Fatal(err)
		}
		if len(priv) != ed25519.PrivateKeySize {
			t.Fatalf("len %d", len(priv))
		}
		if string(priv) != string(want) {
			t.Fatal("private key mismatch")
		}
	})

	t.Run("64_byte_full_key", func(t *testing.T) {
		t.Parallel()
		full := hex.EncodeToString(want)
		priv, err := Ed25519PrivateKeyFromHex(full)
		if err != nil {
			t.Fatal(err)
		}
		if string(priv) != string(want) {
			t.Fatal("private key mismatch")
		}
	})

	t.Run("empty", func(t *testing.T) {
		t.Parallel()
		_, err := Ed25519PrivateKeyFromHex("")
		if err == nil || !strings.Contains(err.Error(), "signing key is require") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("bad_hex", func(t *testing.T) {
		t.Parallel()
		_, err := Ed25519PrivateKeyFromHex("gg")
		if err == nil || !strings.Contains(err.Error(), "signing key hex") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("wrong_length", func(t *testing.T) {
		t.Parallel()
		_, err := Ed25519PrivateKeyFromHex("abcd")
		if err == nil || !strings.Contains(err.Error(), "signing key must be") {
			t.Fatalf("got %v", err)
		}
	})
}

func TestNew(t *testing.T) {
	t.Parallel()
	t.Run("missing_kid", func(t *testing.T) {
		t.Parallel()
		_, err := New(testSeedHex, "")
		if err == nil || !strings.Contains(err.Error(), "signing kid is required") {
			t.Fatalf("got %v", err)
		}
	})

	t.Run("ok", func(t *testing.T) {
		t.Parallel()
		s, err := New(testSeedHex, "did:key:test")
		if err != nil {
			t.Fatal(err)
		}
		if s == nil {
			t.Fatal("nil service")
		}
	})
}

func TestService_ValidatePlaylist(t *testing.T) {
	t.Parallel()
	s, err := New(testSeedHex, "did:key:z6Mkw")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("invalid_schema", func(t *testing.T) {
		t.Parallel()
		_, err := s.ValidatePlaylist([]byte(`{"dpVersion":"1.1.0"}`))
		if err == nil {
			t.Fatal("expected validation error")
		}
		if !errors.Is(err, dp1.ErrValidation) {
			t.Fatalf("expected errors.Is(err, dp1.ErrValidation), got %v", err)
		}
	})

	t.Run("valid_minimal_signed", func(t *testing.T) {
		t.Parallel()
		raw := minimalSignedPlaylistV11(t)
		pl, err := s.ValidatePlaylist(raw)
		if err != nil {
			t.Fatal(err)
		}
		if pl == nil {
			t.Fatal("nil playlist")
		}
		if pl.Title != "Hello" {
			t.Fatalf("parsed title: %q", pl.Title)
		}
	})
}

func TestService_ValidatePlaylistWithExtension(t *testing.T) {
	t.Parallel()
	s, err := New(testSeedHex, "did:key:z6Mkw")
	if err != nil {
		t.Fatal(err)
	}

	t.Run("signed_core_ok", func(t *testing.T) {
		t.Parallel()
		raw := minimalSignedPlaylistV11(t)
		pl, err := s.ValidatePlaylistWithExtension(raw)
		if err != nil {
			t.Fatal(err)
		}
		if pl == nil {
			t.Fatal("nil playlist")
		}
		if pl.Title != "Hello" {
			t.Fatalf("parsed title: %q", pl.Title)
		}
	})
}

func TestService_SignPlaylist(t *testing.T) {
	t.Parallel()
	s, err := New(testSeedHex, "did:key:z6Mkw")
	if err != nil {
		t.Fatal(err)
	}

	pl := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "Signed",
		Items:     []playlist.PlaylistItem{{Source: "https://example.com/w"}},
	}
	raw, err := json.Marshal(pl)
	if err != nil {
		t.Fatal(err)
	}

	ts := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	signed, err := s.SignPlaylist(raw, ts)
	if err != nil {
		t.Fatal(err)
	}

	if _, err := s.ValidatePlaylist(signed); err != nil {
		t.Fatalf("signed doc should validate: %v", err)
	}

	var out playlist.Playlist
	if err := json.Unmarshal(signed, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Signatures) != 1 {
		t.Fatalf("signatures: %+v", out.Signatures)
	}
	if err := sign.VerifyMultiEd25519(signed, out.Signatures[0]); err != nil {
		t.Fatal(err)
	}
}

func TestService_SignPlaylist_preservesNonFeedSignatures(t *testing.T) {
	t.Parallel()
	s, err := New(testSeedHex, "did:key:z6Mkw")
	if err != nil {
		t.Fatal(err)
	}

	raw := minimalSignedPlaylistV11(t)
	ts := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	signed, err := s.SignPlaylist(raw, ts)
	if err != nil {
		t.Fatal(err)
	}

	ok, failed, err := s.VerifyPlaylistSignatures(signed)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatalf("expected all signatures valid, failed=%v", failed)
	}

	var out playlist.Playlist
	if err := json.Unmarshal(signed, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Signatures) != 2 {
		t.Fatalf("want 2 signatures (curator + feed), got %d: %+v", len(out.Signatures), out.Signatures)
	}

	// Re-signing replaces only the feed kid; curator entry remains.
	ts2 := ts.Add(time.Hour)
	signedAgain, err := s.SignPlaylist(signed, ts2)
	if err != nil {
		t.Fatal(err)
	}
	ok, failed, err = s.VerifyPlaylistSignatures(signedAgain)
	if err != nil {
		t.Fatalf("verify after re-sign: %v", err)
	}
	if !ok {
		t.Fatalf("expected all valid after re-sign, failed=%v", failed)
	}
	if err := json.Unmarshal(signedAgain, &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Signatures) != 2 {
		t.Fatalf("want 2 signatures after re-sign, got %d", len(out.Signatures))
	}
}

func TestService_SignPlaylistGroup_preservesNonFeedSignatures(t *testing.T) {
	t.Parallel()
	_, curatorPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(testSeedHex, "did:key:z6Mkw")
	if err != nil {
		t.Fatal(err)
	}

	pg := map[string]any{
		"dpVersion": "1.1.0",
		"title":     "G",
		"playlists": []string{"https://example.com/pl/1"},
	}
	raw, err := json.Marshal(pg)
	if err != nil {
		t.Fatal(err)
	}
	curSig, err := sign.SignMultiEd25519(raw, curatorPriv, "curator", "2025-06-01T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	pg["signatures"] = []playlist.Signature{curSig}
	raw, err = json.Marshal(pg)
	if err != nil {
		t.Fatal(err)
	}

	ts := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	signed, err := s.SignPlaylistGroup(raw, ts)
	if err != nil {
		t.Fatal(err)
	}
	ok, failed, err := s.VerifyPlaylistGroupSignatures(signed)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok, failed=%v", failed)
	}
	var env struct {
		Signatures []playlist.Signature `json:"signatures"`
	}
	if err := json.Unmarshal(signed, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Signatures) != 2 {
		t.Fatalf("want 2 signatures, got %d", len(env.Signatures))
	}
}

func TestService_SignChannel_preservesNonFeedSignatures(t *testing.T) {
	t.Parallel()
	_, pubPriv, err := ed25519.GenerateKey(nil)
	if err != nil {
		t.Fatal(err)
	}
	s, err := New(testSeedHex, "did:key:z6Mkw")
	if err != nil {
		t.Fatal(err)
	}

	ch := map[string]any{
		"title":     "C",
		"version":   "1.0.0",
		"playlists": []string{"https://example.com/pl/1"},
	}
	raw, err := json.Marshal(ch)
	if err != nil {
		t.Fatal(err)
	}
	pubSig, err := sign.SignMultiEd25519(raw, pubPriv, "publisher", "2025-06-01T12:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	ch["signatures"] = []playlist.Signature{pubSig}
	raw, err = json.Marshal(ch)
	if err != nil {
		t.Fatal(err)
	}

	ts := time.Date(2025, 3, 1, 12, 0, 0, 0, time.UTC)
	signed, err := s.SignChannel(raw, ts)
	if err != nil {
		t.Fatal(err)
	}
	ok, failed, err := s.VerifyChannelSignatures(signed)
	if err != nil {
		t.Fatalf("verify: %v", err)
	}
	if !ok {
		t.Fatalf("expected ok, failed=%v", failed)
	}
	var env struct {
		Signatures []playlist.Signature `json:"signatures"`
	}
	if err := json.Unmarshal(signed, &env); err != nil {
		t.Fatal(err)
	}
	if len(env.Signatures) != 2 {
		t.Fatalf("want 2 signatures, got %d", len(env.Signatures))
	}
}
