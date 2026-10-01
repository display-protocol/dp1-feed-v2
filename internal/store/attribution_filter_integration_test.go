//go:build integration

package store_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/display-protocol/dp1-go/extension/channels"
	"github.com/display-protocol/dp1-go/extension/identity"
	"github.com/display-protocol/dp1-go/playlist"
	"github.com/display-protocol/dp1-go/playlistgroup"
	"github.com/google/uuid"

	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// The attribution filters (?curator=, ?publisher=) read the stored document, not the derived owner set.
// These tests pin what each one matches on, that a key appearing in some OTHER field does not match, and
// that a filtered list still pages on the plain created_at keyset.

const (
	keyA = "did:key:zAttribA"
	keyB = "did:key:zAttribB"
)

func createAttributedPlaylist(t *testing.T, ctx context.Context, st store.Store, id, slug string, curators []identity.Entity) {
	t.Helper()
	pid := uuid.MustParse(id)
	raw := rawDoc(t, playlist.Playlist{
		DPVersion: "1.1.0", ID: pid.String(), Slug: slug, Title: slug,
		Items: []playlist.PlaylistItem{}, Curators: curators,
	})
	if err := st.CreatePlaylist(ctx, pid, slug, raw); err != nil {
		t.Fatalf("create playlist %s: %v", slug, err)
	}
	// created_at is the page key; keep rows distinct so the order under test is deterministic.
	time.Sleep(2 * time.Millisecond)
}

func playlistSlugs(recs []store.PlaylistRecord) []string {
	out := make([]string, 0, len(recs))
	for _, r := range recs {
		out = append(out, r.Slug)
	}
	return out
}

func TestIntegration_ListPlaylists_curatorFilter(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	createAttributedPlaylist(t, ctx, st, "a1000000-0000-4000-8000-000000000001", "a-only", []identity.Entity{{Name: "A", Key: keyA}})
	createAttributedPlaylist(t, ctx, st, "a1000000-0000-4000-8000-000000000002", "b-only", []identity.Entity{{Name: "B", Key: keyB}})
	createAttributedPlaylist(t, ctx, st, "a1000000-0000-4000-8000-000000000003", "a-and-b", []identity.Entity{{Name: "B", Key: keyB}, {Name: "A", Key: keyA}})
	// keyA appears only as a display name: not a key, so it must not match.
	createAttributedPlaylist(t, ctx, st, "a1000000-0000-4000-8000-000000000004", "a-as-name", []identity.Entity{{Name: keyA, Key: keyB}})
	// Undeclared: no curators at all, so no curator filter can match it.
	createAttributedPlaylist(t, ctx, st, "a1000000-0000-4000-8000-000000000005", "undeclared", nil)
	// Differs from keyA only in case / surrounding space: matching is exact.
	createAttributedPlaylist(t, ctx, st, "a1000000-0000-4000-8000-000000000006", "case-variant", []identity.Entity{{Name: "A", Key: "did:key:zattriba"}})
	createAttributedPlaylist(t, ctx, st, "a1000000-0000-4000-8000-000000000007", "padded", []identity.Entity{{Name: "A", Key: " " + keyA + " "}})

	tests := []struct {
		name    string
		curator string
		sort    store.SortOrder
		want    []string
	}{
		{name: "matches any curators entry by key", curator: keyA, sort: store.SortAsc, want: []string{"a-only", "a-and-b"}},
		{name: "desc", curator: keyA, sort: store.SortDesc, want: []string{"a-and-b", "a-only"}},
		{name: "other key", curator: keyB, sort: store.SortAsc, want: []string{"b-only", "a-and-b", "a-as-name"}},
		{name: "query value is trimmed by the store", curator: "  " + keyA + "\t", sort: store.SortAsc, want: []string{"a-only", "a-and-b"}},
		{name: "unknown key is an empty page", curator: "did:key:zNobody", sort: store.SortAsc, want: []string{}},
		{name: "json metacharacters are a plain value", curator: `"}]`, sort: store.SortAsc, want: []string{}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, next, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{Limit: 10, Sort: tc.sort, CuratorFilter: tc.curator})
			if err != nil {
				t.Fatal(err)
			}
			if next != "" {
				t.Fatalf("unexpected next cursor %q", next)
			}
			assertDeepEqual(t, "slugs", tc.want, playlistSlugs(got))
		})
	}

	t.Run("pages on the created_at keyset", func(t *testing.T) {
		page1, cur, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{Limit: 2, Sort: store.SortAsc, CuratorFilter: keyB})
		if err != nil {
			t.Fatal(err)
		}
		if cur == "" {
			t.Fatal("want a next cursor after the first of two pages")
		}
		page2, cur2, err := st.ListPlaylists(ctx, &store.ListPlaylistsParams{Limit: 2, Sort: store.SortAsc, Cursor: cur, CuratorFilter: keyB})
		if err != nil {
			t.Fatal(err)
		}
		if cur2 != "" {
			t.Fatalf("want no cursor on the last page, got %q", cur2)
		}
		assertDeepEqual(t, "pages", []string{"b-only", "a-and-b", "a-as-name"}, append(playlistSlugs(page1), playlistSlugs(page2)...))
	})

	t.Run("refused with a container filter", func(t *testing.T) {
		for _, p := range []*store.ListPlaylistsParams{
			{Limit: 10, CuratorFilter: keyA, ChannelFilter: "some-channel"},
			{Limit: 10, CuratorFilter: keyA, PlaylistGroupFilter: "some-group"},
		} {
			if _, _, err := st.ListPlaylists(ctx, p); !errors.Is(err, store.ErrInvalidListFilter) {
				t.Fatalf("want ErrInvalidListFilter for %+v, got %v", p, err)
			}
		}
	})
}

func TestIntegration_ListPlaylistGroups_curatorFilter(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	for _, g := range []struct{ id, slug, curator string }{
		{"a2000000-0000-4000-8000-000000000001", "by-a", keyA},
		{"a2000000-0000-4000-8000-000000000002", "by-name", "Gallery Curator"},
		{"a2000000-0000-4000-8000-000000000003", "no-curator", ""},
		{"a2000000-0000-4000-8000-000000000004", "also-a", keyA},
	} {
		gid := uuid.MustParse(g.id)
		if err := st.CreatePlaylistGroup(ctx, &store.PlaylistGroupInput{
			ID: gid, Slug: g.slug,
			Raw: rawDoc(t, playlistgroup.Group{ID: gid.String(), Slug: g.slug, Title: g.slug, Curator: g.curator, Playlists: []string{}, Created: "2026-01-01T00:00:00Z"}),
		}); err != nil {
			t.Fatalf("create group %s: %v", g.slug, err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	for _, tc := range []struct {
		name, curator string
		want          []string
	}{
		{name: "key-shaped curator", curator: keyA, want: []string{"by-a", "also-a"}},
		{name: "display-name curator", curator: "Gallery Curator", want: []string{"by-name"}},
		{name: "exact, case-sensitive", curator: "gallery curator", want: []string{}},
		{name: "no filter lists all", curator: "", want: []string{"by-a", "by-name", "no-curator", "also-a"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := st.ListPlaylistGroups(ctx, &store.ListPlaylistsParams{Limit: 10, Sort: store.SortAsc, CuratorFilter: tc.curator})
			if err != nil {
				t.Fatal(err)
			}
			slugs := make([]string, 0, len(got))
			for _, r := range got {
				slugs = append(slugs, r.Slug)
			}
			assertDeepEqual(t, "slugs", tc.want, slugs)
		})
	}
}

func TestIntegration_ListChannels_curatorAndPublisherFilters(t *testing.T) {
	st := newStore(t)
	ctx := context.Background()

	for _, c := range []struct {
		id, slug  string
		curators  []identity.Entity
		publisher *identity.Entity
	}{
		{"a3000000-0000-4000-8000-000000000001", "pub-a", nil, &identity.Entity{Name: "A", Key: keyA}},
		{"a3000000-0000-4000-8000-000000000002", "pub-b-cur-a", []identity.Entity{{Name: "A", Key: keyA}}, &identity.Entity{Name: "B", Key: keyB}},
		{"a3000000-0000-4000-8000-000000000003", "pub-a-cur-b", []identity.Entity{{Name: "B", Key: keyB}}, &identity.Entity{Name: "A", Key: keyA}},
		{"a3000000-0000-4000-8000-000000000004", "undeclared", nil, nil},
	} {
		cid := uuid.MustParse(c.id)
		if err := st.CreateChannel(ctx, &store.ChannelInput{
			ID: cid, Slug: c.slug,
			Raw: rawDoc(t, channels.Channel{ID: cid.String(), Slug: c.slug, Title: c.slug, Version: "1.0.0", Playlists: []string{}, Curators: c.curators, Publisher: c.publisher}),
		}); err != nil {
			t.Fatalf("create channel %s: %v", c.slug, err)
		}
		time.Sleep(2 * time.Millisecond)
	}

	for _, tc := range []struct {
		name, curator, publisher string
		want                     []string
	}{
		// Each filter reads only its own field: a publisher key is not a curator and vice versa.
		{name: "publisher", publisher: keyA, want: []string{"pub-a", "pub-a-cur-b"}},
		{name: "curator", curator: keyA, want: []string{"pub-b-cur-a"}},
		{name: "both are ANDed", curator: keyB, publisher: keyA, want: []string{"pub-a-cur-b"}},
		{name: "both, no row satisfies both", curator: keyA, publisher: keyA, want: []string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := st.ListChannels(ctx, &store.ListPlaylistsParams{Limit: 10, Sort: store.SortAsc, CuratorFilter: tc.curator, PublisherFilter: tc.publisher})
			if err != nil {
				t.Fatal(err)
			}
			slugs := make([]string, 0, len(got))
			for _, r := range got {
				slugs = append(slugs, r.Slug)
			}
			assertDeepEqual(t, "slugs", tc.want, slugs)
		})
	}
}
