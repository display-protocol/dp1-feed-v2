//go:build integration

package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/display-protocol/dp1-go/extension/channels"
	"github.com/display-protocol/dp1-go/playlistgroup"
	"github.com/google/uuid"

	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// Playlist visibility (playlists extension §3.7) is a listing request inside the signed document: an
// owner who marks a playlist "unlisted" is asking every directory to leave it out, while its own address
// keeps serving it. These tests pin which store reads are directories (they must fail closed on any value
// but an absent or exact "public"), which are lookups by address (they keep working), and which are
// container listings (they still name the members the container's own signed document names).

const visCurator = "did:key:zVisibilityCurator"

// visibilityDoc is a stored playlist body with the given visibility member. present=false omits the
// member entirely; value is written as JSON, so json.RawMessage("null") is a present null.
func visibilityDoc(t *testing.T, id, slug, itemID, itemTitle string, present bool, value json.RawMessage) json.RawMessage {
	t.Helper()
	doc := map[string]any{
		"dpVersion": "1.1.0",
		"id":        id,
		"slug":      slug,
		"title":     slug,
		"curators":  []map[string]string{{"name": "Curator", "key": visCurator}},
		"items":     []map[string]string{{"id": itemID, "title": itemTitle, "source": "https://cdn.example.com/" + slug + ".html"}},
	}
	if present {
		doc["visibility"] = value
	}
	return rawDoc(t, doc)
}

func createVisibilityPlaylist(t *testing.T, ctx context.Context, st store.Store, id, slug, itemID string, present bool, value json.RawMessage) {
	t.Helper()
	if err := st.CreatePlaylist(ctx, uuid.MustParse(id), slug, visibilityDoc(t, id, slug, itemID, slug, present, value)); err != nil {
		t.Fatalf("create playlist %s: %v", slug, err)
	}
	// created_at is the page key; keep rows distinct so the order under test is deterministic.
	time.Sleep(2 * time.Millisecond)
}

func itemTitles(recs []store.PlaylistItemRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Item.Title)
	}
	return out
}

// visibilityFixture is one row per way a document can state (or fail to state) its visibility. Only the
// first two are listed: everything else fails closed, including values a later spec version might define.
var visibilityFixture = []struct {
	id, slug, itemID string
	present          bool
	value            json.RawMessage
	listed           bool
}{
	{"b1000000-0000-4000-8000-000000000001", "vis-absent", "c1000000-0000-4000-8000-000000000001", false, nil, true},
	{"b1000000-0000-4000-8000-000000000002", "vis-public", "c1000000-0000-4000-8000-000000000002", true, json.RawMessage(`"public"`), true},
	{"b1000000-0000-4000-8000-000000000003", "vis-unlisted", "c1000000-0000-4000-8000-000000000003", true, json.RawMessage(`"unlisted"`), false},
	{"b1000000-0000-4000-8000-000000000004", "vis-future-value", "c1000000-0000-4000-8000-000000000004", true, json.RawMessage(`"followers"`), false},
	{"b1000000-0000-4000-8000-000000000005", "vis-case-variant", "c1000000-0000-4000-8000-000000000005", true, json.RawMessage(`"Public"`), false},
	{"b1000000-0000-4000-8000-000000000006", "vis-empty", "c1000000-0000-4000-8000-000000000006", true, json.RawMessage(`""`), false},
	{"b1000000-0000-4000-8000-000000000007", "vis-null", "c1000000-0000-4000-8000-000000000007", true, json.RawMessage(`null`), false},
}

func seedVisibilityFixture(t *testing.T, ctx context.Context, st store.Store) {
	t.Helper()
	for _, f := range visibilityFixture {
		createVisibilityPlaylist(t, ctx, st, f.id, f.slug, f.itemID, f.present, f.value)
	}
}

func TestIntegration_Visibility_directoriesListOnlyAbsentOrPublic(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seedVisibilityFixture(t, ctx, st)

	want := []string{"vis-absent", "vis-public"}
	for _, tc := range []struct {
		name string
		p    store.ListPlaylistsParams
		want []string
	}{
		{"unfiltered", store.ListPlaylistsParams{Limit: 100, Sort: store.SortAsc}, want},
		{"unfiltered desc", store.ListPlaylistsParams{Limit: 100, Sort: store.SortDesc}, []string{"vis-public", "vis-absent"}},
		// A curator filter is still a directory: one shared link must not hand out a list of the same
		// person's other unlisted playlists.
		{"curator filter", store.ListPlaylistsParams{Limit: 100, Sort: store.SortAsc, CuratorFilter: visCurator}, want},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, next, err := st.ListPlaylists(ctx, &tc.p)
			if err != nil {
				t.Fatal(err)
			}
			if next != "" {
				t.Fatalf("unexpected next cursor %q", next)
			}
			assertDeepEqual(t, "slugs", tc.want, playlistSlugs(got))
		})
	}

	t.Run("paging never surfaces an unlisted row", func(t *testing.T) {
		var all []string
		cursor := ""
		for i := 0; i < 10; i++ {
			page, next, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{Limit: 1, Sort: store.SortAsc, Cursor: cursor})
			if err != nil {
				t.Fatal(err)
			}
			all = append(all, playlistSlugs(page)...)
			if next == "" {
				break
			}
			cursor = next
		}
		assertDeepEqual(t, "paged slugs", want, all)
	})

	t.Run("cross-playlist item list", func(t *testing.T) {
		got, next, err := st.ListPlaylistItems(ctx, &store.ListPlaylistItemsParams{Limit: 100, Sort: store.SortAsc})
		if err != nil {
			t.Fatal(err)
		}
		if next != "" {
			t.Fatalf("unexpected next cursor %q", next)
		}
		assertDeepEqual(t, "item titles", want, itemTitles(got))
	})
}

func TestIntegration_Visibility_addressLookups(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	seedVisibilityFixture(t, ctx, st)

	for _, f := range visibilityFixture {
		t.Run(f.slug, func(t *testing.T) {
			// The id is the address an owner hands out: it serves every playlist, listed or not, and the
			// stored bytes keep the member exactly as signed.
			rec, err := st.GetPlaylist(ctx, f.id)
			if err != nil {
				t.Fatalf("by id: %v", err)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(rec.Raw, &body); err != nil {
				t.Fatal(err)
			}
			got, present := body["visibility"]
			if present != f.present || (present && string(got) != string(f.value)) {
				t.Fatalf("stored visibility = %s (present %v), want %s (present %v)", got, present, f.value, f.present)
			}

			items, err := st.GetPlaylistItems(ctx, f.id)
			if err != nil {
				t.Fatalf("items by id: %v", err)
			}
			if len(items) != 1 {
				t.Fatalf("items by id: len=%d want 1", len(items))
			}

			// A slug is derived from the title and is far easier to guess than a random id, so it must not
			// reach a playlist the owner kept out of directories.
			_, err = st.GetPlaylist(ctx, f.slug)
			if f.listed && err != nil {
				t.Fatalf("by slug: %v", err)
			}
			if !f.listed && !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("by slug: want ErrNotFound, got %v", err)
			}
			items, err = st.GetPlaylistItems(ctx, f.slug)
			if err != nil {
				t.Fatalf("items by slug: %v", err)
			}
			wantItems := 0
			if f.listed {
				wantItems = 1
			}
			if len(items) != wantItems {
				t.Fatalf("items by slug: len=%d want %d", len(items), wantItems)
			}

			// An item lookup is a reverse index across playlists; it answers only from listed ones.
			_, err = st.GetPlaylistItem(ctx, uuid.MustParse(f.itemID))
			if f.listed && err != nil {
				t.Fatalf("item by id: %v", err)
			}
			if !f.listed && !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("item by id: want ErrNotFound, got %v", err)
			}
		})
	}

	t.Run("an unlisted playlist still holds its slug", func(t *testing.T) {
		const id = "b1000000-0000-4000-8000-0000000000ff"
		err := st.CreatePlaylist(ctx, uuid.MustParse(id), "vis-unlisted", visibilityDoc(t, id, "vis-unlisted", "c1000000-0000-4000-8000-0000000000ff", "x", false, nil))
		if !errors.Is(err, store.ErrAlreadyExists) {
			t.Fatalf("want ErrAlreadyExists, got %v", err)
		}
	})
}

// An item id can appear in more than one playlist (a work copied from a public playlist into a private
// one keeps its id). The reverse lookup must answer with the listed copy, never the unlisted one, whose
// title or note may say something the owner did not publish.
func TestIntegration_Visibility_itemLookupPrefersListedCopy(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	const shared = "c2000000-0000-4000-8000-000000000001"

	for _, p := range []struct {
		id, slug, title string
		present         bool
		value           json.RawMessage
	}{
		{"b2000000-0000-4000-8000-000000000001", "copy-unlisted-a", "unlisted copy", true, json.RawMessage(`"unlisted"`)},
		{"b2000000-0000-4000-8000-000000000002", "copy-public", "public copy", false, nil},
		{"b2000000-0000-4000-8000-000000000003", "copy-unlisted-b", "unlisted copy", true, json.RawMessage(`"unlisted"`)},
	} {
		if err := st.CreatePlaylist(ctx, uuid.MustParse(p.id), p.slug, visibilityDoc(t, p.id, p.slug, shared, p.title, p.present, p.value)); err != nil {
			t.Fatalf("create %s: %v", p.slug, err)
		}
	}
	rec, err := st.GetPlaylistItem(ctx, uuid.MustParse(shared))
	if err != nil {
		t.Fatal(err)
	}
	if rec.Item.Title != "public copy" {
		t.Fatalf("item lookup answered from %q, want the listed copy", rec.Item.Title)
	}
}

func TestIntegration_Visibility_containersListTheMembersTheyName(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	const (
		listedID   = "b3000000-0000-4000-8000-000000000001"
		unlistedID = "b3000000-0000-4000-8000-000000000002"
		ingestedID = "b3000000-0000-4000-8000-000000000003"
	)
	createVisibilityPlaylist(t, ctx, st, listedID, "member-listed", "c3000000-0000-4000-8000-000000000001", false, nil)
	createVisibilityPlaylist(t, ctx, st, unlistedID, "member-unlisted", "c3000000-0000-4000-8000-000000000002", true, json.RawMessage(`"unlisted"`))
	stored := func(id string) json.RawMessage {
		rec, err := st.GetPlaylist(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		return rec.Raw
	}

	// A member the feed first learns about through a container (a remote reference) is upserted from its
	// own signed bytes, so its visibility applies the moment it is ingested.
	ingested := visibilityDoc(t, ingestedID, "member-ingested", "c3000000-0000-4000-8000-000000000003", "member-ingested", true, json.RawMessage(`"unlisted"`))
	members := []store.IngestedPlaylist{
		{ID: uuid.MustParse(unlistedID), Slug: "member-unlisted", Raw: stored(unlistedID)},
		{ID: uuid.MustParse(listedID), Slug: "member-listed", Raw: stored(listedID)},
		{ID: uuid.MustParse(ingestedID), Slug: "member-ingested", Raw: ingested, SourceURI: "https://other.example/api/v1/playlists/" + ingestedID},
	}
	uris := []string{"u1", "u2", "u3"}

	chID := uuid.MustParse("d3000000-0000-4000-8000-000000000001")
	if err := st.CreateChannel(ctx, &store.ChannelInput{
		ID: chID, Slug: "vis-channel",
		Raw:       rawDoc(t, channels.Channel{ID: chID.String(), Slug: "vis-channel", Title: "Channel", Playlists: uris, Created: "2026-01-01T00:00:00Z"}),
		Playlists: members,
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}
	grpID := uuid.MustParse("d3000000-0000-4000-8000-000000000002")
	if err := st.CreatePlaylistGroup(ctx, &store.PlaylistGroupInput{
		ID: grpID, Slug: "vis-group",
		Raw:       rawDoc(t, playlistgroup.Group{ID: grpID.String(), Slug: "vis-group", Title: "Group", Playlists: uris, Created: "2026-01-01T00:00:00Z"}),
		Playlists: members,
	}); err != nil {
		t.Fatalf("CreatePlaylistGroup: %v", err)
	}

	wantMembers := []string{"member-unlisted", "member-listed", "member-ingested"}
	for _, tc := range []struct {
		name string
		p    store.ListPlaylistsParams
	}{
		{"channel filter", store.ListPlaylistsParams{Limit: 100, ChannelFilter: "vis-channel"}},
		{"playlist-group filter", store.ListPlaylistsParams{Limit: 100, PlaylistGroupFilter: "vis-group"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := st.ListPlaylists(ctx, &tc.p)
			if err != nil {
				t.Fatal(err)
			}
			assertDeepEqual(t, "members", wantMembers, playlistSlugs(got))
		})
	}

	t.Run("container-scoped item list", func(t *testing.T) {
		got, _, err := st.ListPlaylistItems(ctx, &store.ListPlaylistItemsParams{Limit: 100, ChannelFilter: "vis-channel"})
		if err != nil {
			t.Fatal(err)
		}
		if len(got) != 3 {
			t.Fatalf("channel items: len=%d want 3 (%v)", len(got), itemTitles(got))
		}
	})

	t.Run("membership reads", func(t *testing.T) {
		inChannel, err := st.ListPlaylistsInChannel(ctx, "vis-channel")
		if err != nil {
			t.Fatal(err)
		}
		assertDeepEqual(t, "channel members", wantMembers, playlistSlugs(inChannel))
		inGroup, err := st.ListPlaylistsInGroup(ctx, "vis-group")
		if err != nil {
			t.Fatal(err)
		}
		assertDeepEqual(t, "group members", wantMembers, playlistSlugs(inGroup))
	})

	t.Run("directories still leave the members out", func(t *testing.T) {
		got, _, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		assertDeepEqual(t, "directory", []string{"member-listed"}, playlistSlugs(got))
	})
}

// Visibility is part of the document, so it follows every replace: an owner can withdraw a playlist from
// directories and bring it back, and the item index follows the playlist row.
func TestIntegration_Visibility_followsReplace(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	const (
		id     = "b4000000-0000-4000-8000-000000000001"
		itemID = "c4000000-0000-4000-8000-000000000001"
	)
	createVisibilityPlaylist(t, ctx, st, id, "toggle", itemID, false, nil)

	listed := func() bool {
		t.Helper()
		got, _, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		items, _, err := st.ListPlaylistItems(ctx, &store.ListPlaylistItemsParams{Limit: 100})
		if err != nil {
			t.Fatal(err)
		}
		if (len(got) == 1) != (len(items) == 1) {
			t.Fatalf("playlist list (%d) and item list (%d) disagree", len(got), len(items))
		}
		return len(got) == 1
	}
	if !listed() {
		t.Fatal("absent visibility: want listed")
	}

	for _, step := range []struct {
		present bool
		value   json.RawMessage
		want    bool
	}{
		{true, json.RawMessage(`"unlisted"`), false},
		{true, json.RawMessage(`"public"`), true},
		{true, json.RawMessage(`"unlisted"`), false},
		{false, nil, true},
	} {
		raw := visibilityDoc(t, id, "toggle", itemID, "toggle", step.present, step.value)
		if _, err := st.UpdatePlaylist(ctx, id, raw, plUpdatedAt(t, ctx, st, id), false); err != nil {
			t.Fatalf("UpdatePlaylist(%s): %v", step.value, err)
		}
		if got := listed(); got != step.want {
			t.Fatalf("after replace with visibility %s (present %v): listed=%v want %v", step.value, step.present, got, step.want)
		}
	}
}
