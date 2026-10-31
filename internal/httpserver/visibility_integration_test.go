//go:build integration

package httpserver

import (
	"bytes"
	"crypto/ed25519"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/display-protocol/dp1-go/playlist"
	dp1sign "github.com/display-protocol/dp1-go/sign"
)

// signedVisibilityPlaylist curator-signs a playlist document carrying a top-level visibility member and
// returns the POST body. The document is a map rather than a dp1-go Playlist so a test can set the member
// to any JSON type, which the struct's `*string` could not carry.
func signedVisibilityPlaylist(t *testing.T, priv ed25519.PrivateKey, kid, id, slug, itemID, visibility string) map[string]any {
	t.Helper()
	doc := map[string]any{
		"dpVersion":  "1.1.0",
		"id":         id,
		"created":    "2026-10-28T00:00:00Z",
		"slug":       slug,
		"title":      slug,
		"curators":   curatorEntities(kid),
		"items":      []map[string]string{{"id": itemID, "title": slug, "source": "https://cdn.example.com/" + slug + ".html"}},
		"visibility": visibility,
	}
	unsigned, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	sig, err := dp1sign.SignMultiEd25519(unsigned, priv, playlist.RoleCurator, "2026-10-28T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	doc["signatures"] = []playlist.Signature{sig}
	return doc
}

// TestIntegration_VisibilityHTTP walks an unlisted playlist across every public read: it is accepted and
// stored as signed, served by its id, and absent from every directory and from slug and item lookups,
// while a public sibling stays visible everywhere.
func TestIntegration_VisibilityHTTP(t *testing.T) {
	srv := newIntegrationServer(t)
	priv, kid := newCuratorKeypair(t)

	const (
		unlistedID   = "e1000000-0000-4000-8000-000000000001"
		unlistedItem = "e2000000-0000-4000-8000-000000000001"
		publicID     = "e1000000-0000-4000-8000-000000000002"
		publicItem   = "e2000000-0000-4000-8000-000000000002"
	)
	created := mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists",
		signedVisibilityPlaylist(t, priv, kid, unlistedID, "shared-by-link", unlistedItem, "unlisted"), http.StatusCreated)
	mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists",
		signedVisibilityPlaylist(t, priv, kid, publicID, "for-everyone", publicItem, "public"), http.StatusCreated)

	assertVisibility := func(label string, raw []byte) {
		t.Helper()
		var body map[string]json.RawMessage
		if err := json.Unmarshal(raw, &body); err != nil {
			t.Fatalf("%s: %v", label, err)
		}
		if string(body["visibility"]) != `"unlisted"` {
			t.Fatalf("%s: visibility=%s, want the signed \"unlisted\"", label, body["visibility"])
		}
	}
	assertVisibility("POST response", created)
	assertVisibility("GET by id", mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists/"+unlistedID, nil, http.StatusOK))

	mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists/shared-by-link", nil, http.StatusNotFound)
	mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlists/for-everyone", nil, http.StatusOK)
	mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlist-items/"+unlistedItem, nil, http.StatusNotFound)
	mustDoRaw(t, srv, http.MethodGet, "/api/v1/playlist-items/"+publicItem, nil, http.StatusOK)

	for _, path := range []string{"/api/v1/playlists", "/api/v1/playlists?curator=" + kid} {
		var page ListResponse[playlist.Playlist]
		mustDoJSON(t, srv, http.MethodGet, path, nil, http.StatusOK, &page)
		if len(page.Items) != 1 || page.Items[0].ID != publicID {
			t.Fatalf("%s: got %d playlists, want only the public one", path, len(page.Items))
		}
	}
	var items ListResponse[playlist.PlaylistItem]
	mustDoJSON(t, srv, http.MethodGet, "/api/v1/playlist-items", nil, http.StatusOK, &items)
	if len(items.Items) != 1 || items.Items[0].ID != publicItem {
		t.Fatalf("playlist-items: got %d items, want only the public playlist's", len(items.Items))
	}
}

// The request decoder (models.Visibility) refuses every other JSON type before dp1-go sees the document —
// null included, which a plain Go string would read as "" and dp1-go's `*string` as absent. Any string is
// accepted: the vocabulary is open.
func TestIntegration_VisibilityHTTP_memberType(t *testing.T) {
	srv := newIntegrationServer(t)
	priv, kid := newCuratorKeypair(t)

	for i, tc := range []struct {
		name   string
		value  any
		status int
	}{
		{"null", nil, http.StatusBadRequest},
		{"number", 1, http.StatusBadRequest},
		{"boolean", true, http.StatusBadRequest},
		{"object", map[string]any{}, http.StatusBadRequest},
		{"value a later version may define", "followers", http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := signedVisibilityPlaylist(t, priv, kid,
				fmt.Sprintf("e6000000-0000-4000-8000-%012d", i+1), fmt.Sprintf("member-type-%d", i+1),
				fmt.Sprintf("e7000000-0000-4000-8000-%012d", i+1), "")
			body["visibility"] = tc.value
			// Re-sign over the value under test, so a refusal is about the member, not the signature.
			delete(body, "signatures")
			unsigned, err := json.Marshal(body)
			if err != nil {
				t.Fatal(err)
			}
			sig, err := dp1sign.SignMultiEd25519(unsigned, priv, playlist.RoleCurator, "2026-10-28T00:00:00Z")
			if err != nil {
				t.Fatal(err)
			}
			body["signatures"] = []playlist.Signature{sig}
			resp := mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists", body, tc.status)
			if tc.status == http.StatusBadRequest && !bytes.Contains(resp, []byte("visibility")) {
				t.Fatalf("400 does not name the member: %s", resp)
			}
		})
	}
}

// A container naming a playlist by its slug URL would otherwise be a slug lookup by another route, and
// a probe: a create that succeeds for a taken slug and fails for a free one tells the caller which
// unlisted slugs exist. The same-origin reference must fail exactly as it does for a slug nobody holds,
// and succeed by id.
func TestIntegration_VisibilityHTTP_containerReferenceBySlug(t *testing.T) {
	srv := newIntegrationServer(t)
	priv, kid := newCuratorKeypair(t)

	const unlistedID = "e3000000-0000-4000-8000-000000000001"
	mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlists",
		signedVisibilityPlaylist(t, priv, kid, unlistedID, "kept-off-lists", "e4000000-0000-4000-8000-000000000001", "unlisted"), http.StatusCreated)

	group := func(id, slug, uri string) map[string]any {
		return signedGroupBody(t, priv, kid, id, slug, slug, "2026-10-28T00:00:00Z", []string{uri})
	}
	missing := doRawAnyStatus(t, srv, http.MethodPost, "/api/v1/playlist-groups",
		group("e5000000-0000-4000-8000-000000000001", "points-at-nothing", "http://example.com/api/v1/playlists/no-such-slug"))
	bySlug := doRawAnyStatus(t, srv, http.MethodPost, "/api/v1/playlist-groups",
		group("e5000000-0000-4000-8000-000000000002", "points-at-slug", "http://example.com/api/v1/playlists/kept-off-lists"))
	if missing.status == http.StatusCreated || bySlug.status != missing.status || bySlug.code != missing.code {
		t.Fatalf("slug reference to an unlisted playlist answered %d %q; a missing slug answers %d %q — they must match", bySlug.status, bySlug.code, missing.status, missing.code)
	}
	mustDoRaw(t, srv, http.MethodPost, "/api/v1/playlist-groups",
		group("e5000000-0000-4000-8000-000000000003", "points-at-id", "http://example.com/api/v1/playlists/"+unlistedID), http.StatusCreated)

	var members ListResponse[playlist.Playlist]
	mustDoJSON(t, srv, http.MethodGet, "/api/v1/playlists?playlist-group=points-at-id", nil, http.StatusOK, &members)
	if len(members.Items) != 1 || members.Items[0].ID != unlistedID {
		t.Fatalf("group members: got %d, want the unlisted playlist the group names", len(members.Items))
	}
}

type statusAndCode struct {
	status int
	code   string
}

// doRawAnyStatus performs a request without asserting its status and returns the status and error code.
func doRawAnyStatus(t *testing.T, srv *Server, method, path string, body any) statusAndCode {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	srv.srv.Handler.ServeHTTP(rec, req)
	var env struct {
		Error string `json:"error"`
	}
	_ = json.Unmarshal(rec.Body.Bytes(), &env)
	return statusAndCode{status: rec.Code, code: env.Error}
}
