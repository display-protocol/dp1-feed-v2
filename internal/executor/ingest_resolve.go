package executor

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
	"golang.org/x/sync/errgroup"

	"github.com/display-protocol/dp1-feed-v2/internal/fetcher"
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

// maxPlaylistURILen bounds a reference URI. URIs arrive inside client-submitted documents, which impose
// no limit of their own beyond the request-body cap, so without this a single reference could be
// megabytes of junk that the feed then stores. 2048 is the conventional practical URL ceiling and is far
// above any real DP-1 playlist URL.
const maxPlaylistURILen = 2048

// requireResolvableURILength rejects an over-long reference. The client chose the URI, so it is a 400.
func requireResolvableURILength(uri string) error {
	if len(uri) > maxPlaylistURILen {
		return fmt.Errorf("%w: playlist URI is %d bytes, over the %d byte limit", ErrPlaylistURITooLong, len(uri), maxPlaylistURILen)
	}
	return nil
}

// originUnavailable reports whether a fetch failure means "could not reach the origin" rather than "the
// origin answered".
//
// Only the former may fall back to the cache. A reachable origin is authoritative: if it answers 404 or
// 410 the publisher has withdrawn that playlist, and quietly persisting the previously cached membership
// would contradict an explicit answer — the stale reference could then outlive the withdrawal
// indefinitely, since nothing else revisits it. 5xx and 429 are the origin failing or deferring rather
// than deciding, so they count as unavailable.
//
// A refused destination is NOT unavailability. The guard fires because the URL now resolves somewhere
// this feed must not contact, which is a change in what the URL means; following the old answer would be
// exactly the wrong response to that.
func originUnavailable(err error) bool {
	if err == nil {
		return false
	}
	if errors.Is(err, fetcher.ErrBlockedDestination) {
		return false
	}
	var status *fetcher.StatusError
	if errors.As(err, &status) {
		return status.Transient()
	}
	// No usable HTTP response at all: DNS failure, refused connection, timeout, truncated body.
	return true
}

// lastKnownResolution answers what uri last resolved to, for use when the origin cannot be reached.
//
// Reports ok=false when nothing is cached, so the caller can surface the original fetch failure — that is
// the more useful error, since "we could not reach it and have never seen it" is a fetch problem, not a
// cache miss. A store error is likewise treated as no answer: the fetch failure is already the reason the
// request cannot proceed, and replacing it with a database error would obscure that.
func (e *impl) lastKnownResolution(ctx context.Context, uri string) (store.IngestedPlaylist, bool) {
	rec, err := e.store.GetPlaylistBySourceURI(ctx, uri)
	if err != nil {
		return store.IngestedPlaylist{}, false
	}
	return store.IngestedPlaylist{ID: rec.ID, Slug: rec.Slug, Raw: rec.Raw, SourceURI: uri}, true
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

	if err := requireResolvableURILength(uri); err != nil {
		return store.IngestedPlaylist{}, err
	}

	if e.fetch == nil {
		// No fetcher configured is an unavailable origin like any other, so the cache still applies.
		if ing, ok := e.lastKnownResolution(ctx, uri); ok {
			return ing, nil
		}
		return store.IngestedPlaylist{}, fmt.Errorf("external playlist %q: fetcher is not configured (set playlist.fetch_* and use absolute URLs)", uri)
	}

	// Remote: GET body, validate with same rules as operator-authored playlists, then read id/slug from parsed playlist.
	body, err := e.fetch.FetchPlaylist(ctx, uri)
	if err != nil {
		// Fall back to what this URI last resolved to only when the origin could not be reached. Ingestion
		// never refreshes a stored member, so a fetch that fails for unavailability could only have
		// rediscovered an id already recorded here; failing the write would let someone else's outage block
		// a mutation whose content could not change.
		//
		// Deliberately a fallback rather than the first thing tried. Consulting the cache up front skipped
		// the fetch entirely, which pinned a URI to whatever it first resolved to — globally, permanently,
		// and set by whichever anonymous caller referenced it first, since creation is open. A publisher
		// re-pointing their own URL was then never picked up. Fetching first keeps resolution current and
		// still survives the outage.
		if originUnavailable(err) {
			if ing, ok := e.lastKnownResolution(ctx, uri); ok {
				return ing, nil
			}
		}
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
			return store.IngestedPlaylist{ID: rec.ID, Slug: rec.Slug, Raw: rec.Raw, SourceURI: uri}, nil
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
	return store.IngestedPlaylist{ID: id, Slug: slug, Raw: append(json.RawMessage(nil), body...), SourceURI: uri}, nil
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
//  2. Reject a list longer than the configured cap, before any fetch is issued.
//  3. Run resolveOnePlaylistRef for each index in parallel (errgroup), capped at 8 goroutines,
//     writing into out[i] so completion order does not reorder results.
//
// The count check is the fan-out bound and must stay ahead of step 3. Creation is open to any client and
// every unstored URI becomes an outbound request, so without it the only ceiling is the request body
// size: one valid self-signed document near that limit can name enough URIs to turn a single inbound
// request into six figures of outbound ones. The concurrency limit paces that work, it does not bound it,
// and the SSRF guard only constrains where each request may go. Allocation is deliberately deferred until
// after the check so an oversized list cannot make the feed reserve memory for it either.
func (e *impl) resolvePlaylistURIs(ctx context.Context, uris []string) ([]store.IngestedPlaylist, error) {
	if len(uris) == 0 {
		return nil, fmt.Errorf("playlists must be non-empty")
	}
	if max := e.maxRefs; max > 0 && len(uris) > max {
		return nil, fmt.Errorf("%w: %d playlist references exceeds the maximum of %d", ErrTooManyReferences, len(uris), max)
	}
	// Resolve each DISTINCT URI once and copy the result to every position that names it.
	//
	// A document may legitimately repeat a URI, and resolving each occurrence independently made the same
	// reference answerable differently within one request: two fetches of an unmapped URI straddling a
	// change at the origin yield two different playlist ids, so the membership rows disagree about what
	// that one URI means, while only a single URI→id mapping is recorded. A later replace of the unchanged
	// document would then resolve every occurrence to the mapped winner and silently alter membership.
	// Resolving once makes one URI mean one playlist for the whole request, and incidentally removes the
	// duplicate fetches.
	positions := make(map[string][]int, len(uris))
	order := make([]string, 0, len(uris))
	for i, uri := range uris {
		key := strings.TrimSpace(uri)
		if _, seen := positions[key]; !seen {
			order = append(order, key)
		}
		positions[key] = append(positions[key], i)
	}

	// Aggregate retained-bytes budget, charged as each document is resolved.
	//
	// The reference cap and the per-fetch size cap do not bound memory between them, they multiply: every
	// resolved body is held until the whole set is ready to persist, so 1000 references at the 4 MiB fetch
	// cap is ~4 GiB from one unauthenticated request, and persistence copies the bodies again. The
	// eight-way concurrency limit paces downloads without bounding what accumulates behind them. Charging
	// here — before the result is retained, and once per distinct URI rather than per position — is what
	// actually bounds it; exceeding the budget cancels the errgroup, so in-flight fetches stop too.
	var resolved atomic.Int64

	g, ctx := errgroup.WithContext(ctx)
	g.SetLimit(8)
	out := make([]store.IngestedPlaylist, len(uris))
	for _, uri := range order {
		g.Go(func() error {
			ing, err := e.resolveOnePlaylistRef(ctx, uri)
			if err != nil {
				return err
			}
			if budget := e.maxResolvedBytes; budget > 0 {
				if total := resolved.Add(int64(len(ing.Raw))); total > budget {
					return fmt.Errorf("%w: resolved playlists exceed the %d byte budget for one request", ErrResolvedTooLarge, budget)
				}
			}
			for _, i := range positions[uri] {
				out[i] = ing
			}
			return nil
		})
	}
	if err := g.Wait(); err != nil {
		return nil, err
	}
	return out, nil
}
