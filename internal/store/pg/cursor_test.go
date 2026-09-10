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

func TestCursorDecoders_rejectMalformed(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"not base64":   "%%%",
		"not json":     base64.RawURLEncoding.EncodeToString([]byte("nope")),
		"unknown keys": base64.RawURLEncoding.EncodeToString([]byte(`{"x":1}`)),
		// A playlist-item token carries both "t" and "pos"; each decoder must refuse it by the key the
		// other ordering owns, not accept it by the key it recognizes.
		"playlist-item token": encodePlaylistItemCursor(time.Unix(0, 0), 3, uuid.Nil),
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
