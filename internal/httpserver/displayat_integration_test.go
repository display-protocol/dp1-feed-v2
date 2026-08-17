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
	patchDisplayAt := "2026-07-23T00:00:00Z"

	postBody := map[string]any{
		"dpVersion": "1.1.0",
		"id":        playlistID.String(),
		"created":   created,
		"slug":      slug,
		"title":     "Daily displayAt round trip",
		"items": []map[string]any{
			{
				"id":        item1ID.String(),
				"source":    "https://cdn.example.com/day-1.html",
				"displayAt": postDisplayAt,
			},
		},
	}

	createdPlaylist := mustDoPlaylistJSON(t, srv, http.MethodPost, "/api/v1/playlists", postBody, http.StatusCreated)
	assertPlaylistDisplayAt(t, "POST response", createdPlaylist, item1ID, postDisplayAt)

	gotPlaylist := mustDoPlaylistJSON(t, srv, http.MethodGet, "/api/v1/playlists/"+slug, nil, http.StatusOK)
	assertPlaylistDisplayAt(t, "GET playlist", gotPlaylist, item1ID, postDisplayAt)
	assertListPlaylistDisplayAt(t, srv, item1ID, postDisplayAt)
	assertIndexedItemDisplayAt(t, srv, item1ID, postDisplayAt)

	putBody := map[string]any{
		"dpVersion": "1.1.0",
		"slug":      slug,
		"title":     "Daily displayAt replaced",
		"items": []map[string]any{
			{
				"id":        item2ID.String(),
				"source":    "https://cdn.example.com/day-2.html",
				"displayAt": putDisplayAt,
			},
		},
	}
	replacedPlaylist := mustDoPlaylistJSON(t, srv, http.MethodPut, "/api/v1/playlists/"+slug, putBody, http.StatusOK)
	assertPlaylistDisplayAt(t, "PUT response", replacedPlaylist, item2ID, putDisplayAt)
	assertIndexedItemDisplayAt(t, srv, item2ID, putDisplayAt)

	patchBody := map[string]any{
		"items": []map[string]any{
			{
				"id":        item3ID.String(),
				"source":    "https://cdn.example.com/day-3.html",
				"displayAt": patchDisplayAt,
			},
		},
	}
	patchedPlaylist := mustDoPlaylistJSON(t, srv, http.MethodPatch, "/api/v1/playlists/"+slug, patchBody, http.StatusOK)
	assertPlaylistDisplayAt(t, "PATCH response", patchedPlaylist, item3ID, patchDisplayAt)
	assertListPlaylistDisplayAt(t, srv, item3ID, patchDisplayAt)
	assertIndexedItemDisplayAt(t, srv, item3ID, patchDisplayAt)
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
