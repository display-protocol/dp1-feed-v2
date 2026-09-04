//go:build integration

// Package store_test holds integration contract tests for [store.Store].
//
// These tests verify the [store.Store] contract and work with any backend implementing [store.TestProvider].
// They import only [store] and a concrete provider (e.g. pgtest); changing backends requires
// only updating the TestMain provider initialization.
//
// A single test database runs for the entire suite. Each test truncates tables after completion
// to reset state for the next test.
//
// Docker is required for the default PostgreSQL provider. From the repository root:
//
//	go test -tags=integration -count=1 -v ./internal/store/
package store_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/display-protocol/dp1-go/extension/channels"
	"github.com/display-protocol/dp1-go/extension/identity"
	"github.com/display-protocol/dp1-go/playlist"
	"github.com/display-protocol/dp1-go/playlistgroup"
	"github.com/google/uuid"

	"github.com/display-protocol/dp1-feed-v2/internal/store"
	"github.com/display-protocol/dp1-feed-v2/internal/store/pg/pgtest"
)

// testProvider is the database provider for all tests in this package.
// Concrete implementation (pgtest) can be swapped for other backends without changing tests.
var testProvider store.TestProvider

// TestMain starts the test database provider, runs all tests, then tears down.
func TestMain(m *testing.M) {
	ctx := context.Background()
	var err error
	testProvider, err = pgtest.NewProvider(ctx)
	if err != nil {
		// Fail loudly. This used to exit 0 "if provider setup fails (e.g. no Docker)", which reported a
		// green package with ZERO tests run — a broken migration or an unavailable database was
		// indistinguishable from a pass, locally and in CI. These tests only build under -tags=integration,
		// so reaching here means integration tests were explicitly asked for and could not run.
		fmt.Fprintf(os.Stderr, "integration provider setup failed: %v\n", err)
		os.Exit(1)
	}
	defer testProvider.Close()
	code := m.Run()
	os.Exit(code)
}

// newStore returns a store from the test provider and registers cleanup to reset state.
func newStore(t *testing.T) store.Store {
	t.Helper()
	t.Cleanup(func() {
		testProvider.Cleanup(t)
	})
	return testProvider.NewStore()
}

// jsonEqual compares two JSON documents by value, not bytes.
//
// Byte comparison would be wrong here: the jsonb column re-orders keys and normalizes numeric text on the
// way in. That is JCS-neutral (signatures still verify), but it means stored bytes are not the submitted
// bytes, so "unchanged document" has to be asserted semantically.
func jsonEqual(t *testing.T, a, b []byte) bool {
	t.Helper()
	var av, bv any
	if err := json.Unmarshal(a, &av); err != nil {
		t.Fatalf("unmarshal a: %v", err)
	}
	if err := json.Unmarshal(b, &bv); err != nil {
		t.Fatalf("unmarshal b: %v", err)
	}
	return reflect.DeepEqual(av, bv)
}

// assertDeepEqual fails the test if want and got differ. It uses [reflect.DeepEqual],
// which compares struct fields, slice elements, and map keys/values recursively.
func assertDeepEqual[T any](t *testing.T, label string, want, got T) {
	t.Helper()
	if !reflect.DeepEqual(want, got) {
		t.Fatalf("%s: not equal\nwant: %+v\ngot:  %+v", label, want, got)
	}
}

func TestIntegration_Ping(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()
	// Expect: pool can reach Postgres (migrations applied, connection OK).
	if err := st.Ping(ctx); err != nil {
		t.Fatal(err)
	}
}

// =============================================================================
// Playlist Contract Tests
// =============================================================================

func TestIntegration_PlaylistCRUD_and_List(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	id := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	slug := "contract-test-slug"
	itemID := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	pl := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "T",
		Items: []playlist.PlaylistItem{
			{ID: itemID.String(), Source: "https://x"},
		},
	}

	// Insert row + build playlist_item_index from body.items.
	if err := st.CreatePlaylist(ctx, id, slug, rawDoc(t, &pl)); err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}

	// Load by UUID: slug and JSON body match what we stored.
	byID, err := st.GetPlaylist(ctx, id.String())
	if err != nil {
		t.Fatal(err)
	}
	if byID.Slug != slug {
		t.Fatalf("Get by id slug: %+v", byID)
	}
	assertDeepEqual(t, "Get by id body", pl, byID.Body)

	// Load by slug resolves to the same row as by id.
	bySlug, err := st.GetPlaylist(ctx, slug)
	if err != nil {
		t.Fatal(err)
	}
	if bySlug.ID != id {
		t.Fatalf("Get by slug id %v", bySlug.ID)
	}

	// Index row count, item id, position, and item JSON mirror items[0].
	items, err := st.GetPlaylistItems(ctx, id.String())
	if err != nil {
		t.Fatalf("GetPlaylistItems after create: %v", err)
	}
	if len(items) != 1 {
		t.Fatalf("expected 1 item after create, got %d", len(items))
	}
	if items[0].ItemID != itemID {
		t.Fatalf("wrong item_id: %v", items[0].ItemID)
	}
	if items[0].Position != 0 {
		t.Fatalf("expected position 0, got %d", items[0].Position)
	}
	wantItem := playlist.PlaylistItem{ID: itemID.String(), Source: "https://x"}
	assertDeepEqual(t, "item body after create", wantItem, items[0].Item)

	// Replace body and rebuild index (old items replaced, not merged).
	updatedItemID := uuid.MustParse("ffffffff-ffff-ffff-ffff-ffffffffffff")
	plUpdated := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "Updated",
		Items: []playlist.PlaylistItem{
			{ID: updatedItemID.String(), Source: "https://y"},
		},
	}
	if err := st.UpdatePlaylist(ctx, slug, rawDoc(t, &plUpdated), plUpdatedAt(t, ctx, st, slug)); err != nil {
		t.Fatalf("UpdatePlaylist: %v", err)
	}
	after, _ := st.GetPlaylist(ctx, id.String())
	assertDeepEqual(t, "after UpdatePlaylist", plUpdated, after.Body)

	// Index reflects the new single item after update.
	itemsAfterUpdate, err := st.GetPlaylistItems(ctx, slug)
	if err != nil {
		t.Fatalf("GetPlaylistItems after update: %v", err)
	}
	if len(itemsAfterUpdate) != 1 {
		t.Fatalf("expected 1 item after update, got %d", len(itemsAfterUpdate))
	}
	if itemsAfterUpdate[0].ItemID != updatedItemID {
		t.Fatalf("item_id after update: %v", itemsAfterUpdate[0].ItemID)
	}
	wantAfterItem := playlist.PlaylistItem{ID: updatedItemID.String(), Source: "https://y"}
	assertDeepEqual(t, "item body after update", wantAfterItem, itemsAfterUpdate[0].Item)

	// List includes our playlist with default asc sort (single row in fresh DB).
	recs, _, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{
		Limit:  10,
		Cursor: "",
		Sort:   store.SortAsc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("list len %d", len(recs))
	}

	// Delete removes row; subsequent get returns ErrNotFound.
	if err := st.DeletePlaylist(ctx, id.String(), plUpdatedAt(t, ctx, st, id.String())); err != nil {
		t.Fatal(err)
	}
	_, err = st.GetPlaylist(ctx, id.String())
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}
}

func TestIntegration_GetPlaylist_notFound(t *testing.T) {
	st := newStore(t)
	// Random UUID: no row → ErrNotFound (not a generic driver error).
	_, err := st.GetPlaylist(context.Background(), uuid.New().String())
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestIntegration_ListPlaylists_limitExceeded(t *testing.T) {
	st := newStore(t)
	// Limit above store cap → ErrListLimitExceeded without hitting the DB page query.
	_, _, err := st.ListPlaylists(context.Background(), &store.ListPlaylistsParams{
		Limit:  store.StoreMaxListLimit + 1,
		Sort:   store.SortAsc,
		Cursor: "",
	})
	if err == nil || !errors.Is(err, store.ErrListLimitExceeded) {
		t.Fatalf("got %v", err)
	}
}

func TestIntegration_ListPlaylists_paginationCursor(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	id1 := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	id2 := uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc")
	item1 := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddddd")
	item2 := uuid.MustParse("eeeeeeee-eeee-eeee-eeee-eeeeeeeeeeee")
	pl1 := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "First",
		Items:     []playlist.PlaylistItem{{ID: item1.String(), Source: "https://a"}},
	}
	pl2 := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "Second",
		Items:     []playlist.PlaylistItem{{ID: item2.String(), Source: "https://b"}},
	}
	if err := st.CreatePlaylist(ctx, id1, "p-first", rawDoc(t, &pl1)); err != nil {
		t.Fatal(err)
	}
	// Stagger created_at so asc sort order is deterministic (both rows distinct in time).
	time.Sleep(5 * time.Millisecond)
	if err := st.CreatePlaylist(ctx, id2, "p-second", rawDoc(t, &pl2)); err != nil {
		t.Fatal(err)
	}

	// First page: one row, non-empty next cursor.
	page1, cur, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{
		Limit:  1,
		Cursor: "",
		Sort:   store.SortAsc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 1 || cur == "" {
		t.Fatalf("page1 len=%d cur=%q", len(page1), cur)
	}

	// Second page: other row, empty cursor (no third page).
	page2, cur2, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{
		Limit:  1,
		Cursor: cur,
		Sort:   store.SortAsc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 {
		t.Fatalf("page2 len %d", len(page2))
	}
	// Cursor advanced: pages must not return the same playlist body.
	if reflect.DeepEqual(page1[0].Body, page2[0].Body) {
		t.Fatal("expected different rows across pages")
	}
	if cur2 != "" {
		t.Fatalf("expected empty next cursor on last page, got %q", cur2)
	}
}

func TestIntegration_ListPlaylists_filterByPlaylistGroupAndChannel(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	plID1 := uuid.MustParse("f1f1f1f1-f1f1-f1f1-f1f1-f1f1f1f1f1f1")
	plID2 := uuid.MustParse("f2f2f2f2-f2f2-f2f2-f2f2-f2f2f2f2f2f2")
	item1 := uuid.MustParse("a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1")
	item2 := uuid.MustParse("b2b2b2b2-b2b2-b2b2-b2b2-b2b2b2b2b2b2")
	pl1 := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "Filter P1",
		Items:     []playlist.PlaylistItem{{ID: item1.String(), Source: "https://fp1"}},
	}
	pl2 := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "Filter P2",
		Items:     []playlist.PlaylistItem{{ID: item2.String(), Source: "https://fp2"}},
	}
	if err := st.CreatePlaylist(ctx, plID1, "filter-pl-1", rawDoc(t, &pl1)); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePlaylist(ctx, plID2, "filter-pl-2", rawDoc(t, &pl2)); err != nil {
		t.Fatal(err)
	}

	groupID := uuid.MustParse("81818181-8181-4181-8181-818181818181")
	groupSlug := "filter-test-group"
	groupBody := playlistgroup.Group{
		ID:        groupID.String(),
		Slug:      groupSlug,
		Title:     "Filter Group",
		Playlists: []string{"filter-pl-1"},
	}
	groupInput := &store.PlaylistGroupInput{
		ID:   groupID,
		Slug: groupSlug,
		Raw:  rawDoc(t, groupBody),
		Playlists: []store.IngestedPlaylist{
			{ID: plID1, Slug: "filter-pl-1", Raw: rawDoc(t, pl1)},
		},
	}
	if err := st.CreatePlaylistGroup(ctx, groupInput); err != nil {
		t.Fatalf("CreatePlaylistGroup: %v", err)
	}

	byGroupSlug, _, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{
		Limit:               10,
		Cursor:              "",
		Sort:                store.SortAsc,
		PlaylistGroupFilter: groupSlug,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(byGroupSlug) != 1 || byGroupSlug[0].ID != plID1 {
		t.Fatalf("playlist-group filter by slug: got %d rows, want 1 with id %v", len(byGroupSlug), plID1)
	}

	byGroupUUID, _, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{
		Limit:               10,
		Cursor:              "",
		Sort:                store.SortAsc,
		PlaylistGroupFilter: groupID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(byGroupUUID) != 1 || byGroupUUID[0].ID != plID1 {
		t.Fatalf("playlist-group filter by uuid: got %d rows", len(byGroupUUID))
	}

	chID := uuid.MustParse("c1c1c1c1-c1c1-c1c1-c1c1-c1c1c1c1c1c1")
	chSlug := "filter-test-channel"
	chBody := channels.Channel{
		ID:        chID.String(),
		Slug:      chSlug,
		Title:     "Filter Channel",
		Version:   "1.0.0",
		Playlists: []string{"filter-pl-1", "filter-pl-2"},
	}
	chInput := &store.ChannelInput{
		ID:   chID,
		Slug: chSlug,
		Raw:  rawDoc(t, chBody),
		Playlists: []store.IngestedPlaylist{
			{ID: plID1, Slug: "filter-pl-1", Raw: rawDoc(t, pl1)},
			{ID: plID2, Slug: "filter-pl-2", Raw: rawDoc(t, pl2)},
		},
	}
	if err := st.CreateChannel(ctx, chInput); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	byChSlug, _, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{
		Limit:         10,
		Cursor:        "",
		Sort:          store.SortAsc,
		ChannelFilter: chSlug,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(byChSlug) != 2 {
		t.Fatalf("channel filter: want 2 playlists, got %d", len(byChSlug))
	}

	byChUUID, _, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{
		Limit:         10,
		Cursor:        "",
		Sort:          store.SortAsc,
		ChannelFilter: chID.String(),
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(byChUUID) != 2 {
		t.Fatalf("channel filter by uuid: want 2, got %d", len(byChUUID))
	}
}

func TestIntegration_ListPlaylistItems_and_GetPlaylistItem(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	pid1 := uuid.MustParse("a1a1a1a1-a1a1-a1a1-a1a1-a1a1a1a1a1a1")
	pid2 := uuid.MustParse("b2b2b2b2-b2b2-b2b2-b2b2-b2b2b2b2b2b2")
	pid3 := uuid.MustParse("c3c3c3c3-c3c3-c3c3-c3c3-c3c3c3c3c3c3")
	iid1 := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	iid2 := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	iid3 := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	iid4 := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	iid5 := uuid.MustParse("55555555-5555-5555-5555-555555555555")

	pl1 := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "PL1",
		Items: []playlist.PlaylistItem{
			{ID: iid1.String(), Source: "https://one", Title: "Item One"},
			{ID: iid2.String(), Source: "https://two", Title: "Item Two"},
		},
	}
	pl2 := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "PL2",
		Items: []playlist.PlaylistItem{
			{ID: iid3.String(), Source: "https://three", Title: "Item Three"},
		},
	}
	pl3 := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "PL3",
		Items: []playlist.PlaylistItem{
			{ID: iid4.String(), Source: "https://four", Title: "Item Four"},
			{ID: iid5.String(), Source: "https://five", Title: "Item Five"},
		},
	}

	if err := st.CreatePlaylist(ctx, pid1, "pl-items-1", rawDoc(t, &pl1)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := st.CreatePlaylist(ctx, pid2, "pl-items-2", rawDoc(t, &pl2)); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := st.CreatePlaylist(ctx, pid3, "pl-items-3", rawDoc(t, &pl3)); err != nil {
		t.Fatal(err)
	}

	t.Run("list_all_asc", func(t *testing.T) {
		all, cur, err := st.ListPlaylistItems(ctx, &store.ListPlaylistItemsParams{
			Limit:  10,
			Cursor: "",
			Sort:   store.SortAsc,
		})
		if err != nil {
			t.Fatal(err)
		}
		if cur != "" {
			t.Fatalf("unexpected cursor: %q", cur)
		}
		if len(all) != 5 {
			t.Fatalf("list all: want 5 items, got %d", len(all))
		}
		if all[0].ItemID != iid1 || all[1].ItemID != iid2 || all[2].ItemID != iid3 || all[3].ItemID != iid4 || all[4].ItemID != iid5 {
			t.Fatalf("asc order wrong: %v, %v, %v, %v, %v", all[0].ItemID, all[1].ItemID, all[2].ItemID, all[3].ItemID, all[4].ItemID)
		}
		if all[0].PlaylistID != pid1 {
			t.Fatalf("first item playlist_id: want %v, got %v", pid1, all[0].PlaylistID)
		}
		if all[0].Position != 0 {
			t.Fatalf("first item position: want 0, got %d", all[0].Position)
		}
		if all[1].Position != 1 {
			t.Fatalf("second item position: want 1, got %d", all[1].Position)
		}
		if all[0].Item.Title != "Item One" || all[0].Item.Source != "https://one" {
			t.Fatalf("first item content: %+v", all[0].Item)
		}
	})

	t.Run("list_all_desc", func(t *testing.T) {
		all, cur, err := st.ListPlaylistItems(ctx, &store.ListPlaylistItemsParams{
			Limit:  10,
			Cursor: "",
			Sort:   store.SortDesc,
		})
		if err != nil {
			t.Fatal(err)
		}
		if cur != "" {
			t.Fatalf("unexpected cursor: %q", cur)
		}
		if len(all) != 5 {
			t.Fatalf("list all desc: want 5 items, got %d", len(all))
		}
		if all[0].ItemID != iid5 || all[1].ItemID != iid4 || all[2].ItemID != iid3 || all[3].ItemID != iid2 || all[4].ItemID != iid1 {
			t.Fatalf("desc order wrong: %v, %v, %v, %v, %v", all[0].ItemID, all[1].ItemID, all[2].ItemID, all[3].ItemID, all[4].ItemID)
		}
	})

	t.Run("pagination_asc", func(t *testing.T) {
		page1, cur1, err := st.ListPlaylistItems(ctx, &store.ListPlaylistItemsParams{
			Limit:  2,
			Cursor: "",
			Sort:   store.SortAsc,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(page1) != 2 {
			t.Fatalf("page1 len: want 2, got %d", len(page1))
		}
		if cur1 == "" {
			t.Fatal("page1: expected non-empty cursor")
		}
		if page1[0].ItemID != iid1 || page1[1].ItemID != iid2 {
			t.Fatalf("page1 items: %v, %v", page1[0].ItemID, page1[1].ItemID)
		}

		page2, cur2, err := st.ListPlaylistItems(ctx, &store.ListPlaylistItemsParams{
			Limit:  2,
			Cursor: cur1,
			Sort:   store.SortAsc,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(page2) != 2 {
			t.Fatalf("page2 len: want 2, got %d", len(page2))
		}
		if cur2 == "" {
			t.Fatal("page2: expected non-empty cursor")
		}
		if page2[0].ItemID != iid3 || page2[1].ItemID != iid4 {
			t.Fatalf("page2 items: %v, %v", page2[0].ItemID, page2[1].ItemID)
		}

		page3, cur3, err := st.ListPlaylistItems(ctx, &store.ListPlaylistItemsParams{
			Limit:  2,
			Cursor: cur2,
			Sort:   store.SortAsc,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(page3) != 1 {
			t.Fatalf("page3 len: want 1, got %d", len(page3))
		}
		if cur3 != "" {
			t.Fatalf("page3: expected empty cursor (last page), got %q", cur3)
		}
		if page3[0].ItemID != iid5 {
			t.Fatalf("page3 items: %v", page3[0].ItemID)
		}
	})

	t.Run("get_item_by_id", func(t *testing.T) {
		one, err := st.GetPlaylistItem(ctx, iid2)
		if err != nil {
			t.Fatal(err)
		}
		if one.ItemID != iid2 {
			t.Fatalf("ItemID: want %v, got %v", iid2, one.ItemID)
		}
		if one.PlaylistID != pid1 {
			t.Fatalf("PlaylistID: want %v, got %v", pid1, one.PlaylistID)
		}
		if one.Position != 1 {
			t.Fatalf("Position: want 1, got %d", one.Position)
		}
		if one.Item.Source != "https://two" {
			t.Fatalf("Source: want https://two, got %s", one.Item.Source)
		}
		if one.Item.Title != "Item Two" {
			t.Fatalf("Title: want Item Two, got %s", one.Item.Title)
		}
		if one.Item.ID != iid2.String() {
			t.Fatalf("Item.ID: want %s, got %s", iid2.String(), one.Item.ID)
		}
	})

	t.Run("get_item_not_found", func(t *testing.T) {
		_, err := st.GetPlaylistItem(ctx, uuid.MustParse("99999999-9999-9999-9999-999999999999"))
		if err == nil || !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("GetPlaylistItem missing: want ErrNotFound, got %v", err)
		}
	})

	t.Run("filter_by_playlist_group", func(t *testing.T) {
		gid := uuid.MustParse("e3e3e3e3-e3e3-e3e3-e3e3-e3e3e3e3e3e3")
		gbody := playlistgroup.Group{
			ID:        gid.String(),
			Slug:      "grp-for-items",
			Title:     "G",
			Playlists: []string{"pl-items-1"},
		}
		in := &store.PlaylistGroupInput{
			ID:        gid,
			Slug:      "grp-for-items",
			Raw:       rawDoc(t, gbody),
			Playlists: []store.IngestedPlaylist{{ID: pid1, Slug: "pl-items-1", Raw: rawDoc(t, pl1)}},
		}
		if err := st.CreatePlaylistGroup(ctx, in); err != nil {
			t.Fatal(err)
		}

		filtered, cur, err := st.ListPlaylistItems(ctx, &store.ListPlaylistItemsParams{
			Limit:               10,
			Cursor:              "",
			Sort:                store.SortAsc,
			PlaylistGroupFilter: "grp-for-items",
		})
		if err != nil {
			t.Fatal(err)
		}
		if cur != "" {
			t.Fatalf("filtered: unexpected cursor %q", cur)
		}
		if len(filtered) != 2 {
			t.Fatalf("filtered: want 2 items (pl1 has 2 items), got %d", len(filtered))
		}
		if filtered[0].ItemID != iid1 || filtered[1].ItemID != iid2 {
			t.Fatalf("filtered items: want [%v, %v], got [%v, %v]", iid1, iid2, filtered[0].ItemID, filtered[1].ItemID)
		}

		filteredByUUID, _, err := st.ListPlaylistItems(ctx, &store.ListPlaylistItemsParams{
			Limit:               10,
			Cursor:              "",
			Sort:                store.SortAsc,
			PlaylistGroupFilter: gid.String(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(filteredByUUID) != 2 {
			t.Fatalf("filtered by UUID: want 2, got %d", len(filteredByUUID))
		}
	})

	t.Run("filter_by_channel", func(t *testing.T) {
		chid := uuid.MustParse("f4f4f4f4-f4f4-f4f4-f4f4-f4f4f4f4f4f4")
		chbody := channels.Channel{
			ID:        chid.String(),
			Slug:      "ch-for-items",
			Title:     "CH",
			Playlists: []string{"pl-items-2", "pl-items-3"},
		}
		chin := &store.ChannelInput{
			ID:   chid,
			Slug: "ch-for-items",
			Raw:  rawDoc(t, chbody),
			Playlists: []store.IngestedPlaylist{
				{ID: pid2, Slug: "pl-items-2", Raw: rawDoc(t, pl2)},
				{ID: pid3, Slug: "pl-items-3", Raw: rawDoc(t, pl3)},
			},
		}
		if err := st.CreateChannel(ctx, chin); err != nil {
			t.Fatal(err)
		}

		filtered, cur, err := st.ListPlaylistItems(ctx, &store.ListPlaylistItemsParams{
			Limit:         10,
			Cursor:        "",
			Sort:          store.SortAsc,
			ChannelFilter: "ch-for-items",
		})
		if err != nil {
			t.Fatal(err)
		}
		if cur != "" {
			t.Fatalf("channel filtered: unexpected cursor %q", cur)
		}
		if len(filtered) != 3 {
			t.Fatalf("channel filtered: want 3 items (pl2 has 1, pl3 has 2), got %d", len(filtered))
		}
		if filtered[0].ItemID != iid3 || filtered[1].ItemID != iid4 || filtered[2].ItemID != iid5 {
			t.Fatalf("channel filtered items: want [%v, %v, %v], got [%v, %v, %v]",
				iid3, iid4, iid5, filtered[0].ItemID, filtered[1].ItemID, filtered[2].ItemID)
		}

		filteredBySlug, _, err := st.ListPlaylistItems(ctx, &store.ListPlaylistItemsParams{
			Limit:         10,
			Cursor:        "",
			Sort:          store.SortAsc,
			ChannelFilter: chid.String(),
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(filteredBySlug) != 3 {
			t.Fatalf("filtered by channel UUID: want 3, got %d", len(filteredBySlug))
		}
	})

	t.Run("position_ordering_within_playlist", func(t *testing.T) {
		items, err := st.GetPlaylistItems(ctx, "pl-items-1")
		if err != nil {
			t.Fatal(err)
		}
		if len(items) != 2 {
			t.Fatalf("GetPlaylistItems: want 2, got %d", len(items))
		}
		if items[0].Position != 0 || items[1].Position != 1 {
			t.Fatalf("positions: want [0, 1], got [%d, %d]", items[0].Position, items[1].Position)
		}
		if items[0].ItemID != iid1 || items[1].ItemID != iid2 {
			t.Fatalf("GetPlaylistItems order: want [%v, %v], got [%v, %v]", iid1, iid2, items[0].ItemID, items[1].ItemID)
		}
	})

	t.Run("get_item_comprehensive_fields", func(t *testing.T) {
		rec, err := st.GetPlaylistItem(ctx, iid4)
		if err != nil {
			t.Fatal(err)
		}
		if rec.ItemID != iid4 {
			t.Errorf("ItemID: want %v, got %v", iid4, rec.ItemID)
		}
		if rec.PlaylistID != pid3 {
			t.Errorf("PlaylistID: want %v, got %v", pid3, rec.PlaylistID)
		}
		if rec.Position != 0 {
			t.Errorf("Position: want 0 (first in pl3), got %d", rec.Position)
		}
		if rec.Item.ID != iid4.String() {
			t.Errorf("Item.ID: want %s, got %s", iid4.String(), rec.Item.ID)
		}
		if rec.Item.Source != "https://four" {
			t.Errorf("Item.Source: want https://four, got %s", rec.Item.Source)
		}
		if rec.Item.Title != "Item Four" {
			t.Errorf("Item.Title: want Item Four, got %s", rec.Item.Title)
		}
	})
}

func TestIntegration_UpdatePlaylist_notFound(t *testing.T) {
	st := newStore(t)
	// Missing id/slug → ErrNotFound, no partial write.
	empty := playlist.Playlist{}
	err := st.UpdatePlaylist(context.Background(), uuid.New().String(), rawDoc(t, &empty), time.Time{})
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestIntegration_DeletePlaylist_notFound(t *testing.T) {
	st := newStore(t)
	// Unknown slug → ErrNotFound (same contract as get/update).
	err := st.DeletePlaylist(context.Background(), "missing-slug", time.Time{})
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

// =============================================================================
// Playlist Group Contract Tests
// =============================================================================

func TestIntegration_PlaylistGroupCRUD_and_List(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Setup: Create playlists that will be referenced by the group
	playlistID1 := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	playlistID2 := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	pl1 := playlist.Playlist{DPVersion: "1.1.0", Title: "P1"}
	pl2 := playlist.Playlist{DPVersion: "1.1.0", Title: "P2"}
	if err := st.CreatePlaylist(ctx, playlistID1, "playlist-1", rawDoc(t, &pl1)); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePlaylist(ctx, playlistID2, "playlist-2", rawDoc(t, &pl2)); err != nil {
		t.Fatal(err)
	}

	// Create group with upserted playlists
	groupID := uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa")
	groupSlug := "test-group"
	groupBody := playlistgroup.Group{
		ID:        groupID.String(),
		Slug:      groupSlug,
		Title:     "Test Group",
		Playlists: []string{"playlist-1", "playlist-2"},
	}
	groupInput := &store.PlaylistGroupInput{
		ID:   groupID,
		Slug: groupSlug,
		Raw:  rawDoc(t, groupBody),
		Playlists: []store.IngestedPlaylist{
			{ID: playlistID1, Slug: "playlist-1", Raw: rawDoc(t, pl1)},
			{ID: playlistID2, Slug: "playlist-2", Raw: rawDoc(t, pl2)},
		},
	}

	if err := st.CreatePlaylistGroup(ctx, groupInput); err != nil {
		t.Fatalf("CreatePlaylistGroup: %v", err)
	}

	// Get by UUID - comprehensive assertions
	byID, err := st.GetPlaylistGroup(ctx, groupID.String())
	if err != nil {
		t.Fatal(err)
	}
	if byID.ID != groupID {
		t.Fatalf("Get by id: ID mismatch, want %v, got %v", groupID, byID.ID)
	}
	if byID.Slug != groupSlug {
		t.Fatalf("Get by id: slug mismatch, want %q, got %q", groupSlug, byID.Slug)
	}
	if byID.Body.Title != "Test Group" {
		t.Fatalf("Get by id: title mismatch, want %q, got %q", "Test Group", byID.Body.Title)
	}
	if len(byID.Body.Playlists) != 2 {
		t.Fatalf("Get by id: playlists length mismatch, want 2, got %d", len(byID.Body.Playlists))
	}
	if byID.Body.Playlists[0] != "playlist-1" || byID.Body.Playlists[1] != "playlist-2" {
		t.Fatalf("Get by id: playlists content mismatch, got %v", byID.Body.Playlists)
	}
	if byID.CreatedAt.IsZero() {
		t.Fatal("Get by id: CreatedAt should be set")
	}
	if byID.UpdatedAt.IsZero() {
		t.Fatal("Get by id: UpdatedAt should be set")
	}
	createdAt := byID.CreatedAt // Save for later comparison

	// Get by slug - verify same record
	bySlug, err := st.GetPlaylistGroup(ctx, groupSlug)
	if err != nil {
		t.Fatal(err)
	}
	if bySlug.ID != groupID {
		t.Fatalf("Get by slug: ID mismatch, want %v, got %v", groupID, bySlug.ID)
	}
	if bySlug.Slug != groupSlug {
		t.Fatalf("Get by slug: slug mismatch, want %q, got %q", groupSlug, bySlug.Slug)
	}
	assertDeepEqual(t, "Get by slug body", byID.Body, bySlug.Body)

	// Verify membership before update
	membersBefore, err := st.ListPlaylistsInGroup(ctx, groupID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(membersBefore) != 2 {
		t.Fatalf("members before update: want 2, got %d", len(membersBefore))
	}

	// List includes our group
	recs, _, err := st.ListPlaylistGroups(ctx, &store.ListPlaylistsParams{
		Limit:  10,
		Cursor: "",
		Sort:   store.SortAsc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("list len: want 1, got %d", len(recs))
	}
	if recs[0].ID != groupID {
		t.Fatalf("list record ID: want %v, got %v", groupID, recs[0].ID)
	}

	// Update group body and membership (reduce from 2 playlists to 1)
	time.Sleep(5 * time.Millisecond) // Ensure updated_at changes
	updatedGroupBody := playlistgroup.Group{
		ID:        groupID.String(),
		Slug:      groupSlug,
		Title:     "Updated Group",
		Playlists: []string{"playlist-2"},
	}
	updatedInput := &store.PlaylistGroupInput{
		Raw: rawDoc(t, updatedGroupBody),
		Playlists: []store.IngestedPlaylist{
			{ID: playlistID2, Slug: "playlist-2", Raw: rawDoc(t, pl2)},
		},
	}
	if err := st.UpdatePlaylistGroup(ctx, groupSlug, updatedInput, grpUpdatedAt(t, ctx, st, groupSlug)); err != nil {
		t.Fatalf("UpdatePlaylistGroup: %v", err)
	}

	// Verify update worked - comprehensive checks
	afterUpdate, err := st.GetPlaylistGroup(ctx, groupID.String())
	if err != nil {
		t.Fatalf("GetPlaylistGroup after update: %v", err)
	}
	if afterUpdate.ID != groupID {
		t.Fatalf("after update: ID changed unexpectedly to %v", afterUpdate.ID)
	}
	if afterUpdate.Slug != groupSlug {
		t.Fatalf("after update: slug changed unexpectedly to %q", afterUpdate.Slug)
	}
	if afterUpdate.Body.Title != "Updated Group" {
		t.Fatalf("after update: title mismatch, want %q, got %q", "Updated Group", afterUpdate.Body.Title)
	}
	if len(afterUpdate.Body.Playlists) != 1 {
		t.Fatalf("after update: playlists length, want 1, got %d", len(afterUpdate.Body.Playlists))
	}
	if afterUpdate.Body.Playlists[0] != "playlist-2" {
		t.Fatalf("after update: playlist mismatch, want %q, got %q", "playlist-2", afterUpdate.Body.Playlists[0])
	}
	if afterUpdate.CreatedAt != createdAt {
		t.Fatalf("after update: CreatedAt changed from %v to %v", createdAt, afterUpdate.CreatedAt)
	}
	if !afterUpdate.UpdatedAt.After(afterUpdate.CreatedAt) {
		t.Fatalf("after update: UpdatedAt (%v) should be after CreatedAt (%v)", afterUpdate.UpdatedAt, afterUpdate.CreatedAt)
	}

	// Verify membership was actually replaced (old member removed)
	membersAfter, err := st.ListPlaylistsInGroup(ctx, groupID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(membersAfter) != 1 {
		t.Fatalf("members after update: want 1, got %d", len(membersAfter))
	}
	if membersAfter[0].ID != playlistID2 {
		t.Fatalf("members after update: want playlist %v, got %v", playlistID2, membersAfter[0].ID)
	}

	// Delete removes row
	if err := st.DeletePlaylistGroup(ctx, groupID.String(), grpUpdatedAt(t, ctx, st, groupID.String())); err != nil {
		t.Fatal(err)
	}
	_, err = st.GetPlaylistGroup(ctx, groupID.String())
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: expected ErrNotFound, got %v", err)
	}

	// Verify membership rows were cascade-deleted
	_, err = st.ListPlaylistsInGroup(ctx, groupID.String())
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: membership should return ErrNotFound, got %v", err)
	}
}

func TestIntegration_GetPlaylistGroup_notFound(t *testing.T) {
	st := newStore(t)
	_, err := st.GetPlaylistGroup(context.Background(), uuid.New().String())
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestIntegration_UpdatePlaylistGroup_notFound(t *testing.T) {
	st := newStore(t)
	input := &store.PlaylistGroupInput{
		Raw:       rawDoc(t, playlistgroup.Group{Title: "Missing"}),
		Playlists: []store.IngestedPlaylist{},
	}
	err := st.UpdatePlaylistGroup(context.Background(), uuid.New().String(), input, time.Time{})
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestIntegration_DeletePlaylistGroup_notFound(t *testing.T) {
	st := newStore(t)
	err := st.DeletePlaylistGroup(context.Background(), "missing-slug", time.Time{})
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestIntegration_ListPlaylistsInGroup(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Create playlists
	playlistID1 := uuid.MustParse("33333333-3333-3333-3333-333333333333")
	playlistID2 := uuid.MustParse("44444444-4444-4444-4444-444444444444")
	pl1 := playlist.Playlist{DPVersion: "1.1.0", Title: "First"}
	pl2 := playlist.Playlist{DPVersion: "1.1.0", Title: "Second"}
	if err := st.CreatePlaylist(ctx, playlistID1, "pl-first", rawDoc(t, &pl1)); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePlaylist(ctx, playlistID2, "pl-second", rawDoc(t, &pl2)); err != nil {
		t.Fatal(err)
	}

	// Create group with ordered membership
	groupID := uuid.MustParse("55555555-5555-5555-5555-555555555555")
	groupInput := &store.PlaylistGroupInput{
		ID:   groupID,
		Slug: "membership-group",
		Raw: rawDoc(t, playlistgroup.Group{
			ID:        groupID.String(),
			Slug:      "membership-group",
			Title:     "Membership Test",
			Playlists: []string{"pl-first", "pl-second"},
		}),
		Playlists: []store.IngestedPlaylist{
			{ID: playlistID1, Slug: "pl-first", Raw: rawDoc(t, pl1)},
			{ID: playlistID2, Slug: "pl-second", Raw: rawDoc(t, pl2)},
		},
	}
	if err := st.CreatePlaylistGroup(ctx, groupInput); err != nil {
		t.Fatal(err)
	}

	// List members preserves order
	members, err := st.ListPlaylistsInGroup(ctx, groupID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}
	if members[0].ID != playlistID1 {
		t.Fatalf("first member ID mismatch: got %v", members[0].ID)
	}
	if members[1].ID != playlistID2 {
		t.Fatalf("second member ID mismatch: got %v", members[1].ID)
	}
	assertDeepEqual(t, "first member body", pl1, members[0].Body)
	assertDeepEqual(t, "second member body", pl2, members[1].Body)
}

func TestIntegration_ListPlaylistsInGroup_notFound(t *testing.T) {
	st := newStore(t)
	_, err := st.ListPlaylistsInGroup(context.Background(), "missing-group")
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestIntegration_ListPlaylistGroups_pagination(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Create two groups
	groupID1 := uuid.MustParse("66666666-6666-6666-6666-666666666666")
	groupID2 := uuid.MustParse("77777777-7777-7777-7777-777777777777")
	group1 := &store.PlaylistGroupInput{
		ID:   groupID1,
		Slug: "group-first",
		Raw: rawDoc(t, playlistgroup.Group{
			ID:        groupID1.String(),
			Slug:      "group-first",
			Title:     "First Group",
			Playlists: []string{},
		}),
		Playlists: []store.IngestedPlaylist{},
	}
	group2 := &store.PlaylistGroupInput{
		ID:   groupID2,
		Slug: "group-second",
		Raw: rawDoc(t, playlistgroup.Group{
			ID:        groupID2.String(),
			Slug:      "group-second",
			Title:     "Second Group",
			Playlists: []string{},
		}),
		Playlists: []store.IngestedPlaylist{},
	}
	if err := st.CreatePlaylistGroup(ctx, group1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := st.CreatePlaylistGroup(ctx, group2); err != nil {
		t.Fatal(err)
	}

	// First page: one row, non-empty cursor
	page1, cur, err := st.ListPlaylistGroups(ctx, &store.ListPlaylistsParams{
		Limit:  1,
		Cursor: "",
		Sort:   store.SortAsc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 1 || cur == "" {
		t.Fatalf("page1 len=%d cur=%q", len(page1), cur)
	}

	// Second page: other row, empty cursor
	page2, cur2, err := st.ListPlaylistGroups(ctx, &store.ListPlaylistsParams{
		Limit:  1,
		Cursor: cur,
		Sort:   store.SortAsc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 {
		t.Fatalf("page2 len %d", len(page2))
	}
	if page1[0].Body.Title == page2[0].Body.Title {
		t.Fatal("expected different rows across pages")
	}
	if cur2 != "" {
		t.Fatalf("expected empty next cursor on last page, got %q", cur2)
	}
}

// =============================================================================
// Playlist Group Edge Case Tests
// =============================================================================

func TestIntegration_PlaylistGroup_emptyPlaylists(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Create group with no playlists
	groupID := uuid.MustParse("00000000-0000-0000-0000-000000000001")
	emptyGroupInput := &store.PlaylistGroupInput{
		ID:   groupID,
		Slug: "empty-group",
		Raw: rawDoc(t, playlistgroup.Group{
			ID:        groupID.String(),
			Slug:      "empty-group",
			Title:     "Empty Group",
			Playlists: []string{},
		}),
		Playlists: []store.IngestedPlaylist{},
	}
	if err := st.CreatePlaylistGroup(ctx, emptyGroupInput); err != nil {
		t.Fatalf("CreatePlaylistGroup with empty playlists: %v", err)
	}

	// Verify it was created
	rec, err := st.GetPlaylistGroup(ctx, groupID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Body.Playlists) != 0 {
		t.Fatalf("expected 0 playlists, got %d", len(rec.Body.Playlists))
	}

	// List members should return empty but not error
	members, err := st.ListPlaylistsInGroup(ctx, groupID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 0 {
		t.Fatalf("expected 0 members, got %d", len(members))
	}

	// Update to add a playlist
	playlistID := uuid.MustParse("00000000-0000-0000-0000-000000000002")
	pl := playlist.Playlist{DPVersion: "1.1.0", Title: "Added Later"}
	if err := st.CreatePlaylist(ctx, playlistID, "added-later", rawDoc(t, &pl)); err != nil {
		t.Fatal(err)
	}

	updateInput := &store.PlaylistGroupInput{
		Raw: rawDoc(t, playlistgroup.Group{
			ID:        groupID.String(),
			Slug:      "empty-group",
			Title:     "No Longer Empty",
			Playlists: []string{"added-later"},
		}),
		Playlists: []store.IngestedPlaylist{
			{ID: playlistID, Slug: "added-later", Raw: rawDoc(t, pl)},
		},
	}
	if err := st.UpdatePlaylistGroup(ctx, groupID.String(), updateInput, grpUpdatedAt(t, ctx, st, groupID.String())); err != nil {
		t.Fatalf("UpdatePlaylistGroup to add playlists: %v", err)
	}

	// Verify update worked
	afterUpdate, _ := st.GetPlaylistGroup(ctx, groupID.String())
	if len(afterUpdate.Body.Playlists) != 1 {
		t.Fatalf("after update: expected 1 playlist, got %d", len(afterUpdate.Body.Playlists))
	}
	membersAfter, _ := st.ListPlaylistsInGroup(ctx, groupID.String())
	if len(membersAfter) != 1 {
		t.Fatalf("after update: expected 1 member, got %d", len(membersAfter))
	}

	// Update back to empty
	emptyAgain := &store.PlaylistGroupInput{
		Raw: rawDoc(t, playlistgroup.Group{
			ID:        groupID.String(),
			Slug:      "empty-group",
			Title:     "Empty Again",
			Playlists: []string{},
		}),
		Playlists: []store.IngestedPlaylist{},
	}
	if err := st.UpdatePlaylistGroup(ctx, groupID.String(), emptyAgain, grpUpdatedAt(t, ctx, st, groupID.String())); err != nil {
		t.Fatalf("UpdatePlaylistGroup to remove all playlists: %v", err)
	}

	// Verify all members removed
	finalMembers, err := st.ListPlaylistsInGroup(ctx, groupID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(finalMembers) != 0 {
		t.Fatalf("after removing all: expected 0 members, got %d", len(finalMembers))
	}
}

func TestIntegration_PlaylistGroup_duplicatePlaylists(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	playlistID := uuid.MustParse("00000000-0000-0000-0000-000000000003")
	itemID := uuid.MustParse("00000000-0000-4000-8000-000000000003")
	pl := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "Repeated",
		Items:     []playlist.PlaylistItem{{ID: itemID.String(), Source: "https://example.com/repeated.html"}},
	}
	laterItemID := uuid.MustParse("00000000-0000-4000-8000-000000000004")
	later := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "Conflicting later occurrence",
		Items:     []playlist.PlaylistItem{{ID: laterItemID.String(), Source: "https://example.com/later.html"}},
	}

	// Create group with same playlist repeated (valid in DP-1 spec - position matters)
	groupID := uuid.MustParse("00000000-0000-0000-0000-000000000004")
	dupInput := &store.PlaylistGroupInput{
		ID:   groupID,
		Slug: "dup-group",
		Raw: rawDoc(t, playlistgroup.Group{
			ID:        groupID.String(),
			Slug:      "dup-group",
			Title:     "Duplicate Test",
			Playlists: []string{"repeated", "repeated", "repeated"},
		}),
		Playlists: []store.IngestedPlaylist{
			{ID: playlistID, Slug: "repeated", Raw: rawDoc(t, pl)},
			{ID: playlistID, Slug: "repeated-later", Raw: rawDoc(t, later)},
			{ID: playlistID, Slug: "repeated-later", Raw: rawDoc(t, later)},
		},
	}
	if err := st.CreatePlaylistGroup(ctx, dupInput); err != nil {
		t.Fatalf("CreatePlaylistGroup with duplicate playlists: %v", err)
	}

	// Verify all three positions were created
	members, err := st.ListPlaylistsInGroup(ctx, groupID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 3 {
		t.Fatalf("expected 3 members (duplicates allowed), got %d", len(members))
	}
	// All should be the same playlist
	for i, m := range members {
		if m.ID != playlistID {
			t.Fatalf("member[%d]: wrong ID, want %v, got %v", i, playlistID, m.ID)
		}
	}
	stored, err := st.GetPlaylist(ctx, playlistID.String())
	if err != nil {
		t.Fatalf("GetPlaylist: %v", err)
	}
	if stored.Slug != "repeated" {
		t.Fatalf("deduplicated playlist slug: got %q, want first occurrence", stored.Slug)
	}
	assertDeepEqual(t, "deduplicated playlist body", pl, stored.Body)

	items, err := st.GetPlaylistItems(ctx, playlistID.String())
	if err != nil {
		t.Fatalf("GetPlaylistItems: %v", err)
	}
	if len(items) != 1 || items[0].ItemID != itemID {
		t.Fatalf("item index for deduplicated playlist: %+v", items)
	}
}

// Membership ingestion may link an existing playlist but must never modify one. Group and channel
// creation is open, so if ingest upserted, anyone able to create a group could host a document carrying a
// victim's playlist id and overwrite that playlist's body, slug, owner set and item index on this feed.
// This is that attack: a second ingest reuses the stored id with different content and a different owner.
func TestIntegration_PlaylistGroupIngestNeverModifiesExistingPlaylist(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	playlistID := uuid.MustParse("10000000-0000-4000-8000-000000000001")
	originalItemID := uuid.MustParse("20000000-0000-4000-8000-000000000001")
	original := playlist.Playlist{
		DPVersion: "1.1.0",
		ID:        playlistID.String(),
		Slug:      "remote-ingest",
		Title:     "Remotely ingested",
		Curators:  []identity.Entity{{Name: "Owner A", Key: "did:key:ownerA"}},
		Items: []playlist.PlaylistItem{
			{ID: originalItemID.String(), Source: "https://example.com/original.html"},
		},
	}
	groupID := uuid.MustParse("30000000-0000-4000-8000-000000000001")
	group := playlistgroup.Group{
		ID:        groupID.String(),
		Slug:      "remote-ingest-group",
		Title:     "Remote ingest group",
		Playlists: []string{"https://origin.example/playlist.json"},
	}

	if err := st.CreatePlaylistGroup(ctx, &store.PlaylistGroupInput{
		ID:        groupID,
		Slug:      group.Slug,
		Raw:       rawDoc(t, group),
		Playlists: []store.IngestedPlaylist{{ID: playlistID, Slug: "remote-ingest", Raw: rawDoc(t, original)}},
	}); err != nil {
		t.Fatalf("CreatePlaylistGroup: %v", err)
	}

	// A playlist new to this feed is created by the ingest, index and all.
	items, err := st.GetPlaylistItems(ctx, playlistID.String())
	if err != nil {
		t.Fatalf("GetPlaylistItems after create ingest: %v", err)
	}
	if len(items) != 1 || items[0].ItemID != originalItemID {
		t.Fatalf("item index after create ingest: %+v", items)
	}
	before, err := st.GetPlaylist(ctx, playlistID.String())
	if err != nil {
		t.Fatalf("GetPlaylist after create ingest: %v", err)
	}

	// The spoof: same id, different content, different owner, different slug.
	spoofItemID := uuid.MustParse("20000000-0000-4000-8000-000000000002")
	spoof := playlist.Playlist{
		DPVersion: "1.1.0",
		ID:        playlistID.String(),
		Slug:      "hijacked",
		Title:     "Hijacked",
		Curators:  []identity.Entity{{Name: "Attacker B", Key: "did:key:attackerB"}},
		Items: []playlist.PlaylistItem{
			{ID: spoofItemID.String(), Source: "https://attacker.example/spoof.html"},
		},
	}
	if err := st.UpdatePlaylistGroup(ctx, group.Slug, &store.PlaylistGroupInput{
		Raw:       rawDoc(t, group),
		Playlists: []store.IngestedPlaylist{{ID: playlistID, Slug: "hijacked", Raw: rawDoc(t, spoof)}},
	}, grpUpdatedAt(t, ctx, st, group.Slug)); err != nil {
		t.Fatalf("UpdatePlaylistGroup: %v", err)
	}

	after, err := st.GetPlaylist(ctx, playlistID.String())
	if err != nil {
		t.Fatalf("GetPlaylist after re-ingest: %v", err)
	}
	if string(after.Raw) != string(before.Raw) {
		t.Fatalf("ingest overwrote the stored playlist body:\n before=%s\n after =%s", before.Raw, after.Raw)
	}
	if after.Slug != before.Slug {
		t.Fatalf("ingest moved the stored playlist slug: got %q, want %q", after.Slug, before.Slug)
	}
	if len(after.Body.Curators) != 1 || after.Body.Curators[0].Key != "did:key:ownerA" {
		t.Fatalf("ingest changed the owner set: %+v", after.Body.Curators)
	}

	// The victim's item index is untouched: the spoof's item was never indexed.
	items, err = st.GetPlaylistItems(ctx, playlistID.String())
	if err != nil {
		t.Fatalf("GetPlaylistItems after re-ingest: %v", err)
	}
	if len(items) != 1 || items[0].ItemID != originalItemID {
		t.Fatalf("ingest rebuilt the item index: %+v", items)
	}
	if _, err := st.GetPlaylistItem(ctx, spoofItemID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("spoof item was indexed: got %v, want ErrNotFound", err)
	}

	// The group still links the (unmodified) playlist.
	linked, _, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{Limit: 10, PlaylistGroupFilter: group.Slug})
	if err != nil {
		t.Fatalf("ListPlaylists in group: %v", err)
	}
	if len(linked) != 1 || linked[0].ID != playlistID {
		t.Fatalf("group membership: %+v", linked)
	}
}

// A referenced id this feed has deleted must not be re-created through ingestion — otherwise membership
// would be a side door around the create-path tombstone guard.
func TestIntegration_IngestRejectsTombstonedPlaylistID(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	playlistID := uuid.MustParse("10000000-0000-4000-8000-0000000000aa")
	doc := playlist.Playlist{
		DPVersion: "1.1.0",
		ID:        playlistID.String(),
		Slug:      "tombstoned-ingest",
		Title:     "Doomed",
	}
	if err := st.CreatePlaylist(ctx, playlistID, doc.Slug, rawDoc(t, doc)); err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}
	if err := st.DeletePlaylist(ctx, playlistID.String(), plUpdatedAt(t, ctx, st, playlistID.String())); err != nil {
		t.Fatalf("DeletePlaylist: %v", err)
	}

	groupID := uuid.MustParse("30000000-0000-4000-8000-0000000000aa")
	group := playlistgroup.Group{
		ID:        groupID.String(),
		Slug:      "tombstoned-ingest-group",
		Title:     "Group referencing a deleted playlist",
		Playlists: []string{"https://origin.example/playlist.json"},
	}
	err := st.CreatePlaylistGroup(ctx, &store.PlaylistGroupInput{
		ID:        groupID,
		Slug:      group.Slug,
		Raw:       rawDoc(t, group),
		Playlists: []store.IngestedPlaylist{{ID: playlistID, Slug: doc.Slug, Raw: rawDoc(t, doc)}},
	})
	if !errors.Is(err, store.ErrDocumentDeleted) {
		t.Fatalf("CreatePlaylistGroup with tombstoned member: got %v, want ErrDocumentDeleted", err)
	}
}

// The remote-URI cache must resolve on its own, follow a re-point, tolerate a URI far past the btree
// index limit, and never outlive the playlist it points at — a row naming a deleted (tombstoned) id would
// send a later ingest back to a row that must not be resurrected.
func TestIntegration_PlaylistSources_cacheResolvesRepointsAndCascades(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	const remoteURI = "https://elsewhere.test/remote-playlist.json"
	playlistID := uuid.MustParse("00000000-0000-0000-0000-00000000000b")
	pl := playlist.Playlist{DPVersion: "1.1.0", Title: "Ingested from a remote origin"}

	groupID := uuid.MustParse("00000000-0000-0000-0000-00000000000c")
	groupInput := &store.PlaylistGroupInput{
		ID:   groupID,
		Slug: "sources-group",
		Raw: rawDoc(t, playlistgroup.Group{
			ID:        groupID.String(),
			Slug:      "sources-group",
			Title:     "Sources Group",
			Playlists: []string{remoteURI},
		}),
		Playlists: []store.IngestedPlaylist{
			{ID: playlistID, Slug: "remote-pl", Raw: rawDoc(t, pl), SourceURI: remoteURI},
		},
	}
	if err := st.CreatePlaylistGroup(ctx, groupInput); err != nil {
		t.Fatal(err)
	}

	rec, err := st.GetPlaylistBySourceURI(ctx, remoteURI)
	if err != nil {
		t.Fatalf("ingesting a remote reference must record its URI mapping: %v", err)
	}
	if rec.ID != playlistID {
		t.Fatalf("mapping resolved to %s, want %s", rec.ID, playlistID)
	}

	// An unmapped URI is a plain miss, not an error the caller has to special-case.
	if _, err := st.GetPlaylistBySourceURI(ctx, "https://elsewhere.test/never-seen.json"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown URI should be ErrNotFound, got %v", err)
	}

	// The key is sha256(uri), so a URI far past the ~2704-byte btree limit must still record. Keyed on the
	// text it raised "index row size exceeds maximum" and turned client input into a 500.
	longURI := "https://elsewhere.test/" + strings.Repeat("abcdefgh", 500) + ".json"
	longID := uuid.MustParse("00000000-0000-0000-0000-00000000000d")
	longGroupID := uuid.MustParse("00000000-0000-0000-0000-00000000000e")
	if err := st.CreatePlaylistGroup(ctx, &store.PlaylistGroupInput{
		ID:   longGroupID,
		Slug: "long-uri-group",
		Raw: rawDoc(t, playlistgroup.Group{
			ID: longGroupID.String(), Slug: "long-uri-group", Title: "Long", Playlists: []string{longURI},
		}),
		Playlists: []store.IngestedPlaylist{
			{ID: longID, Slug: "long-pl", Raw: rawDoc(t, playlist.Playlist{DPVersion: "1.1.0", Title: "Long"}), SourceURI: longURI},
		},
	}); err != nil {
		t.Fatalf("a long URI must not break the source mapping: %v", err)
	}
	if rec, err := st.GetPlaylistBySourceURI(ctx, longURI); err != nil || rec.ID != longID {
		t.Fatalf("long URI should resolve to %s, got %v (%v)", longID, rec, err)
	}

	// Re-pointing: the cache is last-known-good, so a later resolution of the same URI replaces the old
	// one. Under the previous first-wins rule this silently kept serving the abandoned playlist.
	repointID := uuid.MustParse("00000000-0000-0000-0000-00000000000f")
	repointGroupID := uuid.MustParse("00000000-0000-0000-0000-000000000010")
	if err := st.CreatePlaylistGroup(ctx, &store.PlaylistGroupInput{
		ID:   repointGroupID,
		Slug: "repoint-group",
		Raw: rawDoc(t, playlistgroup.Group{
			ID: repointGroupID.String(), Slug: "repoint-group", Title: "Repoint", Playlists: []string{remoteURI},
		}),
		Playlists: []store.IngestedPlaylist{
			{ID: repointID, Slug: "repointed-pl", Raw: rawDoc(t, playlist.Playlist{DPVersion: "1.1.0", Title: "Repointed"}), SourceURI: remoteURI},
		},
	}); err != nil {
		t.Fatal(err)
	}
	if rec, err := st.GetPlaylistBySourceURI(ctx, remoteURI); err != nil || rec.ID != repointID {
		t.Fatalf("cache should hold the latest resolution %s, got %v (%v)", repointID, rec, err)
	}

	// Deleting the playlist must take its cache row with it (ON DELETE CASCADE), so a later ingest of the
	// same URI goes through the full create bar and meets the tombstone guard rather than silently
	// relinking a retired id. Delete the playlist the row currently points at — after the re-point above
	// that is repointID, not the original: asserting against the original would have passed only because
	// the row it named no longer existed, which proves nothing about the cascade.
	if err := st.DeletePlaylist(ctx, repointID.String(), plUpdatedAt(t, ctx, st, repointID.String())); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetPlaylistBySourceURI(ctx, remoteURI); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cache row must not outlive the playlist it points at, got %v", err)
	}
}

// A playlist owner's deletion must not be blockable by someone else's document.
//
// Creation is open, so any caller can self-sign a group referencing an already-stored playlist they do
// not own. While the membership FK was ON DELETE RESTRICT that reference vetoed the playlist owner's own
// signed DELETE, and the documented remedy — remove the references — was impossible for them, since only
// the group's owner may edit or delete the group. Migration 000005 makes membership CASCADE instead.
//
// The rows are a derived index, not owned content: a group is served from its stored signed document, so
// cascading leaves that document byte-for-byte intact (asserted below) and only drops the membership row
// that backs the `?playlist-group=` filter.
func TestIntegration_PlaylistGroup_deletingReferencedPlaylistCascadesMembership(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	playlistID := uuid.MustParse("00000000-0000-0000-0000-000000000005")
	pl := playlist.Playlist{DPVersion: "1.1.0", Title: "Referenced"}
	if err := st.CreatePlaylist(ctx, playlistID, "referenced-pl", rawDoc(t, &pl)); err != nil {
		t.Fatal(err)
	}

	// The group is a third party's: it references the playlist without its owner's involvement, which is
	// exactly the situation that used to deadlock.
	groupID := uuid.MustParse("00000000-0000-0000-0000-000000000006")
	groupRaw := rawDoc(t, playlistgroup.Group{
		ID:        groupID.String(),
		Slug:      "referencing-group",
		Title:     "Referencing Group",
		Playlists: []string{"referenced-pl"},
	})
	groupInput := &store.PlaylistGroupInput{
		ID:   groupID,
		Slug: "referencing-group",
		Raw:  groupRaw,
		Playlists: []store.IngestedPlaylist{
			{ID: playlistID, Slug: "referenced-pl", Raw: rawDoc(t, pl)},
		},
	}
	if err := st.CreatePlaylistGroup(ctx, groupInput); err != nil {
		t.Fatal(err)
	}

	if err := st.DeletePlaylist(ctx, playlistID.String(), plUpdatedAt(t, ctx, st, playlistID.String())); err != nil {
		t.Fatalf("a playlist owner's delete must not be blocked by a third party's reference: %v", err)
	}

	if _, err := st.GetPlaylist(ctx, playlistID.String()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("playlist should be gone after delete, got %v", err)
	}

	// The referencing group survives untouched. Its document is signed, so the feed must not edit it: it
	// still lists the deleted playlist's URI, and its bytes must be unchanged.
	grp, err := st.GetPlaylistGroup(ctx, groupID.String())
	if err != nil {
		t.Fatalf("third party's group must survive the playlist delete: %v", err)
	}
	if !jsonEqual(t, grp.Raw, groupRaw) {
		t.Fatalf("group document must be untouched by another resource's deletion:\n got %s\nwant %s", grp.Raw, groupRaw)
	}

	// The membership row is gone, so the group filter no longer offers a playlist that no longer exists.
	items, _, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{
		Limit:               10,
		Sort:                store.SortAsc,
		PlaylistGroupFilter: groupID.String(),
	})
	if err != nil {
		t.Fatalf("list by group filter: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("membership should have cascaded away, got %d rows", len(items))
	}
}

// =============================================================================
// Channel Contract Tests
// =============================================================================

func TestIntegration_ChannelCRUD_and_List(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Setup: Create playlists
	playlistID1 := uuid.MustParse("88888888-8888-8888-8888-888888888888")
	playlistID2 := uuid.MustParse("99999999-9999-9999-9999-999999999999")
	pl1 := playlist.Playlist{DPVersion: "1.1.0", Title: "Ch P1"}
	pl2 := playlist.Playlist{DPVersion: "1.1.0", Title: "Ch P2"}
	if err := st.CreatePlaylist(ctx, playlistID1, "ch-playlist-1", rawDoc(t, &pl1)); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePlaylist(ctx, playlistID2, "ch-playlist-2", rawDoc(t, &pl2)); err != nil {
		t.Fatal(err)
	}

	// Create channel
	channelID := uuid.MustParse("aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee")
	channelSlug := "test-channel"
	channelBody := channels.Channel{
		ID:        channelID.String(),
		Slug:      channelSlug,
		Title:     "Test Channel",
		Version:   "1.0.0",
		Playlists: []string{"ch-playlist-1", "ch-playlist-2"},
	}
	channelInput := &store.ChannelInput{
		ID:   channelID,
		Slug: channelSlug,
		Raw:  rawDoc(t, channelBody),
		Playlists: []store.IngestedPlaylist{
			{ID: playlistID1, Slug: "ch-playlist-1", Raw: rawDoc(t, pl1)},
			{ID: playlistID2, Slug: "ch-playlist-2", Raw: rawDoc(t, pl2)},
		},
	}
	if err := st.CreateChannel(ctx, channelInput); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	// Get by UUID - comprehensive assertions
	byID, err := st.GetChannel(ctx, channelID.String())
	if err != nil {
		t.Fatal(err)
	}
	if byID.ID != channelID {
		t.Fatalf("Get by id: ID mismatch, want %v, got %v", channelID, byID.ID)
	}
	if byID.Slug != channelSlug {
		t.Fatalf("Get by id: slug mismatch, want %q, got %q", channelSlug, byID.Slug)
	}
	if byID.Body.Title != "Test Channel" {
		t.Fatalf("Get by id: title mismatch, want %q, got %q", "Test Channel", byID.Body.Title)
	}
	if len(byID.Body.Playlists) != 2 {
		t.Fatalf("Get by id: playlists length mismatch, want 2, got %d", len(byID.Body.Playlists))
	}
	if byID.Body.Version != "1.0.0" {
		t.Fatalf("Get by id: version mismatch, want %q, got %q", "1.0.0", byID.Body.Version)
	}
	if byID.CreatedAt.IsZero() {
		t.Fatal("Get by id: CreatedAt should be set")
	}
	if byID.UpdatedAt.IsZero() {
		t.Fatal("Get by id: UpdatedAt should be set")
	}
	createdAt := byID.CreatedAt

	// Get by slug - verify same record
	bySlug, err := st.GetChannel(ctx, channelSlug)
	if err != nil {
		t.Fatal(err)
	}
	if bySlug.ID != channelID {
		t.Fatalf("Get by slug: ID mismatch, want %v, got %v", channelID, bySlug.ID)
	}
	assertDeepEqual(t, "Get by slug body", byID.Body, bySlug.Body)

	// Verify membership before update
	membersBefore, err := st.ListPlaylistsInChannel(ctx, channelID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(membersBefore) != 2 {
		t.Fatalf("members before update: want 2, got %d", len(membersBefore))
	}

	// List includes our channel
	recs, _, err := st.ListChannels(ctx, &store.ListPlaylistsParams{
		Limit:  10,
		Cursor: "",
		Sort:   store.SortAsc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(recs) != 1 {
		t.Fatalf("list len: want 1, got %d", len(recs))
	}

	// Update channel (reduce from 2 playlists to 1)
	time.Sleep(5 * time.Millisecond)
	updatedChannelBody := channels.Channel{
		ID:        channelID.String(),
		Slug:      channelSlug,
		Title:     "Updated Channel",
		Version:   "1.0.0",
		Playlists: []string{"ch-playlist-1"},
	}
	updatedInput := &store.ChannelInput{
		Raw: rawDoc(t, updatedChannelBody),
		Playlists: []store.IngestedPlaylist{
			{ID: playlistID1, Slug: "ch-playlist-1", Raw: rawDoc(t, pl1)},
		},
	}
	if err := st.UpdateChannel(ctx, channelSlug, updatedInput, chUpdatedAt(t, ctx, st, channelSlug)); err != nil {
		t.Fatalf("UpdateChannel: %v", err)
	}

	// Verify update worked - comprehensive checks
	afterUpdate, err := st.GetChannel(ctx, channelID.String())
	if err != nil {
		t.Fatalf("GetChannel after update: %v", err)
	}
	if afterUpdate.Body.Title != "Updated Channel" {
		t.Fatalf("after update: title mismatch, want %q, got %q", "Updated Channel", afterUpdate.Body.Title)
	}
	if len(afterUpdate.Body.Playlists) != 1 {
		t.Fatalf("after update: playlists length, want 1, got %d", len(afterUpdate.Body.Playlists))
	}
	if afterUpdate.Body.Playlists[0] != "ch-playlist-1" {
		t.Fatalf("after update: playlist mismatch, want %q, got %q", "ch-playlist-1", afterUpdate.Body.Playlists[0])
	}
	if afterUpdate.CreatedAt != createdAt {
		t.Fatalf("after update: CreatedAt changed from %v to %v", createdAt, afterUpdate.CreatedAt)
	}
	if !afterUpdate.UpdatedAt.After(afterUpdate.CreatedAt) {
		t.Fatalf("after update: UpdatedAt should be after CreatedAt")
	}

	// Verify membership was actually replaced
	membersAfter, err := st.ListPlaylistsInChannel(ctx, channelID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(membersAfter) != 1 {
		t.Fatalf("members after update: want 1, got %d", len(membersAfter))
	}
	if membersAfter[0].ID != playlistID1 {
		t.Fatalf("members after update: want playlist %v, got %v", playlistID1, membersAfter[0].ID)
	}

	// Delete removes row
	if err := st.DeleteChannel(ctx, channelID.String(), chUpdatedAt(t, ctx, st, channelID.String())); err != nil {
		t.Fatal(err)
	}
	_, err = st.GetChannel(ctx, channelID.String())
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: expected ErrNotFound, got %v", err)
	}

	// Verify membership rows were cascade-deleted
	_, err = st.ListPlaylistsInChannel(ctx, channelID.String())
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: membership should return ErrNotFound, got %v", err)
	}
}

func TestIntegration_GetChannel_notFound(t *testing.T) {
	st := newStore(t)
	_, err := st.GetChannel(context.Background(), uuid.New().String())
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestIntegration_UpdateChannel_notFound(t *testing.T) {
	st := newStore(t)
	input := &store.ChannelInput{
		Raw:       rawDoc(t, channels.Channel{Title: "Missing"}),
		Playlists: []store.IngestedPlaylist{},
	}
	err := st.UpdateChannel(context.Background(), uuid.New().String(), input, time.Time{})
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestIntegration_DeleteChannel_notFound(t *testing.T) {
	st := newStore(t)
	err := st.DeleteChannel(context.Background(), "missing-channel", time.Time{})
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestIntegration_ListPlaylistsInChannel(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Create playlists
	playlistID1 := uuid.MustParse("aaaaaaaa-1111-1111-1111-111111111111")
	playlistID2 := uuid.MustParse("bbbbbbbb-2222-2222-2222-222222222222")
	pl1 := playlist.Playlist{DPVersion: "1.1.0", Title: "Ch First"}
	pl2 := playlist.Playlist{DPVersion: "1.1.0", Title: "Ch Second"}
	if err := st.CreatePlaylist(ctx, playlistID1, "ch-pl-first", rawDoc(t, &pl1)); err != nil {
		t.Fatal(err)
	}
	if err := st.CreatePlaylist(ctx, playlistID2, "ch-pl-second", rawDoc(t, &pl2)); err != nil {
		t.Fatal(err)
	}

	// Create channel with ordered membership
	channelID := uuid.MustParse("cccccccc-3333-3333-3333-333333333333")
	channelInput := &store.ChannelInput{
		ID:   channelID,
		Slug: "membership-channel",
		Raw: rawDoc(t, channels.Channel{
			ID:        channelID.String(),
			Slug:      "membership-channel",
			Title:     "Membership Test",
			Version:   "1.0.0",
			Playlists: []string{"ch-pl-first", "ch-pl-second"},
		}),
		Playlists: []store.IngestedPlaylist{
			{ID: playlistID1, Slug: "ch-pl-first", Raw: rawDoc(t, pl1)},
			{ID: playlistID2, Slug: "ch-pl-second", Raw: rawDoc(t, pl2)},
		},
	}
	if err := st.CreateChannel(ctx, channelInput); err != nil {
		t.Fatal(err)
	}

	// List members preserves order
	members, err := st.ListPlaylistsInChannel(ctx, channelID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 2 {
		t.Fatalf("expected 2 members, got %d", len(members))
	}
	if members[0].ID != playlistID1 {
		t.Fatalf("first member ID mismatch: got %v", members[0].ID)
	}
	if members[1].ID != playlistID2 {
		t.Fatalf("second member ID mismatch: got %v", members[1].ID)
	}
	assertDeepEqual(t, "first member body", pl1, members[0].Body)
	assertDeepEqual(t, "second member body", pl2, members[1].Body)
}

func TestIntegration_ListPlaylistsInChannel_notFound(t *testing.T) {
	st := newStore(t)
	_, err := st.ListPlaylistsInChannel(context.Background(), "missing-channel")
	if err == nil || !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("got %v", err)
	}
}

func TestIntegration_ListChannels_pagination(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Create two channels
	channelID1 := uuid.MustParse("dddddddd-4444-4444-4444-444444444444")
	channelID2 := uuid.MustParse("eeeeeeee-5555-5555-5555-555555555555")
	channel1 := &store.ChannelInput{
		ID:   channelID1,
		Slug: "channel-first",
		Raw: rawDoc(t, channels.Channel{
			ID:        channelID1.String(),
			Slug:      "channel-first",
			Title:     "First Channel",
			Version:   "1.0.0",
			Playlists: []string{},
		}),
		Playlists: []store.IngestedPlaylist{},
	}
	channel2 := &store.ChannelInput{
		ID:   channelID2,
		Slug: "channel-second",
		Raw: rawDoc(t, channels.Channel{
			ID:        channelID2.String(),
			Slug:      "channel-second",
			Title:     "Second Channel",
			Version:   "1.0.0",
			Playlists: []string{},
		}),
		Playlists: []store.IngestedPlaylist{},
	}
	if err := st.CreateChannel(ctx, channel1); err != nil {
		t.Fatal(err)
	}
	time.Sleep(5 * time.Millisecond)
	if err := st.CreateChannel(ctx, channel2); err != nil {
		t.Fatal(err)
	}

	// First page: one row, non-empty cursor
	page1, cur, err := st.ListChannels(ctx, &store.ListPlaylistsParams{
		Limit:  1,
		Cursor: "",
		Sort:   store.SortAsc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page1) != 1 || cur == "" {
		t.Fatalf("page1 len=%d cur=%q", len(page1), cur)
	}

	// Second page: other row, empty cursor
	page2, cur2, err := st.ListChannels(ctx, &store.ListPlaylistsParams{
		Limit:  1,
		Cursor: cur,
		Sort:   store.SortAsc,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page2) != 1 {
		t.Fatalf("page2 len %d", len(page2))
	}
	if page1[0].Body.Title == page2[0].Body.Title {
		t.Fatal("expected different rows across pages")
	}
	if cur2 != "" {
		t.Fatalf("expected empty next cursor on last page, got %q", cur2)
	}
}

// =============================================================================
// Channel Edge Case Tests
// =============================================================================

func TestIntegration_Channel_emptyPlaylists(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Create channel with no playlists
	channelID := uuid.MustParse("00000000-0000-0000-0000-000000000007")
	emptyChannelInput := &store.ChannelInput{
		ID:   channelID,
		Slug: "empty-channel",
		Raw: rawDoc(t, channels.Channel{
			ID:        channelID.String(),
			Slug:      "empty-channel",
			Title:     "Empty Channel",
			Version:   "1.0.0",
			Playlists: []string{},
		}),
		Playlists: []store.IngestedPlaylist{},
	}
	if err := st.CreateChannel(ctx, emptyChannelInput); err != nil {
		t.Fatalf("CreateChannel with empty playlists: %v", err)
	}

	// Verify it was created
	rec, err := st.GetChannel(ctx, channelID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(rec.Body.Playlists) != 0 {
		t.Fatalf("expected 0 playlists, got %d", len(rec.Body.Playlists))
	}

	// List members should return empty but not error
	members, err := st.ListPlaylistsInChannel(ctx, channelID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 0 {
		t.Fatalf("expected 0 members, got %d", len(members))
	}

	// Update back to empty after adding (test removal)
	playlistID := uuid.MustParse("00000000-0000-0000-0000-000000000008")
	pl := playlist.Playlist{DPVersion: "1.1.0", Title: "Temporary"}
	if err := st.CreatePlaylist(ctx, playlistID, "temporary", rawDoc(t, &pl)); err != nil {
		t.Fatal(err)
	}

	// Add playlist
	updateInput := &store.ChannelInput{
		Raw: rawDoc(t, channels.Channel{
			ID:        channelID.String(),
			Slug:      "empty-channel",
			Title:     "Not Empty",
			Version:   "1.0.0",
			Playlists: []string{"temporary"},
		}),
		Playlists: []store.IngestedPlaylist{
			{ID: playlistID, Slug: "temporary", Raw: rawDoc(t, pl)},
		},
	}
	if err := st.UpdateChannel(ctx, channelID.String(), updateInput, chUpdatedAt(t, ctx, st, channelID.String())); err != nil {
		t.Fatalf("UpdateChannel to add playlists: %v", err)
	}

	// Verify added
	membersAfterAdd, _ := st.ListPlaylistsInChannel(ctx, channelID.String())
	if len(membersAfterAdd) != 1 {
		t.Fatalf("after add: expected 1 member, got %d", len(membersAfterAdd))
	}

	// Remove all
	emptyAgain := &store.ChannelInput{
		Raw: rawDoc(t, channels.Channel{
			ID:        channelID.String(),
			Slug:      "empty-channel",
			Title:     "Empty Again",
			Version:   "1.0.0",
			Playlists: []string{},
		}),
		Playlists: []store.IngestedPlaylist{},
	}
	if err := st.UpdateChannel(ctx, channelID.String(), emptyAgain, chUpdatedAt(t, ctx, st, channelID.String())); err != nil {
		t.Fatalf("UpdateChannel to remove all playlists: %v", err)
	}

	// Verify all removed
	finalMembers, err := st.ListPlaylistsInChannel(ctx, channelID.String())
	if err != nil {
		t.Fatal(err)
	}
	if len(finalMembers) != 0 {
		t.Fatalf("after removing all: expected 0 members, got %d", len(finalMembers))
	}
}

// Channel counterpart of TestIntegration_PlaylistGroup_deletingReferencedPlaylistCascadesMembership:
// a channel owned by someone else must not be able to block the playlist owner's delete either.
func TestIntegration_Channel_deletingReferencedPlaylistCascadesMembership(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	playlistID := uuid.MustParse("00000000-0000-0000-0000-000000000009")
	pl := playlist.Playlist{DPVersion: "1.1.0", Title: "Referenced by Channel"}
	if err := st.CreatePlaylist(ctx, playlistID, "ref-by-channel", rawDoc(t, &pl)); err != nil {
		t.Fatal(err)
	}

	channelID := uuid.MustParse("00000000-0000-0000-0000-00000000000a")
	channelRaw := rawDoc(t, channels.Channel{
		ID:        channelID.String(),
		Slug:      "referencing-channel",
		Title:     "Referencing Channel",
		Version:   "1.0.0",
		Playlists: []string{"ref-by-channel"},
	})
	channelInput := &store.ChannelInput{
		ID:   channelID,
		Slug: "referencing-channel",
		Raw:  channelRaw,
		Playlists: []store.IngestedPlaylist{
			{ID: playlistID, Slug: "ref-by-channel", Raw: rawDoc(t, pl)},
		},
	}
	if err := st.CreateChannel(ctx, channelInput); err != nil {
		t.Fatal(err)
	}

	if err := st.DeletePlaylist(ctx, playlistID.String(), plUpdatedAt(t, ctx, st, playlistID.String())); err != nil {
		t.Fatalf("a playlist owner's delete must not be blocked by a third party's channel: %v", err)
	}

	if _, err := st.GetPlaylist(ctx, playlistID.String()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("playlist should be gone after delete, got %v", err)
	}

	ch, err := st.GetChannel(ctx, channelID.String())
	if err != nil {
		t.Fatalf("third party's channel must survive the playlist delete: %v", err)
	}
	if !jsonEqual(t, ch.Raw, channelRaw) {
		t.Fatalf("channel document must be untouched by another resource's deletion:\n got %s\nwant %s", ch.Raw, channelRaw)
	}

	items, _, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{
		Limit:         10,
		Sort:          store.SortAsc,
		ChannelFilter: channelID.String(),
	})
	if err != nil {
		t.Fatalf("list by channel filter: %v", err)
	}
	if len(items) != 0 {
		t.Fatalf("membership should have cascaded away, got %d rows", len(items))
	}
}

// =============================================================================
// Registry Contract Tests
// =============================================================================

func TestIntegration_Registry_GetEmpty(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	pubs, chans, err := st.GetChannelRegistry(ctx)
	if err != nil {
		t.Fatalf("GetChannelRegistry: %v", err)
	}
	if len(pubs) != 0 {
		t.Errorf("expected 0 publishers, got %d", len(pubs))
	}
	if len(chans) != 0 {
		t.Errorf("expected 0 channels, got %d", len(chans))
	}
}

func TestIntegration_Registry_ReplaceAndGet(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	pub1ID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	pub2ID := uuid.MustParse("22222222-2222-2222-2222-222222222222")

	publishers := []store.RegistryPublisher{
		{
			ID:       pub1ID,
			Name:     "Publisher One",
			Position: 0,
		},
		{
			ID:       pub2ID,
			Name:     "Publisher Two",
			Position: 1,
		},
	}

	channels := []store.RegistryPublisherChannel{
		{
			ID:          uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
			PublisherID: pub1ID,
			ChannelURL:  "https://example.com/api/v1/channels/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			Position:    0,
		},
		{
			ID:          uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
			PublisherID: pub1ID,
			ChannelURL:  "https://example.com/api/v1/channels/bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
			Position:    1,
		},
		{
			ID:          uuid.MustParse("cccccccc-cccc-cccc-cccc-cccccccccccc"),
			PublisherID: pub2ID,
			ChannelURL:  "https://example.com/api/v1/channels/cccccccc-cccc-cccc-cccc-cccccccccccc",
			Position:    0,
		},
	}

	if err := st.ReplaceChannelRegistry(ctx, publishers, channels); err != nil {
		t.Fatalf("ReplaceChannelRegistry: %v", err)
	}

	// Retrieve and verify
	gotPubs, gotChans, err := st.GetChannelRegistry(ctx)
	if err != nil {
		t.Fatalf("GetChannelRegistry: %v", err)
	}

	if len(gotPubs) != 2 {
		t.Fatalf("expected 2 publishers, got %d", len(gotPubs))
	}
	if len(gotChans) != 3 {
		t.Fatalf("expected 3 channels, got %d", len(gotChans))
	}

	// Verify publisher order
	if gotPubs[0].Name != "Publisher One" || gotPubs[0].Position != 0 {
		t.Errorf("publisher 0: want name='Publisher One' pos=0, got name=%q pos=%d", gotPubs[0].Name, gotPubs[0].Position)
	}
	if gotPubs[1].Name != "Publisher Two" || gotPubs[1].Position != 1 {
		t.Errorf("publisher 1: want name='Publisher Two' pos=1, got name=%q pos=%d", gotPubs[1].Name, gotPubs[1].Position)
	}

	// Verify channels are ordered by publisher and position
	if gotChans[0].PublisherID != pub1ID || gotChans[0].Position != 0 {
		t.Errorf("channel 0: expected pub1 pos=0, got pub=%v pos=%d", gotChans[0].PublisherID, gotChans[0].Position)
	}
	if gotChans[1].PublisherID != pub1ID || gotChans[1].Position != 1 {
		t.Errorf("channel 1: expected pub1 pos=1, got pub=%v pos=%d", gotChans[1].PublisherID, gotChans[1].Position)
	}
	if gotChans[2].PublisherID != pub2ID || gotChans[2].Position != 0 {
		t.Errorf("channel 2: expected pub2 pos=0, got pub=%v pos=%d", gotChans[2].PublisherID, gotChans[2].Position)
	}
}

func TestIntegration_Registry_ReplaceIsAtomic(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	// Insert initial data
	pub1ID := uuid.MustParse("11111111-1111-1111-1111-111111111111")
	publishers := []store.RegistryPublisher{
		{ID: pub1ID, Name: "Initial Publisher", Position: 0},
	}
	channels := []store.RegistryPublisherChannel{
		{
			ID:          uuid.MustParse("aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa"),
			PublisherID: pub1ID,
			ChannelURL:  "https://example.com/api/v1/channels/aaaaaaaa-aaaa-aaaa-aaaa-aaaaaaaaaaaa",
			Position:    0,
		},
	}
	if err := st.ReplaceChannelRegistry(ctx, publishers, channels); err != nil {
		t.Fatalf("initial ReplaceChannelRegistry: %v", err)
	}

	// Replace with new data
	pub2ID := uuid.MustParse("22222222-2222-2222-2222-222222222222")
	newPublishers := []store.RegistryPublisher{
		{ID: pub2ID, Name: "Replaced Publisher", Position: 0},
	}
	newChannels := []store.RegistryPublisherChannel{
		{
			ID:          uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb"),
			PublisherID: pub2ID,
			ChannelURL:  "https://example.com/api/v1/channels/bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb",
			Position:    0,
		},
	}
	if err := st.ReplaceChannelRegistry(ctx, newPublishers, newChannels); err != nil {
		t.Fatalf("replace ReplaceChannelRegistry: %v", err)
	}

	// Verify old data is gone
	gotPubs, gotChans, err := st.GetChannelRegistry(ctx)
	if err != nil {
		t.Fatalf("GetChannelRegistry: %v", err)
	}

	if len(gotPubs) != 1 {
		t.Fatalf("expected 1 publisher after replace, got %d", len(gotPubs))
	}
	if gotPubs[0].Name != "Replaced Publisher" {
		t.Errorf("expected 'Replaced Publisher', got %q", gotPubs[0].Name)
	}

	if len(gotChans) != 1 {
		t.Fatalf("expected 1 channel after replace, got %d", len(gotChans))
	}
	if gotChans[0].ChannelURL != "https://example.com/api/v1/channels/bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb" {
		t.Errorf("unexpected channel URL: %s", gotChans[0].ChannelURL)
	}
}

// rawDoc marshals a typed document into the raw JSON the store persists. Tests build documents as
// typed structs for readability; the store contract takes bytes (see store.PlaylistRecord).
func rawDoc(t testing.TB, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal document: %v", err)
	}
	return b
}

// Optimistic-concurrency helpers: every mutating store call is conditional on the updated_at the caller
// last observed, so these read the row's current value immediately before the call. A test that wants to
// exercise a *stale* expectation passes an explicit older timestamp instead (see the CAS tests).
func plUpdatedAt(t testing.TB, ctx context.Context, st store.Store, idOrSlug string) time.Time {
	t.Helper()
	rec, err := st.GetPlaylist(ctx, idOrSlug)
	if err != nil {
		t.Fatalf("read playlist updated_at for %q: %v", idOrSlug, err)
	}
	return rec.UpdatedAt
}

func grpUpdatedAt(t testing.TB, ctx context.Context, st store.Store, idOrSlug string) time.Time {
	t.Helper()
	rec, err := st.GetPlaylistGroup(ctx, idOrSlug)
	if err != nil {
		t.Fatalf("read playlist_group updated_at for %q: %v", idOrSlug, err)
	}
	return rec.UpdatedAt
}

func chUpdatedAt(t testing.TB, ctx context.Context, st store.Store, idOrSlug string) time.Time {
	t.Helper()
	rec, err := st.GetChannel(ctx, idOrSlug)
	if err != nil {
		t.Fatalf("read channel updated_at for %q: %v", idOrSlug, err)
	}
	return rec.UpdatedAt
}

// TestIntegration_ConditionalWrite_staleUpdatedAt pins the optimistic-concurrency contract that makes
// owner authorization atomic with the write: the executor authorizes a request against the record it
// read, then writes. If anything commits in between, the stale expectation must lose.
//
// The delete cases are the security-relevant half: document ids are client-assigned and reusable, so an
// unconditional delete-by-id could apply a decision made about the former owner's document to a
// different one that now occupies the same id.
func TestIntegration_ConditionalWrite_staleUpdatedAt(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	id := uuid.MustParse("bbbbbbbb-bbbb-bbbb-bbbb-bbbbbbbbbbbb")
	slug := "cas-playlist"
	pl := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "v1",
		Items:     []playlist.PlaylistItem{{ID: uuid.NewString(), Source: "https://x"}},
	}
	if err := st.CreatePlaylist(ctx, id, slug, rawDoc(t, &pl)); err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}

	// Capture the generation an authorization decision would have been made against.
	stale := plUpdatedAt(t, ctx, st, id.String())

	// A concurrent write lands, moving updated_at forward.
	pl.Title = "v2"
	if err := st.UpdatePlaylist(ctx, id.String(), rawDoc(t, &pl), stale); err != nil {
		t.Fatalf("first UpdatePlaylist: %v", err)
	}
	fresh := plUpdatedAt(t, ctx, st, id.String())
	if !fresh.After(stale) {
		t.Fatalf("updated_at did not advance: stale=%v fresh=%v", stale, fresh)
	}

	// The stale-expectation update must be refused, and must not have modified the row.
	pl.Title = "clobber"
	err := st.UpdatePlaylist(ctx, id.String(), rawDoc(t, &pl), stale)
	if !errors.Is(err, store.ErrConcurrentModification) {
		t.Fatalf("stale UpdatePlaylist: want ErrConcurrentModification, got %v", err)
	}
	cur, err := st.GetPlaylist(ctx, id.String())
	if err != nil {
		t.Fatal(err)
	}
	if cur.Body.Title != "v2" {
		t.Fatalf("stale update must not modify the row; title = %q", cur.Body.Title)
	}

	// A stale-expectation delete must be refused too, and must leave the row in place.
	if err := st.DeletePlaylist(ctx, id.String(), stale); !errors.Is(err, store.ErrConcurrentModification) {
		t.Fatalf("stale DeletePlaylist: want ErrConcurrentModification, got %v", err)
	}
	if _, err := st.GetPlaylist(ctx, id.String()); err != nil {
		t.Fatalf("row must survive a refused delete: %v", err)
	}

	// The current generation still succeeds.
	if err := st.DeletePlaylist(ctx, id.String(), fresh); err != nil {
		t.Fatalf("fresh DeletePlaylist: %v", err)
	}
	if _, err := st.GetPlaylist(ctx, id.String()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("after delete: %v", err)
	}

	// A conditional write against a row that no longer exists is ErrNotFound, not a concurrency error.
	if err := st.DeletePlaylist(ctx, id.String(), fresh); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("delete of missing row: want ErrNotFound, got %v", err)
	}
}

// TestIntegration_Tombstone_survivesConcurrentDelete pins the race the sequential test cannot reach.
//
// The tombstone guard used to be check-then-act: a replaying create read no tombstone, then blocked on the
// unique-key conflict with the row a concurrent delete was removing. When that delete committed — writing
// its tombstone — the blocked insert proceeded against a now-empty key and succeeded, never re-reading the
// tombstone, resurrecting the document immediately after a successful owner delete. Both paths now take a
// transaction-scoped advisory lock on (resource_type, id) before the check, so the two serialize.
//
// The interleaving is timing-dependent, so this runs the pair repeatedly and asserts the invariant that
// actually matters: a delete and a re-create of the same id can never both succeed.
func TestIntegration_Tombstone_survivesConcurrentDelete(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	pl := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "racy",
		Items:     []playlist.PlaylistItem{{ID: uuid.NewString(), Source: "https://x"}},
	}

	const rounds = 25
	for i := range rounds {
		id := uuid.New()
		slug := fmt.Sprintf("race-%d-%s", i, id.String()[:8])
		if err := st.CreatePlaylist(ctx, id, slug, rawDoc(t, &pl)); err != nil {
			t.Fatalf("seed CreatePlaylist: %v", err)
		}
		updatedAt := plUpdatedAt(t, ctx, st, id.String())

		var wg sync.WaitGroup
		var delErr, createErr error
		wg.Add(2)
		go func() {
			defer wg.Done()
			delErr = st.DeletePlaylist(ctx, id.String(), updatedAt)
		}()
		go func() {
			defer wg.Done()
			// The replay: the same id, the same still-valid public bytes, a different slug.
			createErr = st.CreatePlaylist(ctx, id, slug+"-replay", rawDoc(t, &pl))
		}()
		wg.Wait()

		if delErr == nil && createErr == nil {
			t.Fatalf("round %d: delete and re-create both succeeded — the id was resurrected", i)
		}
		if delErr == nil {
			// Whatever the interleaving, a committed delete must leave the resource gone.
			if _, err := st.GetPlaylist(ctx, id.String()); !errors.Is(err, store.ErrNotFound) {
				t.Fatalf("round %d: resource survived a successful delete: %v", i, err)
			}
		}
	}
}

// TestIntegration_Tombstone_blocksIDReuse pins the resurrect defense.
//
// Create is intentionally open, so nothing stops someone re-POSTing a document they saw earlier — its
// owner signatures are public and still valid. Retiring the id on delete is what makes that replay fail,
// and it works because `id` is inside the signed payload and so cannot be rewritten on a replayed body.
func TestIntegration_Tombstone_blocksIDReuse(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	id := uuid.MustParse("cccccccc-cccc-cccc-cccc-ccccccccccc1")
	pl := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "tombstone me",
		Items:     []playlist.PlaylistItem{{ID: uuid.NewString(), Source: "https://x"}},
	}
	if err := st.CreatePlaylist(ctx, id, "tombstone-playlist", rawDoc(t, &pl)); err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}
	if err := st.DeletePlaylist(ctx, id.String(), plUpdatedAt(t, ctx, st, id.String())); err != nil {
		t.Fatalf("DeletePlaylist: %v", err)
	}

	// Replaying the same document (same id) must not resurrect the resource, even under a different slug.
	err := st.CreatePlaylist(ctx, id, "tombstone-playlist-again", rawDoc(t, &pl))
	if !errors.Is(err, store.ErrDocumentDeleted) {
		t.Fatalf("re-create of a deleted id: want ErrDocumentDeleted, got %v", err)
	}
	if _, err := st.GetPlaylist(ctx, id.String()); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("resource must stay deleted, got %v", err)
	}

	// A genuine re-publish under a fresh id is unaffected.
	newID := uuid.MustParse("cccccccc-cccc-cccc-cccc-ccccccccccc2")
	if err := st.CreatePlaylist(ctx, newID, "tombstone-playlist-new", rawDoc(t, &pl)); err != nil {
		t.Fatalf("create under a new id must succeed: %v", err)
	}
}

// TestIntegration_Create_liveCollisions pins live create collisions to ErrAlreadyExists.
//
// Create is open, so colliding with a resource that already exists is ordinary client behavior — a retried
// POST, or two publishers reaching for the same slug — not a server fault. Without an explicit sentinel the
// unique violation surfaces as a bare pgx error and the HTTP layer answers 500, which tells the client to
// retry something that can never succeed. Each create path is covered because each has its own statement;
// a missed one silently regresses to 500.
func TestIntegration_Create_liveCollisions(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	pl := playlist.Playlist{
		DPVersion: "1.1.0",
		Title:     "collide",
		Items:     []playlist.PlaylistItem{{ID: uuid.NewString(), Source: "https://x"}},
	}
	plID := uuid.MustParse("dddddddd-dddd-dddd-dddd-ddddddddddd1")
	if err := st.CreatePlaylist(ctx, plID, "collide-playlist", rawDoc(t, &pl)); err != nil {
		t.Fatalf("CreatePlaylist: %v", err)
	}

	t.Run("playlist id", func(t *testing.T) {
		err := st.CreatePlaylist(ctx, plID, "some-other-slug", rawDoc(t, &pl))
		assertAlreadyExists(t, err, "id")
	})

	t.Run("playlist slug", func(t *testing.T) {
		other := uuid.MustParse("dddddddd-dddd-dddd-dddd-ddddddddddd2")
		err := st.CreatePlaylist(ctx, other, "collide-playlist", rawDoc(t, &pl))
		assertAlreadyExists(t, err, "slug")
	})

	groupID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddde1")
	group := playlistgroup.Group{
		ID:        groupID.String(),
		Slug:      "collide-group",
		Title:     "Collide group",
		Playlists: []string{"collide-playlist"},
	}
	if err := st.CreatePlaylistGroup(ctx, &store.PlaylistGroupInput{
		ID:        groupID,
		Slug:      "collide-group",
		Raw:       rawDoc(t, &group),
		Playlists: []store.IngestedPlaylist{},
	}); err != nil {
		t.Fatalf("CreatePlaylistGroup: %v", err)
	}

	t.Run("playlist-group slug", func(t *testing.T) {
		err := st.CreatePlaylistGroup(ctx, &store.PlaylistGroupInput{
			ID:        uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddde2"),
			Slug:      "collide-group",
			Raw:       rawDoc(t, &group),
			Playlists: []store.IngestedPlaylist{},
		})
		assertAlreadyExists(t, err, "slug")
	})

	chID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddf01")
	ch := channels.Channel{
		ID:        chID.String(),
		Slug:      "collide-channel",
		Title:     "Collide Channel",
		Version:   "1.0.0",
		Playlists: []string{"collide-playlist"},
	}
	if err := st.CreateChannel(ctx, &store.ChannelInput{
		ID:        chID,
		Slug:      "collide-channel",
		Raw:       rawDoc(t, ch),
		Playlists: []store.IngestedPlaylist{{ID: plID, Slug: "collide-playlist", Raw: rawDoc(t, &pl)}},
	}); err != nil {
		t.Fatalf("CreateChannel: %v", err)
	}

	t.Run("channel slug", func(t *testing.T) {
		err := st.CreateChannel(ctx, &store.ChannelInput{
			ID:        uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddf02"),
			Slug:      "collide-channel",
			Raw:       rawDoc(t, ch),
			Playlists: []store.IngestedPlaylist{},
		})
		assertAlreadyExists(t, err, "slug")
	})

	// The ingestion batch is the subtle one: ON CONFLICT (id) DO NOTHING absorbs an id that is already
	// stored, but a playlist that is new to this feed can still carry a slug a different live playlist
	// holds, and that unique violation escapes the DO NOTHING.
	t.Run("ingested playlist slug", func(t *testing.T) {
		err := st.CreateChannel(ctx, &store.ChannelInput{
			ID:   uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddf03"),
			Slug: "ingest-collision-channel",
			Raw:  rawDoc(t, ch),
			Playlists: []store.IngestedPlaylist{
				{ID: uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddf04"), Slug: "collide-playlist", Raw: rawDoc(t, &pl)},
			},
		})
		assertAlreadyExists(t, err, "slug")
	})

	// A tombstoned id must keep its own sentinel: both are 409 at the HTTP layer, but only this one means
	// the id can never be used again, so conflating them would tell a client to give up on a retryable
	// create or to retry one that is permanently refused.
	t.Run("tombstone stays distinct", func(t *testing.T) {
		deadID := uuid.MustParse("dddddddd-dddd-dddd-dddd-dddddddddf05")
		if err := st.CreatePlaylist(ctx, deadID, "doomed-playlist", rawDoc(t, &pl)); err != nil {
			t.Fatalf("CreatePlaylist: %v", err)
		}
		if err := st.DeletePlaylist(ctx, deadID.String(), plUpdatedAt(t, ctx, st, deadID.String())); err != nil {
			t.Fatalf("DeletePlaylist: %v", err)
		}
		err := st.CreatePlaylist(ctx, deadID, "doomed-playlist-again", rawDoc(t, &pl))
		if !errors.Is(err, store.ErrDocumentDeleted) {
			t.Fatalf("re-create of a deleted id: want ErrDocumentDeleted, got %v", err)
		}
		if errors.Is(err, store.ErrAlreadyExists) {
			t.Fatalf("tombstone must not also report ErrAlreadyExists, got %v", err)
		}
	})
}

// assertAlreadyExists checks the sentinel and that the message names which uniqueness rule was hit, so an
// operator reading a 409 can tell an id retry from a slug someone else already took.
func assertAlreadyExists(t *testing.T, err error, wantMention string) {
	t.Helper()
	if !errors.Is(err, store.ErrAlreadyExists) {
		t.Fatalf("want ErrAlreadyExists, got %v", err)
	}
	if !strings.Contains(err.Error(), wantMention) {
		t.Fatalf("error should mention %q, got %q", wantMention, err)
	}
}
