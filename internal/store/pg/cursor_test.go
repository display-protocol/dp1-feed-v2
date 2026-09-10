package pg

import (
	"encoding/base64"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
)

// The two list orderings issue tokens with disjoint key sets, and each decoder must refuse the other's
// token: decoding a membership token as a (created_at, id) tuple would yield zero values and quietly
// restart the created_at list, which the client could not distinguish from a legitimate first page.
func TestCursorDecoders_rejectEachOthersTokens(t *testing.T) {
	t.Parallel()
	createdTok := encodeCursor(time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC), uuid.MustParse("11111111-1111-1111-1111-111111111111"))
	memberTok := encodeMembershipCursor(38)

	if _, _, err := decodeCursor(memberTok); err == nil || !strings.Contains(err.Error(), "filtered") {
		t.Fatalf("decodeCursor(membership token): want refusal naming the filtered list, got %v", err)
	}
	if _, err := decodeMembershipCursor(createdTok); err == nil || !strings.Contains(err.Error(), "unfiltered") {
		t.Fatalf("decodeMembershipCursor(created_at token): want refusal naming the unfiltered list, got %v", err)
	}
}

func TestCursorDecoders_roundTrip(t *testing.T) {
	t.Parallel()
	wantT := time.Date(2026, 9, 9, 10, 29, 14, 0, time.UTC)
	wantID := uuid.MustParse("b3ca1b81-15f5-4a88-b9c1-501366c3e807")
	gotT, gotID, err := decodeCursor(encodeCursor(wantT, wantID))
	if err != nil || !gotT.Equal(wantT) || gotID != wantID {
		t.Fatalf("created_at cursor round trip: got (%v, %v, %v)", gotT, gotID, err)
	}
	pos, err := decodeMembershipCursor(encodeMembershipCursor(38))
	if err != nil || pos != 38 {
		t.Fatalf("membership cursor round trip: got (%d, %v)", pos, err)
	}
}

// A field that is absent or null decodes to its zero value, and a zero created_at / Nil id / zero or
// negative position would be accepted by the keyset query as a real boundary. Every such token must be
// refused, for all three cursor shapes, so a 400 rather than a silently wrong page is what the client sees.
func TestCursorDecoders_rejectIncompleteFields(t *testing.T) {
	t.Parallel()
	tok := func(j string) string { return base64.RawURLEncoding.EncodeToString([]byte(j)) }
	const goodT = `"2026-09-09T10:00:00Z"`
	const goodID = `"11111111-1111-1111-1111-111111111111"`

	for name, j := range map[string]string{
		"missing id":  `{"t":` + goodT + `}`,
		"null id":     `{"t":` + goodT + `,"id":null}`,
		"nil id":      `{"t":` + goodT + `,"id":"00000000-0000-0000-0000-000000000000"}`,
		"null t":      `{"t":null,"id":` + goodID + `}`,
		"zero t":      `{"t":"0001-01-01T00:00:00Z","id":` + goodID + `}`,
		"bad t type":  `{"t":5,"id":` + goodID + `}`,
		"bad id type": `{"t":` + goodT + `,"id":5}`,
	} {
		if _, _, err := decodeCursor(tok(j)); err == nil {
			t.Errorf("decodeCursor(%s): want error", name)
		}
	}
	for name, j := range map[string]string{
		"null pos":     `{"pos":null}`,
		"negative pos": `{"pos":-1}`,
		"bad pos type": `{"pos":"3"}`,
		"empty object": `{}`,
	} {
		if _, err := decodeMembershipCursor(tok(j)); err == nil {
			t.Errorf("decodeMembershipCursor(%s): want error", name)
		}
	}
	if pos, err := decodeMembershipCursor(tok(`{"pos":0}`)); err != nil || pos != 0 {
		t.Errorf("decodeMembershipCursor(pos 0): position 0 is the first member and must be accepted, got (%d, %v)", pos, err)
	}
	for name, j := range map[string]string{
		"missing t":    `{"pos":1,"iid":` + goodID + `}`,
		"null t":       `{"t":null,"pos":1,"iid":` + goodID + `}`,
		"zero t":       `{"t":"0001-01-01T00:00:00Z","pos":1,"iid":` + goodID + `}`,
		"missing pos":  `{"t":` + goodT + `,"iid":` + goodID + `}`,
		"null pos":     `{"t":` + goodT + `,"pos":null,"iid":` + goodID + `}`,
		"negative pos": `{"t":` + goodT + `,"pos":-1,"iid":` + goodID + `}`,
		"missing iid":  `{"t":` + goodT + `,"pos":1}`,
		"null iid":     `{"t":` + goodT + `,"pos":1,"iid":null}`,
		"nil iid":      `{"t":` + goodT + `,"pos":1,"iid":"00000000-0000-0000-0000-000000000000"}`,
	} {
		if _, _, _, err := decodePlaylistItemCursor(tok(j)); err == nil {
			t.Errorf("decodePlaylistItemCursor(%s): want error", name)
		}
	}
	if pt, pos, iid, err := decodePlaylistItemCursor(encodePlaylistItemCursor(time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC), 0, uuid.MustParse("11111111-1111-1111-1111-111111111111"))); err != nil || pos != 0 || pt.IsZero() || iid == uuid.Nil {
		t.Errorf("decodePlaylistItemCursor(valid, pos 0): got (%v, %d, %v, %v)", pt, pos, iid, err)
	}
}

func TestCursorDecoders_rejectMalformed(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"not base64":   "%%%",
		"not json":     base64.RawURLEncoding.EncodeToString([]byte("nope")),
		"unknown keys": base64.RawURLEncoding.EncodeToString([]byte(`{"x":1}`)),
		// A playlist-item token carries both "t" and "pos"; each decoder must refuse it by the key the
		// other ordering owns, not accept it by the key it recognizes.
		"playlist-item token": encodePlaylistItemCursor(time.Date(2026, 9, 9, 10, 0, 0, 0, time.UTC), 3, uuid.MustParse("22222222-2222-2222-2222-222222222222")),
	}
	for name, tok := range cases {
		if _, _, err := decodeCursor(tok); err == nil {
			t.Errorf("decodeCursor(%s): want error", name)
		}
		if _, err := decodeMembershipCursor(tok); err == nil {
			t.Errorf("decodeMembershipCursor(%s): want error", name)
		}
	}
}
