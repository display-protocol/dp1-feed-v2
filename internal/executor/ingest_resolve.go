package executor

import (
	"context"
	"encoding/json"
	"errors"
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
		// Carry the stored bytes verbatim: re-marshaling the typed body would orphan the member's own
		// curator signatures when the upsert writes it back.
		return store.IngestedPlaylist{ID: rec.ID, Slug: rec.Slug, Raw: rec.Raw}, nil
	}

	if e.fetch == nil {
		return store.IngestedPlaylist{}, fmt.Errorf("external playlist %q: fetcher is not configured (set playlist.fetch_* and use absolute URLs)", uri)
	}

	// Remote: GET body, validate with same rules as operator-authored playlists, then read id/slug from parsed playlist.
	body, err := e.fetch.FetchPlaylist(ctx, uri)
	if err != nil {
		return store.IngestedPlaylist{}, fmt.Errorf("fetch %q: %w", uri, err)
	}

	// Reference-only contract: ingestion links a playlist this feed already holds, it never rewrites one.
	// So identity is resolved before content is judged — if the id is already stored, the stored bytes are
	// what gets linked and the remote representation is not consulted at all.
	//
	// Ordering is the whole point. Validating first made membership depend on a document that, by contract,
	// is ignored: once a member was stored here, its origin could rot, rotate keys, or serve something
	// malformed, and every later group or channel referencing that URL would fail to be created even though
	// nothing about the stored playlist needed to change.
	//
	// This grants no new reach. A body claiming an id we already hold only earns a link to that stored
	// playlist — exactly what the same-origin URL form above already offers any caller — and the stored row
	// is returned untouched, so a forged body cannot alter, replace, or reveal anything.
	if id, ok := playlistIDFromBody(body); ok {
		rec, err := e.store.GetPlaylist(ctx, id.String())
		switch {
		case err == nil:
			return store.IngestedPlaylist{ID: rec.ID, Slug: rec.Slug, Raw: rec.Raw}, nil
		case errors.Is(err, store.ErrNotFound):
			// Not held here, so this ingest would create it: fall through to the full create bar below.
			// A tombstoned id also lands here, and the store refuses to insert it (ErrDocumentDeleted),
			// so retiring an id still cannot be undone through an ingest.
		default:
			return store.IngestedPlaylist{}, fmt.Errorf("playlist %q: %w", uri, err)
		}
	}

	p, err := e.parseValidatedPlaylist(body)
	if err != nil {
		return store.IngestedPlaylist{}, fmt.Errorf("playlist %q: %w", uri, err)
	}
	if p == nil {
		return store.IngestedPlaylist{}, fmt.Errorf("playlist %q: nil parsed document", uri)
	}
	// A remote document this feed does not already hold would be *created* by this ingest, so hold it to
	// the same bar as POST: it must be validly self-signed by a key it declares as a curator. Schema
	// validity alone is not enough — an unsigned or badly signed body would otherwise be published here
	// under the referencing party's request. (This cannot authorize overwriting an existing playlist:
	// ingestion only ever links those, never modifies them.)
	if err := e.verifyPlaylistCuratorSignatures(body, p.Signatures, p.Curators); err != nil {
		return store.IngestedPlaylist{}, fmt.Errorf("playlist %q: curator signature verification: %w", uri, err)
	}
	// Materializing a remote document is a create, so it must satisfy exactly what POST requires. The feed
	// previously synthesized a missing slug and slugified a supplied one while storing the signed body
	// untouched, which left the row's routing slug disagreeing with the slug inside the document: the
	// document served at that URL contradicted its own address, and a later replace — which requires the
	// submitted identity to equal the stored row's — could never match. Reject instead of repairing;
	// repairing is exactly what verbatim storage forbids.
	id, err := uuid.Parse(strings.TrimSpace(p.ID))
	if err != nil {
		return store.IngestedPlaylist{}, fmt.Errorf("playlist %q: id: %w", uri, err)
	}
	slug, err := requireSlug(p.Slug)
	if err != nil {
		return store.IngestedPlaylist{}, fmt.Errorf("playlist %q: %w", uri, err)
	}
	if err := requireItemIDs(p.Items); err != nil {
		return store.IngestedPlaylist{}, fmt.Errorf("playlist %q: %w", uri, err)
	}
	if _, err := parseUserProvidedCreated(&p.Created); err != nil {
		return store.IngestedPlaylist{}, fmt.Errorf("playlist %q: %w", uri, err)
	}
	// Keep the fetched bytes exactly as served: the remote document's signatures are bound to them, so a
	// typed re-marshal here would store a member whose own signatures no longer verify.
	return store.IngestedPlaylist{ID: id, Slug: slug, Raw: append(json.RawMessage(nil), body...)}, nil
}

// playlistIDFromBody pulls just the id out of a fetched body.
//
// Deliberately the cheapest possible parse — no schema, no signatures, no identity rules — because its
// only job is to answer "do we already hold this?". A body that could never clear the create bar must
// still be able to name a playlist this feed already trusts; running validation first is precisely the
// bug this avoids. A body that is not JSON, or carries no usable id, simply has no answer to give, so the
// caller falls through to the full create path and reports the real reason it is unacceptable.
func playlistIDFromBody(body []byte) (uuid.UUID, bool) {
	var probe struct {
		ID string `json:"id"`
	}
	if err := json.Unmarshal(body, &probe); err != nil {
		return uuid.UUID{}, false
	}
	id, err := uuid.Parse(strings.TrimSpace(probe.ID))
	if err != nil {
		return uuid.UUID{}, false
	}
	return id, true
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
