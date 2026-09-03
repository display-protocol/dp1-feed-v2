//go:build integration

package httpserver

import (
	"net/http"
	"testing"

	"github.com/display-protocol/dp1-go/playlist"
	"github.com/google/uuid"
)

func TestIntegration_DisplayAtHTTPRoundTrip(t *testing.T) {
	srv := newIntegrationServer(t)

	playlistID := uuid.MustParse("aaaaaaaa-2222-4333-8444-555555555555")
	item1ID := uuid.MustParse("11111111-2222-4333-8444-555555555555")
	item2ID := uuid.MustParse("22222222-3333-4444-8555-666666666666")
	item3ID := uuid.MustParse("33333333-4444-4555-8666-777777777777")

	created := "2020-01-02T03:04:05Z"
	slug := "daily-displayat-round-trip"
	postDisplayAt := "2026-07-21T00:00:00Z"
	putDisplayAt := "2026-07-22T00:00:00Z"
	put2DisplayAt := "2026-07-23T00:00:00Z"

	priv, kid := newCuratorKeypair(t)
	// base identity is fixed across replaces: id, slug, created, and the curator (owner) never change.
	baseDoc := func(title string, item playlist.PlaylistItem) playlist.Playlist {
		return playlist.Playlist{
			DPVersion: "1.1.0",
			ID:        playlistID.String(),
			Slug:      slug,
			Title:     title,
			Created:   created,
			Curators:  curatorEntities(kid),
			Items:     []playlist.PlaylistItem{item},
		}
	}

	postDoc := baseDoc("Daily displayAt round trip", playlist.PlaylistItem{
		ID: item1ID.String(), Source: "https://cdn.example.com/day-1.html", DisplayAt: &postDisplayAt,
	})
	createdPlaylist := mustDoPlaylistJSON(t, srv, http.MethodPost, "/api/v1/playlists", signedPlaylistBody(t, priv, postDoc), http.StatusCreated)
	assertPlaylistDisplayAt(t, "POST response", createdPlaylist, item1ID, postDisplayAt)

	gotPlaylist := mustDoPlaylistJSON(t, srv, http.MethodGet, "/api/v1/playlists/"+slug, nil, http.StatusOK)
	assertPlaylistDisplayAt(t, "GET playlist", gotPlaylist, item1ID, postDisplayAt)
	assertListPlaylistDisplayAt(t, srv, item1ID, postDisplayAt)
	assertIndexedItemDisplayAt(t, srv, item1ID, postDisplayAt)

	putDoc := baseDoc("Daily displayAt replaced", playlist.PlaylistItem{
		ID: item2ID.String(), Source: "https://cdn.example.com/day-2.html", DisplayAt: &putDisplayAt,
	})
	replacedPlaylist := mustDoPlaylistJSON(t, srv, http.MethodPut, "/api/v1/playlists/"+slug, signedPlaylistBody(t, priv, putDoc), http.StatusOK)
	assertPlaylistDisplayAt(t, "PUT response", replacedPlaylist, item2ID, putDisplayAt)
	assertIndexedItemDisplayAt(t, srv, item2ID, putDisplayAt)

	// A second signed PUT stands in for the former PATCH: with no partial-update endpoint, an owner
	// edits by re-signing the full document.
	put2Doc := baseDoc("Daily displayAt replaced again", playlist.PlaylistItem{
		ID: item3ID.String(), Source: "https://cdn.example.com/day-3.html", DisplayAt: &put2DisplayAt,
	})
	replacedAgain := mustDoPlaylistJSON(t, srv, http.MethodPut, "/api/v1/playlists/"+slug, signedPlaylistBody(t, priv, put2Doc), http.StatusOK)
	assertPlaylistDisplayAt(t, "second PUT response", replacedAgain, item3ID, put2DisplayAt)
	assertListPlaylistDisplayAt(t, srv, item3ID, put2DisplayAt)
	assertIndexedItemDisplayAt(t, srv, item3ID, put2DisplayAt)
}

func assertListPlaylistDisplayAt(t *testing.T, srv *Server, itemID uuid.UUID, wantDisplayAt string) {
	t.Helper()
	var page ListResponse[playlist.Playlist]
	mustDoJSON(t, srv, http.MethodGet, "/api/v1/playlists", nil, http.StatusOK, &page)
	if len(page.Items) != 1 {
		t.Fatalf("playlist list: len=%d want 1", len(page.Items))
	}
	assertPlaylistDisplayAt(t, "playlist list", page.Items[0], itemID, wantDisplayAt)
}

func assertIndexedItemDisplayAt(t *testing.T, srv *Server, itemID uuid.UUID, wantDisplayAt string) {
	t.Helper()
	var page ListResponse[playlist.PlaylistItem]
	mustDoJSON(t, srv, http.MethodGet, "/api/v1/playlist-items", nil, http.StatusOK, &page)
	if len(page.Items) != 1 {
		t.Fatalf("playlist item list: len=%d want 1", len(page.Items))
	}
	assertItemDisplayAt(t, "playlist item list", page.Items[0], itemID, wantDisplayAt)

	var one playlist.PlaylistItem
	mustDoJSON(t, srv, http.MethodGet, "/api/v1/playlist-items/"+itemID.String(), nil, http.StatusOK, &one)
	assertItemDisplayAt(t, "GET playlist item", one, itemID, wantDisplayAt)
}

func assertPlaylistDisplayAt(t *testing.T, label string, pl playlist.Playlist, itemID uuid.UUID, wantDisplayAt string) {
	t.Helper()
	if len(pl.Items) != 1 {
		t.Fatalf("%s: item len=%d want 1", label, len(pl.Items))
	}
	assertItemDisplayAt(t, label, pl.Items[0], itemID, wantDisplayAt)
}

func assertItemDisplayAt(t *testing.T, label string, item playlist.PlaylistItem, itemID uuid.UUID, wantDisplayAt string) {
	t.Helper()
	if item.ID != itemID.String() {
		t.Fatalf("%s: item id=%q want %q", label, item.ID, itemID.String())
	}
	if item.DisplayAt == nil {
		t.Fatalf("%s: displayAt is nil, want %q", label, wantDisplayAt)
	}
	if *item.DisplayAt != wantDisplayAt {
		t.Fatalf("%s: displayAt=%q want %q", label, *item.DisplayAt, wantDisplayAt)
	}
}
