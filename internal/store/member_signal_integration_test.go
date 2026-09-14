//go:build integration

package store_test

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/display-protocol/dp1-go/extension/channels"
	"github.com/display-protocol/dp1-go/playlist"

	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// A member playlist's replace or delete must be observable at every channel that lists it, without the
// channel's own generation token moving (issue #25). Two store-level facts carry that: GetChannel's
// MembersDigest tracks member state, and the playlist writes report the channels that list the playlist.

// memberFixture creates n playlists and returns their ids, slugs and ingest inputs.
func memberFixture(t *testing.T, ctx context.Context, st store.Store, prefix string, n int) ([]uuid.UUID, []store.IngestedPlaylist) {
	t.Helper()
	ids := make([]uuid.UUID, n)
	members := make([]store.IngestedPlaylist, n)
	for i := range ids {
		// Fresh ids per test: deleted ids are tombstoned and the test-database cleanup truncates documents,
		// not tombstones, so a fixed id reused across tests would be refused as "deleted".
		ids[i] = uuid.New()
		slug := fmt.Sprintf("%s-pl-%d", prefix, i+1)
		pl := playlist.Playlist{DPVersion: "1.1.0", Title: slug, Items: []playlist.PlaylistItem{{ID: uuid.New().String(), Source: "https://" + slug}}}
		if err := st.CreatePlaylist(ctx, ids[i], slug, rawDoc(t, &pl)); err != nil {
			t.Fatal(err)
		}
		members[i] = store.IngestedPlaylist{ID: ids[i], Slug: slug, Raw: rawDoc(t, &pl)}
	}
	return ids, members
}

func memberChannel(t *testing.T, ctx context.Context, st store.Store, id uuid.UUID, slug string, order []store.IngestedPlaylist) {
	t.Helper()
	slugs := make([]string, len(order))
	for i, m := range order {
		slugs[i] = m.Slug
	}
	doc := channels.Channel{ID: id.String(), Slug: slug, Title: slug, Version: "1.0.0", Playlists: slugs}
	if err := st.CreateChannel(ctx, &store.ChannelInput{ID: id, Slug: slug, Raw: rawDoc(t, doc), Playlists: order}); err != nil {
		t.Fatalf("CreateChannel %s: %v", slug, err)
	}
}

func replacePlaylist(t *testing.T, ctx context.Context, st store.Store, id uuid.UUID, title string) []uuid.UUID {
	t.Helper()
	pl := playlist.Playlist{DPVersion: "1.1.0", Title: title, Items: []playlist.PlaylistItem{{ID: uuid.New().String(), Source: "https://" + title}}}
	listing, err := st.UpdatePlaylist(ctx, id.String(), rawDoc(t, &pl), plUpdatedAt(t, ctx, st, id.String()))
	if err != nil {
		t.Fatalf("UpdatePlaylist %s: %v", id, err)
	}
	return listing
}

func channelDigest(t *testing.T, ctx context.Context, st store.Store, idOrSlug string) string {
	t.Helper()
	rec, err := st.GetChannel(ctx, idOrSlug)
	if err != nil {
		t.Fatalf("GetChannel %s: %v", idOrSlug, err)
	}
	return rec.MembersDigest
}

func TestIntegration_GetChannel_membersDigestTracksMemberWrites(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	ids, members := memberFixture(t, ctx, st, "digest", 3)
	a, b, c := ids[0], ids[1], ids[2]
	chID := uuid.New()
	// A listed twice: the digest mirrors the membership rows, so a repeated member appears twice.
	memberChannel(t, ctx, st, chID, "digest-channel", []store.IngestedPlaylist{members[0], members[1], members[0]})

	rev0 := channelDigest(t, ctx, st, chID.String())
	if rev0 == "" || len(rev0) != 64 {
		t.Fatalf("digest of a channel with members = %q, want 64 hex characters", rev0)
	}
	// The digest is a function of stored state, so it must not depend on the lookup key or on which pool
	// connection ran the query (a timestamptz::text cast would).
	if bySlug := channelDigest(t, ctx, st, "digest-channel"); bySlug != rev0 {
		t.Fatalf("digest by slug %q != by id %q", bySlug, rev0)
	}
	updatedAt0 := chUpdatedAt(t, ctx, st, chID.String())

	// Editing a member changes the digest but not the channel's own generation token.
	replacePlaylist(t, ctx, st, a, "digest-pl-1-v2")
	rev1 := channelDigest(t, ctx, st, chID.String())
	if rev1 == rev0 {
		t.Fatal("digest unchanged after a member replace")
	}
	if got := chUpdatedAt(t, ctx, st, chID.String()); !got.Equal(updatedAt0) {
		t.Fatalf("channel updated_at moved on a member replace: %v -> %v", updatedAt0, got)
	}

	// Editing a playlist the channel does not list changes nothing.
	replacePlaylist(t, ctx, st, c, "digest-pl-3-v2")
	if rev := channelDigest(t, ctx, st, chID.String()); rev != rev1 {
		t.Fatalf("digest moved on a non-member replace: %q -> %q", rev1, rev)
	}

	// Deleting a member cascades its rows away, which also changes the digest and still not updated_at.
	if _, err := st.DeletePlaylist(ctx, b.String(), plUpdatedAt(t, ctx, st, b.String())); err != nil {
		t.Fatalf("DeletePlaylist: %v", err)
	}
	rev2 := channelDigest(t, ctx, st, chID.String())
	if rev2 == rev1 || rev2 == rev0 {
		t.Fatalf("digest after member delete = %q, want a value distinct from %q and %q", rev2, rev1, rev0)
	}
	if got := chUpdatedAt(t, ctx, st, chID.String()); !got.Equal(updatedAt0) {
		t.Fatalf("channel updated_at moved on a member delete: %v -> %v", updatedAt0, got)
	}

	// Sanity: replacing the channel itself changes the digest too (membership rows are rebuilt).
	if err := st.UpdateChannel(ctx, chID.String(), &store.ChannelInput{
		Raw:       rawDoc(t, channels.Channel{ID: chID.String(), Slug: "digest-channel", Title: "v2", Version: "1.0.0", Playlists: []string{members[0].Slug}}),
		Playlists: []store.IngestedPlaylist{members[0]},
	}, chUpdatedAt(t, ctx, st, chID.String())); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}
	if rev := channelDigest(t, ctx, st, chID.String()); rev == rev2 {
		t.Fatal("digest unchanged after the channel's membership was rebuilt")
	}
}

func TestIntegration_GetChannel_membersDigestEmptyWithoutMembers(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	_, members := memberFixture(t, ctx, st, "lonely", 1)
	chID := uuid.New()
	memberChannel(t, ctx, st, chID, "lonely-channel", members)
	if _, err := st.DeletePlaylist(ctx, members[0].ID.String(), plUpdatedAt(t, ctx, st, members[0].ID.String())); err != nil {
		t.Fatal(err)
	}
	// Every membership row is gone; the digest collapses to the documented empty value rather than a
	// hash of nothing, so a channel with no members is distinguishable from one whose members changed.
	if rev := channelDigest(t, ctx, st, chID.String()); rev != "" {
		t.Fatalf("digest with no membership rows = %q, want empty", rev)
	}
}

// Fixture shared by the two listing-channel tests: X lists A twice, Y lists A once, Z lists B; C is in
// no channel.
func listingFixture(t *testing.T, ctx context.Context, st store.Store) (ids []uuid.UUID, x, y, z uuid.UUID) {
	t.Helper()
	ids, members := memberFixture(t, ctx, st, "listing", 3)
	x = uuid.New()
	y = uuid.New()
	z = uuid.New()
	memberChannel(t, ctx, st, x, "listing-x", []store.IngestedPlaylist{members[0], members[0]})
	memberChannel(t, ctx, st, y, "listing-y", []store.IngestedPlaylist{members[0]})
	memberChannel(t, ctx, st, z, "listing-z", []store.IngestedPlaylist{members[1]})
	return ids, x, y, z
}

// sortedIDs orders ids the way channelsListingPlaylist does (ORDER BY channel_id: bytewise on the UUID).
func sortedIDs(ids ...uuid.UUID) []uuid.UUID {
	out := slices.Clone(ids)
	slices.SortFunc(out, func(a, b uuid.UUID) int { return slices.Compare(a[:], b[:]) })
	return out
}

func TestIntegration_UpdatePlaylist_returnsListingChannels(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	ids, x, y, z := listingFixture(t, ctx, st)
	a, b, c := ids[0], ids[1], ids[2]

	// A is listed by X (twice) and Y: each channel once, sorted, regardless of how many positions.
	if got, want := replacePlaylist(t, ctx, st, a, "a-v2"), sortedIDs(x, y); !slices.Equal(got, want) {
		t.Fatalf("listing channels for A = %v, want %v", got, want)
	}
	if got, want := replacePlaylist(t, ctx, st, b, "b-v2"), sortedIDs(z); !slices.Equal(got, want) {
		t.Fatalf("listing channels for B = %v, want %v", got, want)
	}
	if got := replacePlaylist(t, ctx, st, c, "c-v2"); len(got) != 0 {
		t.Fatalf("listing channels for unlisted C = %v, want none", got)
	}

	// A refused write reports nothing: the caller must not notify for a change that did not happen.
	pl := playlist.Playlist{DPVersion: "1.1.0", Title: "stale", Items: []playlist.PlaylistItem{{ID: uuid.New().String(), Source: "https://stale"}}}
	stale := plUpdatedAt(t, ctx, st, a.String()).Add(-time.Microsecond)
	listing, err := st.UpdatePlaylist(ctx, a.String(), rawDoc(t, &pl), stale)
	if !errors.Is(err, store.ErrConcurrentModification) || listing != nil {
		t.Fatalf("stale UpdatePlaylist = (%v, %v), want (nil, ErrConcurrentModification)", listing, err)
	}
}

func TestIntegration_DeletePlaylist_returnsListingChannels(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	ids, x, y, _ := listingFixture(t, ctx, st)
	a := ids[0]

	// A stale delete is refused, reports nothing, and leaves the row (and its membership) in place.
	stale := plUpdatedAt(t, ctx, st, a.String()).Add(-time.Microsecond)
	if listing, err := st.DeletePlaylist(ctx, a.String(), stale); !errors.Is(err, store.ErrConcurrentModification) || listing != nil {
		t.Fatalf("stale DeletePlaylist = (%v, %v), want (nil, ErrConcurrentModification)", listing, err)
	}
	if _, err := st.GetPlaylist(ctx, a.String()); err != nil {
		t.Fatalf("playlist should survive a refused delete: %v", err)
	}

	// The listing is captured before the cascade removes it: X and Y are reported, each once.
	listing, err := st.DeletePlaylist(ctx, a.String(), plUpdatedAt(t, ctx, st, a.String()))
	if err != nil {
		t.Fatalf("DeletePlaylist: %v", err)
	}
	if want := sortedIDs(x, y); !slices.Equal(listing, want) {
		t.Fatalf("listing channels for deleted A = %v, want %v", listing, want)
	}
	// ...and the membership rows are indeed gone: X's member list no longer serves A.
	inX, _, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{Limit: 10, Sort: store.SortAsc, ChannelFilter: x.String()})
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range inX {
		if rec.ID == a {
			t.Fatal("deleted playlist still listed under channel X")
		}
	}
}

// The membership cursor is bound to the channel's updated_at. A member edit must leave that alone, so a
// client paging a channel while one of its playlists is replaced or deleted keeps its cursor — contrast
// TestIntegration_ListPlaylists_membershipCursorRefusedAfterReplace, where the channel itself changes.
func TestIntegration_ListPlaylists_membershipCursorSurvivesMemberEdit(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	ids, members := memberFixture(t, ctx, st, "cursor", 4)
	chID := uuid.New()
	memberChannel(t, ctx, st, chID, "cursor-channel", members)

	page1, cur, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{Limit: 2, Sort: store.SortAsc, ChannelFilter: chID.String()})
	if err != nil || cur == "" || len(page1) != 2 || page1[0].ID != ids[0] || page1[1].ID != ids[1] {
		t.Fatalf("page 1: %d rows, cursor %q, err %v", len(page1), cur, err)
	}

	replacePlaylist(t, ctx, st, ids[0], "cursor-pl-1-v2")
	page2, cur2, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{Limit: 2, Cursor: cur, Sort: store.SortAsc, ChannelFilter: chID.String()})
	if err != nil || cur2 != "" || len(page2) != 2 || page2[0].ID != ids[2] || page2[1].ID != ids[3] {
		t.Fatalf("page 2 after a member replace: %d rows, cursor %q, err %v", len(page2), cur2, err)
	}

	// Deleting a member on a later page shortens the order but does not invalidate the token; the client
	// simply sees the remaining rows.
	if _, err := st.DeletePlaylist(ctx, ids[2].String(), plUpdatedAt(t, ctx, st, ids[2].String())); err != nil {
		t.Fatal(err)
	}
	page2b, _, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{Limit: 2, Cursor: cur, Sort: store.SortAsc, ChannelFilter: chID.String()})
	if err != nil || len(page2b) != 1 || page2b[0].ID != ids[3] {
		t.Fatalf("page 2 after a member delete: %d rows, err %v", len(page2b), err)
	}
}
