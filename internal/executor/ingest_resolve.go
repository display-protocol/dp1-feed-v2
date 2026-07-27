package executor

import (
	"context"
	"fmt"
	"strings"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/display-protocol/dp1-feed-v2/internal/store"
)

// Group/channel ingest: each string in the document's "playlists" array becomes a store.IngestedPlaylist row.
// Local URLs hit the DB; remote URLs use HTTP fetch. Output order matches the input URI list, including repeated references to the same playlist.

// playlistAPIPrefix is the base URL for playlists on this feed (public_base_url + "/api/v1/playlists/").
// Used to detect same-origin playlist links so we load them from the DB instead of HTTP.
func (e *impl) playlistAPIPrefix() string {
	b := strings.TrimSuffix(strings.TrimSpace(e.publicBase), "/")
	if b == "" {
		return ""
	}
	return b + "/api/v1/playlists/"
}

// isLocalPlaylistURL reports whether raw points at this service's playlist API (see playlistAPIPrefix).
func (e *impl) isLocalPlaylistURL(raw string) bool {
	p := e.playlistAPIPrefix()
	if p == "" {
		return false
	}
	return strings.HasPrefix(strings.TrimSpace(raw), p)
}

// localPlaylistKeyFromURL strips the API prefix so GetPlaylist receives an id or slug fragment only.
func (e *impl) localPlaylistKeyFromURL(raw string) string {
	return strings.TrimPrefix(strings.TrimSpace(raw), e.playlistAPIPrefix())
}

// resolveOnePlaylistRef loads or fetches a single playlist URI:
//   - Local URL → store.GetPlaylist by id/slug from the path after /playlists/.
//   - Otherwise → HTTP fetch, validate JSON, parse id/slug from the DP-1 playlist object.
func (e *impl) resolveOnePlaylistRef(ctx context.Context, uri string) (store.IngestedPlaylist, error) {
	uri = strings.TrimSpace(uri)
	if uri == "" {
		return store.IngestedPlaylist{}, fmt.Errorf("empty playlist URI")
	}

	if e.isLocalPlaylistURL(uri) {
		// Same-origin: load already-stored JSON by id or slug fragment after /api/v1/playlists/.
		key := e.localPlaylistKeyFromURL(uri)
		if key == "" {
			return store.IngestedPlaylist{}, fmt.Errorf("invalid local playlist URL %q", uri)
		}
		rec, err := e.store.GetPlaylist(ctx, key)
		if err != nil {
			return store.IngestedPlaylist{}, fmt.Errorf("local playlist %q: %w", uri, err)
		}
		return store.IngestedPlaylist{ID: rec.ID, Slug: rec.Slug, Body: rec.Body}, nil
	}

	if e.fetch == nil {
		return store.IngestedPlaylist{}, fmt.Errorf("external playlist %q: fetcher is not configured (set playlist.fetch_* and use absolute URLs)", uri)
	}

	// Remote: GET body, validate with same rules as operator-authored playlists, then read id/slug from parsed playlist.
	body, err := e.fetch.FetchPlaylist(ctx, uri)
	if err != nil {
		return store.IngestedPlaylist{}, fmt.Errorf("fetch %q: %w", uri, err)
	}
	p, err := e.parseValidatedPlaylist(body)
	if err != nil {
		return store.IngestedPlaylist{}, fmt.Errorf("playlist %q: %w", uri, err)
	}
	if p == nil {
		return store.IngestedPlaylist{}, fmt.Errorf("playlist %q: nil parsed document", uri)
	}
	id, err := uuid.Parse(strings.TrimSpace(p.ID))
	if err != nil {
		return store.IngestedPlaylist{}, fmt.Errorf("playlist %q: id: %w", uri, err)
	}
	slug := strings.TrimSpace(p.Slug)
	if slug == "" {
		slug = fmt.Sprintf("ingested-%s", id.String()[:8])
	} else {
		slug = slugify(slug)
	}
	return store.IngestedPlaylist{ID: id, Slug: slug, Body: *p}, nil
}

// resolvePlaylistURIs resolves every URI in uris. The returned slice has the same length and order
// as uris (index i is the resolution of uris[i]), so membership position matches the document.
//
// Steps:
//  1. Reject an empty list (groups/channels require at least one playlist).
//  2. Run resolveOnePlaylistRef for each index in parallel (errgroup), capped at 8 goroutines,
//     writing into out[i] so completion order does not reorder results.
func (e *impl) resolvePlaylistURIs(ctx context.Context, uris []string) ([]store.IngestedPlaylist, error) {
	if len(uris) == 0 {
		return nil, fmt.Errorf("playlists must be non-empty")
	}
	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	out := make([]store.IngestedPlaylist, len(uris))
	for i := range uris {
		g.Go(func() error {
			ing, err := e.resolveOnePlaylistRef(ctx, uris[i])
			if err != nil {
				return err
			}
			out[i] = ing
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}
