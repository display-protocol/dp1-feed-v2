// Package pg implements store.Store on PostgreSQL using pgxpool (CRUD, keyset pagination, transactional group/channel ingest).
package pg

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/display-protocol/dp1-go/extension/channels"
	"github.com/display-protocol/dp1-go/playlist"
	"github.com/display-protocol/dp1-go/playlistgroup"
	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/display-protocol/dp1-feed-v2/internal/store"
	"github.com/display-protocol/dp1-feed-v2/internal/utils"
)

// insertPlaylistItemIndexFromBody fills playlist_item_index from body.items (array order → position).
// $1 playlist id, $2 body JSONB, $3 playlists.created_at (caller supplies from INSERT/UPDATE RETURNING).
// Non-array or missing "items" inserts no rows. Each element must have a UUID "id" field.
const insertPlaylistItemIndexFromBody = `
WITH items AS (
	SELECT CASE
		WHEN jsonb_typeof($2::jsonb->'items') = 'array' THEN $2::jsonb->'items'
		ELSE '[]'::jsonb
	END AS arr
)
INSERT INTO playlist_item_index (item_id, playlist_id, playlist_created_at, position, item)
SELECT (elem->>'id')::uuid, $1, $3::timestamptz, (ord - 1)::int, elem
FROM items, jsonb_array_elements(items.arr) WITH ORDINALITY AS t(elem, ord)`

// Store is the PostgreSQL-backed store; it does not take ownership of the pool (caller closes it).
type Store struct {
	pool *pgxpool.Pool
}

// Tombstones. A deleted document's id is retired on this feed so a replay of its (public, still validly
// signed) bytes cannot resurrect it through the open create path. resource_type is the document's table
// name — an internal constant, never client input.
const (
	tombstoneInsert = `INSERT INTO deleted_documents (resource_type, id) VALUES ($1, $2) ON CONFLICT DO NOTHING`
	tombstoneExists = `SELECT EXISTS (SELECT 1 FROM deleted_documents WHERE resource_type = $1 AND id = $2)`
)

// lockDocumentID serializes every path that creates, deletes or ingests one (resource_type, id) against
// the others, for the remainder of the caller's transaction.
//
// Without it the tombstone guard is check-then-act and can be stepped over: a replaying create reads no
// tombstone, then blocks on the unique-key conflict with the row a concurrent delete is removing; when
// that delete commits — writing its tombstone — the blocked insert proceeds against a now-empty key and
// succeeds, never re-reading the tombstone. That resurrects a document immediately after a successful
// owner delete, which is precisely the guarantee tombstones exist to provide. Taking the lock *before*
// the check makes check and write atomic with respect to each other.
//
// Deadlock safety: a transaction takes at most one container lock (its own new group/channel id, unique
// to it) and then playlist locks in sorted order, so no two transactions can hold each other's next lock.
// The lock is advisory and transaction-scoped, so it is released on commit or rollback either way.
const documentLockKey = `SELECT pg_advisory_xact_lock(hashtextextended($1 || ':' || $2::text, 0))`

func lockDocumentID(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, table string, id uuid.UUID) error {
	if _, err := q.Exec(ctx, documentLockKey, table, id); err != nil {
		return fmt.Errorf("lock %s %s: %w", table, id, err)
	}
	return nil
}

// lockDocumentIDs takes lockDocumentID for a batch, in ascending id order so concurrent batches that
// overlap cannot deadlock by acquiring the same pair in opposite orders.
func lockDocumentIDs(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, table string, ids []uuid.UUID) error {
	sorted := append([]uuid.UUID(nil), ids...)
	slices.SortFunc(sorted, func(a, b uuid.UUID) int { return bytes.Compare(a[:], b[:]) })
	var prev uuid.UUID
	for i, id := range sorted {
		if i > 0 && id == prev {
			continue // membership may repeat an id; one lock is enough
		}
		if err := lockDocumentID(ctx, q, table, id); err != nil {
			return err
		}
		prev = id
	}
	return nil
}

// createConflict translates a Postgres unique violation on a create into store.ErrAlreadyExists, or
// returns nil when err is something else.
//
// The tombstone check cannot cover this: it answers "was this id deleted", while these are collisions
// with a resource that is still live. The common case is not an attack but a retry — a POST whose
// response was lost, re-sent — so it must read as 409, not as a server fault. The constraint name tells
// which column collided: Postgres names the implicit ones <table>_pkey and <table>_slug_key.
func createConflict(err error, resource string) error {
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != uniqueViolationCode {
		return nil
	}
	// Detail is what the client sees, so it names the rule that was violated rather than the constraint.
	// The default arm keeps the constraint name because an unrecognized unique index is an operator
	// problem: the message is the only clue about which one fired.
	switch {
	case strings.HasSuffix(pgErr.ConstraintName, "_pkey"):
		return &store.ConflictError{Kind: store.ErrAlreadyExists, Detail: fmt.Sprintf("a %s with this id already exists", resource)}
	case strings.HasSuffix(pgErr.ConstraintName, "_slug_key"):
		return &store.ConflictError{Kind: store.ErrAlreadyExists, Detail: fmt.Sprintf("a %s with this slug already exists", resource)}
	default:
		return &store.ConflictError{Kind: store.ErrAlreadyExists, Detail: fmt.Sprintf("this %s violates %s", resource, pgErr.ConstraintName)}
	}
}

// uniqueViolationCode is the SQLSTATE Postgres raises for a unique or primary-key violation.
const uniqueViolationCode = "23505"

// requireNotTombstoned fails a create whose id this feed has already deleted. It runs on the caller's
// transaction so the check and the insert commit together, and callers must hold lockDocumentID first.
func requireNotTombstoned(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, table string, id uuid.UUID) error {
	var deleted bool
	if err := q.QueryRow(ctx, tombstoneExists, table, id).Scan(&deleted); err != nil {
		return fmt.Errorf("check %s tombstone: %w", table, err)
	}
	if deleted {
		return fmt.Errorf("%w: %s %s", store.ErrDocumentDeleted, table, id)
	}
	return nil
}

// requireNoneTombstoned is requireNotTombstoned for a batch of ids (membership ingestion). It runs on the
// caller's transaction so the check and the inserts commit together.
//
// Checking every referenced id, rather than only the ones that turn out to be missing, is safe and
// simpler: a row that exists cannot be tombstoned (a delete removes the row and writes the tombstone in
// one transaction, and the create guard blocks re-creating it), so a tombstoned id in the batch always
// denotes a row that would otherwise be inserted here.
func requireNoneTombstoned(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, table string, ids []uuid.UUID) error {
	const anyTombstoned = `SELECT id FROM deleted_documents WHERE resource_type = $1 AND id = ANY($2::uuid[]) LIMIT 1`
	var deleted uuid.UUID
	switch err := q.QueryRow(ctx, anyTombstoned, table, ids).Scan(&deleted); {
	case errors.Is(err, pgx.ErrNoRows):
		return nil
	case err != nil:
		return fmt.Errorf("check %s tombstones: %w", table, err)
	default:
		return fmt.Errorf("%w: %s %s", store.ErrDocumentDeleted, table, deleted)
	}
}

// classifyConditionalWrite explains a conditional UPDATE/DELETE that matched no row: if the row still
// exists, the caller's expected updated_at was stale (a concurrent write, or a delete and re-create,
// landed) → ErrConcurrentModification; if the row is gone → ErrNotFound.
//
// The row-is-gone case is deliberately ErrNotFound (404) rather than ErrConcurrentModification (409),
// even though it is reached after the executor authorized against a row it had loaded. 409 carries
// "re-read and retry", and that advice would be wrong here: deleting an id tombstones it, so the resource
// cannot come back and no number of retries will succeed. 404 is both accurate and terminal. The
// deleted-and-re-created case is genuinely different — the row exists, the caller's generation is simply
// stale — and does return 409, where retrying can succeed.
//
// table is a fixed internal constant, never client input, so interpolating it into the existence probe
// carries no injection risk. The probe runs on the same tx/conn as the failed write, so it observes the
// same snapshot.
func classifyConditionalWrite(ctx context.Context, q interface {
	QueryRow(context.Context, string, ...any) pgx.Row
}, table string, rowID uuid.UUID) error {
	var exists bool
	if err := q.QueryRow(ctx, "SELECT EXISTS(SELECT 1 FROM "+table+" WHERE id = $1)", rowID).Scan(&exists); err != nil {
		return fmt.Errorf("classify conditional write: %w", err)
	}
	if exists {
		return store.ErrConcurrentModification
	}
	return store.ErrNotFound
}

// requireDocument guards the write path: a document is persisted as the raw bytes the executor signed,
// so an empty payload here is a programming error, not a client error.
func requireDocument(raw json.RawMessage, label string) error {
	if len(raw) == 0 {
		return fmt.Errorf("nil %s", label)
	}
	return nil
}

// scanDocument turns a jsonb column into the record pair (Raw, Body). Raw is copied out of the scan
// buffer because the record outlives the row iteration; Body is the decoded view (see store.PlaylistRecord).
func scanDocument[T any](raw []byte, label string) (json.RawMessage, T, error) {
	body, err := utils.DecodeJSONB[T](raw, label)
	if err != nil {
		var zero T
		return nil, zero, err
	}
	return append(json.RawMessage(nil), raw...), body, nil
}

// NewStore wraps a pgx pool as a store.Store (created_at/updated_at use column defaults; updated_at is refreshed by triggers).
func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

// Ping implements store.Store.
func (s *Store) Ping(ctx context.Context) error {
	return s.pool.Ping(ctx)
}

// =============================================================================
// Playlists
// =============================================================================

// CreatePlaylist implements store.Store.
//
// Process: insert the playlist row, then derive playlist_item_index rows from body.items
// (array index → position). Missing or non-array "items" yields no index rows; each item needs a UUID "id".
func (s *Store) CreatePlaylist(ctx context.Context, id uuid.UUID, slug string, raw json.RawMessage) error {
	const insertPlaylist = `
INSERT INTO playlists (id, slug, body)
VALUES ($1, $2, $3::jsonb)
RETURNING created_at`

	if err := requireDocument(raw, "playlist body"); err != nil {
		return err
	}
	bodyJSON := []byte(raw)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	// Commit succeeds → Rollback becomes no-op. On failure, Rollback aborts the tx.
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockDocumentID(ctx, tx, "playlists", id); err != nil {
		return err
	}
	if err := requireNotTombstoned(ctx, tx, "playlists", id); err != nil {
		return err
	}

	var createdAt time.Time
	if err := tx.QueryRow(ctx, insertPlaylist, id, slug, bodyJSON).Scan(&createdAt); err != nil {
		if conflict := createConflict(err, "playlist"); conflict != nil {
			return conflict
		}
		return fmt.Errorf("insert playlist: %w", err)
	}
	if _, err := tx.Exec(ctx, insertPlaylistItemIndexFromBody, id, bodyJSON, createdAt); err != nil {
		return fmt.Errorf("insert playlist_item_index: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// GetPlaylist implements store.Store.
func (s *Store) GetPlaylist(ctx context.Context, idOrSlug string) (*store.PlaylistRecord, error) {
	const (
		byID = `
SELECT id, slug, body, created_at, updated_at
FROM playlists
WHERE id = $1`

		bySlug = `
SELECT id, slug, body, created_at, updated_at
FROM playlists
WHERE slug = $1`
	)

	id, err := uuid.Parse(idOrSlug)
	var row pgx.Row
	if err == nil {
		row = s.pool.QueryRow(ctx, byID, id)
	} else {
		row = s.pool.QueryRow(ctx, bySlug, idOrSlug)
	}

	var rec store.PlaylistRecord
	var raw []byte
	if err := row.Scan(&rec.ID, &rec.Slug, &raw, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w", store.ErrNotFound)
		}
		return nil, fmt.Errorf("select playlist: %w", err)
	}
	if rec.Raw, rec.Body, err = scanDocument[playlist.Playlist](raw, "playlist body"); err != nil {
		return nil, err
	}
	return &rec, nil
}

// GetPlaylistItems implements store.Store.
// GetPlaylistBySourceURI implements store.Store. It answers with the playlist a URI last resolved to.
//
// This is the fallback consulted when fetching the origin fails, not the primary resolution path, so a
// row here can be stale by design: it is only ever read when the authoritative answer is unavailable, and
// the next successful fetch refreshes it. Matching is on the generated uri_hash, since uri itself can
// exceed the btree index limit.
func (s *Store) GetPlaylistBySourceURI(ctx context.Context, uri string) (*store.PlaylistRecord, error) {
	const q = `
SELECT p.id, p.slug, p.body, p.created_at, p.updated_at
FROM playlist_sources src
JOIN playlists p ON p.id = src.playlist_id
WHERE src.uri_hash = $1`

	var rec store.PlaylistRecord
	var raw []byte
	if err := s.pool.QueryRow(ctx, q, sourceURIHash(uri)).Scan(&rec.ID, &rec.Slug, &raw, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w", store.ErrNotFound)
		}
		return nil, fmt.Errorf("select playlist by source uri: %w", err)
	}
	var err error
	if rec.Raw, rec.Body, err = scanDocument[playlist.Playlist](raw, "playlist body"); err != nil {
		return nil, err
	}
	return &rec, nil
}

// sourceURIHash is the playlist_sources key: sha256 of the URI's UTF-8 bytes.
//
// Computed here rather than by the database because convert_to() is STABLE, not IMMUTABLE, so Postgres
// refuses sha256(convert_to(uri,'UTF8')) in a generated column or index expression. Hashing also sidesteps
// the ~2704-byte btree limit that keying on the URI text itself would hit, and URIs are client input with
// no length bound of their own.
func sourceURIHash(uri string) []byte {
	sum := sha256.Sum256([]byte(uri))
	return sum[:]
}

// recordPlaylistSources records what each remote URI resolved to on this request.
//
// UPSERT, not first-wins. The table is a last-known-good cache consulted only when a fetch fails, so it
// has to track the most recent successful resolution: if a publisher re-points a URL to a different
// playlist, the fallback must follow rather than serve an answer the origin abandoned. (An earlier
// revision kept the first value forever, which was required only because resolution consulted the cache
// *before* fetching — a design that pinned a URI globally to whatever the first caller saw.)
//
// Runs on the caller's transaction, so the cache and the membership it backs commit together. Local
// (same-origin) references carry no SourceURI and are skipped — their id or slug is already in the path.
func recordPlaylistSources(ctx context.Context, tx pgx.Tx, playlists []store.IngestedPlaylist) error {
	uris := make([]string, 0, len(playlists))
	ids := make([]uuid.UUID, 0, len(playlists))
	for _, p := range playlists {
		if p.SourceURI == "" {
			continue
		}
		uris = append(uris, p.SourceURI)
		ids = append(ids, p.ID)
	}
	if len(uris) == 0 {
		return nil
	}
	hashes := make([][]byte, len(uris))
	for i, u := range uris {
		hashes[i] = sourceURIHash(u)
	}
	const q = `
INSERT INTO playlist_sources (uri, uri_hash, playlist_id)
SELECT DISTINCT ON (x.uri_hash) x.uri, x.uri_hash, x.playlist_id
FROM unnest($1::text[], $2::bytea[], $3::uuid[]) WITH ORDINALITY AS x(uri, uri_hash, playlist_id, ord)
ORDER BY x.uri_hash, x.ord
ON CONFLICT (uri_hash) DO UPDATE
SET playlist_id = EXCLUDED.playlist_id,
    updated_at  = now()`
	if _, err := tx.Exec(ctx, q, uris, hashes, ids); err != nil {
		return fmt.Errorf("record playlist sources: %w", err)
	}
	return nil
}

func (s *Store) GetPlaylistItems(ctx context.Context, idOrSlug string) ([]store.PlaylistItemRecord, error) {
	const (
		byID = `
SELECT item_id, playlist_id, position, item
FROM playlist_item_index
WHERE playlist_id = $1
ORDER BY position`

		bySlug = `
SELECT i.item_id, i.playlist_id, i.position, i.item
FROM playlist_item_index i
JOIN playlists p ON i.playlist_id = p.id
WHERE p.slug = $1
ORDER BY i.position`
	)

	id, err := uuid.Parse(idOrSlug)
	var rows pgx.Rows
	if err == nil {
		rows, err = s.pool.Query(ctx, byID, id)
	} else {
		rows, err = s.pool.Query(ctx, bySlug, idOrSlug)
	}
	if err != nil {
		return nil, fmt.Errorf("query playlist_item_index: %w", err)
	}
	defer rows.Close()

	var out []store.PlaylistItemRecord
	for rows.Next() {
		var rec store.PlaylistItemRecord
		var raw []byte
		if err := rows.Scan(&rec.ItemID, &rec.PlaylistID, &rec.Position, &raw); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		it, err := utils.DecodeJSONB[playlist.PlaylistItem](raw, "playlist item")
		if err != nil {
			return nil, err
		}
		rec.Item = it
		out = append(out, rec)
	}
	return out, rows.Err()
}

// ListPlaylistItems implements store.Store.
//
// Ordering: playlist_created_at (denormalized from playlists.created_at), position, item_id — keyset tuple matches ORDER BY.
func (s *Store) ListPlaylistItems(ctx context.Context, p *store.ListPlaylistItemsParams) ([]store.PlaylistItemRecord, string, error) {
	if p == nil {
		return nil, "", fmt.Errorf("nil list params")
	}
	limit, err := store.ResolveListLimit(p.Limit)
	if err != nil {
		return nil, "", err
	}

	chF := strings.TrimSpace(p.ChannelFilter)
	pgF := strings.TrimSpace(p.PlaylistGroupFilter)

	order := p.Sort.SQLOrder()
	tupleOp := p.Sort.TupleAfterCursorOp()

	args := []any{limit + 1}

	var filterSQL string
	if chF != "" {
		n := len(args) + 1
		filterSQL = fmt.Sprintf(` AND EXISTS (
			SELECT 1 FROM channel_members cm
			WHERE cm.playlist_id = i.playlist_id
			AND cm.channel_id IN (SELECT id FROM channels WHERE id::text = $%d OR slug = $%d)
		)`, n, n)
		args = append(args, chF)
	} else if pgF != "" {
		n := len(args) + 1
		filterSQL = fmt.Sprintf(` AND EXISTS (
			SELECT 1 FROM playlist_group_members pgm
			WHERE pgm.playlist_id = i.playlist_id
			AND pgm.playlist_group_id IN (SELECT id FROM playlist_groups WHERE id::text = $%d OR slug = $%d)
		)`, n, n)
		args = append(args, pgF)
	}

	var cursorSQL string
	if p.Cursor != "" {
		plCreated, pos, iid, derr := decodePlaylistItemCursor(p.Cursor)
		if derr != nil {
			return nil, "", fmt.Errorf("cursor: %w", derr)
		}
		n := len(args) + 1
		cursorSQL = fmt.Sprintf(
			` AND (i.playlist_created_at, i.position, i.item_id) %s ($%d::timestamptz, $%d::int, $%d::uuid)`,
			tupleOp, n, n+1, n+2,
		)
		args = append(args, plCreated, pos, iid)
	}

	q := fmt.Sprintf(`
SELECT i.item_id, i.playlist_id, i.position, i.item, i.playlist_created_at
FROM playlist_item_index i
WHERE 1=1%s%s
ORDER BY i.playlist_created_at %s, i.position %s, i.item_id %s
LIMIT $1`, filterSQL, cursorSQL, order, order, order)

	rows, err := s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list playlist items: %w", err)
	}
	defer rows.Close()

	type rowWithCursor struct {
		rec       store.PlaylistItemRecord
		plCreated time.Time
	}
	var buf []rowWithCursor
	for rows.Next() {
		var rec store.PlaylistItemRecord
		var raw []byte
		var plCreated time.Time
		if err := rows.Scan(&rec.ItemID, &rec.PlaylistID, &rec.Position, &raw, &plCreated); err != nil {
			return nil, "", fmt.Errorf("scan: %w", err)
		}
		it, err := utils.DecodeJSONB[playlist.PlaylistItem](raw, "playlist item")
		if err != nil {
			return nil, "", err
		}
		rec.Item = it
		buf = append(buf, rowWithCursor{rec: rec, plCreated: plCreated})
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}

	nextCursor := ""
	if len(buf) > limit {
		last := buf[limit-1]
		buf = buf[:limit]
		nextCursor = encodePlaylistItemCursor(last.plCreated, last.rec.Position, last.rec.ItemID)
	}
	out := make([]store.PlaylistItemRecord, len(buf))
	for i := range buf {
		out[i] = buf[i].rec
	}
	return out, nextCursor, nil
}

// GetPlaylistItem implements store.Store.
func (s *Store) GetPlaylistItem(ctx context.Context, itemID uuid.UUID) (*store.PlaylistItemRecord, error) {
	const q = `
SELECT item_id, playlist_id, position, item
FROM playlist_item_index
WHERE item_id = $1`

	var rec store.PlaylistItemRecord
	var raw []byte
	err := s.pool.QueryRow(ctx, q, itemID).Scan(&rec.ItemID, &rec.PlaylistID, &rec.Position, &raw)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w", store.ErrNotFound)
		}
		return nil, fmt.Errorf("get playlist item: %w", err)
	}
	it, err := utils.DecodeJSONB[playlist.PlaylistItem](raw, "playlist item")
	if err != nil {
		return nil, err
	}
	rec.Item = it
	return &rec, nil
}

// ListPlaylists implements store.Store.
//
// Ordering: created_at then id, direction from p.Sort. Pagination: fetch limit+1 rows; if extra row exists,
// trim to limit and return next_cursor built from the last kept row (see encodeCursor).
func (s *Store) ListPlaylists(ctx context.Context, p *store.ListPlaylistsParams) ([]store.PlaylistRecord, string, error) {
	if p == nil {
		return nil, "", fmt.Errorf("nil list params")
	}
	limit, err := store.ResolveListLimit(p.Limit)
	if err != nil {
		return nil, "", err
	}
	order := p.Sort.SQLOrder()
	tupleOp := p.Sort.TupleAfterCursorOp()

	chF := strings.TrimSpace(p.ChannelFilter)
	pgF := strings.TrimSpace(p.PlaylistGroupFilter)

	var filterSQL string
	var args []any
	if p.Cursor == "" {
		args = []any{limit + 1}
		if chF != "" {
			const n = 2
			filterSQL = fmt.Sprintf(` AND EXISTS (
			SELECT 1 FROM channel_members cm
			WHERE cm.playlist_id = playlists.id
			AND cm.channel_id IN (SELECT id FROM channels WHERE id::text = $%d OR slug = $%d)
		)`, n, n)
			args = append(args, chF)
		} else if pgF != "" {
			const n = 2
			filterSQL = fmt.Sprintf(` AND EXISTS (
			SELECT 1 FROM playlist_group_members pgm
			WHERE pgm.playlist_id = playlists.id
			AND pgm.playlist_group_id IN (SELECT id FROM playlist_groups WHERE id::text = $%d OR slug = $%d)
		)`, n, n)
			args = append(args, pgF)
		}
	} else {
		created, id, derr := decodeCursor(p.Cursor)
		if derr != nil {
			return nil, "", fmt.Errorf("cursor: %w", derr)
		}
		args = []any{limit + 1, created, id}
		if chF != "" {
			const n = 4
			filterSQL = fmt.Sprintf(` AND EXISTS (
			SELECT 1 FROM channel_members cm
			WHERE cm.playlist_id = playlists.id
			AND cm.channel_id IN (SELECT id FROM channels WHERE id::text = $%d OR slug = $%d)
		)`, n, n)
			args = append(args, chF)
		} else if pgF != "" {
			const n = 4
			filterSQL = fmt.Sprintf(` AND EXISTS (
			SELECT 1 FROM playlist_group_members pgm
			WHERE pgm.playlist_id = playlists.id
			AND pgm.playlist_group_id IN (SELECT id FROM playlist_groups WHERE id::text = $%d OR slug = $%d)
		)`, n, n)
			args = append(args, pgF)
		}
	}

	var q string
	if p.Cursor == "" {
		q = fmt.Sprintf(`
SELECT id, slug, body, created_at, updated_at
FROM playlists
WHERE 1=1%s
ORDER BY created_at %s, id %s
LIMIT $1`, filterSQL, order, order)
	} else {
		q = fmt.Sprintf(`
SELECT id, slug, body, created_at, updated_at
FROM playlists
WHERE (created_at, id) %s ($2::timestamptz, $3::uuid)%s
ORDER BY created_at %s, id %s
LIMIT $1`, tupleOp, filterSQL, order, order)
	}

	var rows pgx.Rows
	rows, err = s.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, "", fmt.Errorf("list playlists: %w", err)
	}
	defer rows.Close()

	var out []store.PlaylistRecord
	for rows.Next() {
		var rec store.PlaylistRecord
		var raw []byte
		if err := rows.Scan(&rec.ID, &rec.Slug, &raw, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, "", fmt.Errorf("scan: %w", err)
		}
		if rec.Raw, rec.Body, err = scanDocument[playlist.Playlist](raw, "playlist body"); err != nil {
			return nil, "", err
		}
		out = append(out, rec)
	}

	nextCursor := ""
	if len(out) > limit {
		last := out[limit-1]
		out = out[:limit]
		nextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, nextCursor, rows.Err()
}

// UpdatePlaylist implements store.Store (updated_at is set by trigger; item index rebuilt from body.items).
func (s *Store) UpdatePlaylist(ctx context.Context, idOrSlug string, raw json.RawMessage, expectedUpdatedAt time.Time) error {
	const (
		updateByID = `UPDATE playlists
SET body = $2::jsonb, slug = COALESCE(NULLIF($2::jsonb->>'slug', ''), slug)
WHERE id = $1 AND updated_at = $3 RETURNING created_at`
		selectIDBySlug = `SELECT id FROM playlists WHERE slug = $1`
		clearItemIndex = `DELETE FROM playlist_item_index WHERE playlist_id = $1`
	)

	if err := requireDocument(raw, "playlist body"); err != nil {
		return err
	}
	bodyJSON := []byte(raw)

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var rowID uuid.UUID
	if id, perr := uuid.Parse(idOrSlug); perr == nil {
		rowID = id
	} else {
		if err := tx.QueryRow(ctx, selectIDBySlug, idOrSlug).Scan(&rowID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w", store.ErrNotFound)
			}
			return fmt.Errorf("lookup playlist slug: %w", err)
		}
	}

	var playlistCreatedAt time.Time
	err = tx.QueryRow(ctx, updateByID, rowID, bodyJSON, expectedUpdatedAt).Scan(&playlistCreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return classifyConditionalWrite(ctx, tx, "playlists", rowID)
		}
		return fmt.Errorf("update playlist: %w", err)
	}
	if _, err := tx.Exec(ctx, clearItemIndex, rowID); err != nil {
		return fmt.Errorf("clear playlist_item_index: %w", err)
	}
	if _, err := tx.Exec(ctx, insertPlaylistItemIndexFromBody, rowID, bodyJSON, playlistCreatedAt); err != nil {
		return fmt.Errorf("insert playlist_item_index: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// DeletePlaylist implements store.Store. The delete is conditional on expectedUpdatedAt so a decision
// made on an earlier read cannot remove a row that has since changed or been re-created (see
// store.ErrConcurrentModification).
func (s *Store) DeletePlaylist(ctx context.Context, idOrSlug string, expectedUpdatedAt time.Time) error {
	return s.deleteDocumentRow(ctx, "playlists", idOrSlug, expectedUpdatedAt)
}

// deleteDocumentRow is the shared conditional delete for the three document tables. It resolves the row
// id (accepting a UUID or a slug), deletes only when updated_at still matches what the caller authorized
// against, and classifies a zero-row delete as ErrConcurrentModification or ErrNotFound.
//
// table is a fixed internal constant, never client input (see classifyConditionalWrite).
func (s *Store) deleteDocumentRow(ctx context.Context, table, idOrSlug string, expectedUpdatedAt time.Time) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	var rowID uuid.UUID
	if id, perr := uuid.Parse(idOrSlug); perr == nil {
		rowID = id
	} else {
		if err := tx.QueryRow(ctx, "SELECT id FROM "+table+" WHERE slug = $1", idOrSlug).Scan(&rowID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w", store.ErrNotFound)
			}
			return fmt.Errorf("lookup %s slug: %w", table, err)
		}
	}

	// Lock before deleting so a concurrent replaying create waits here rather than on the row key, and
	// therefore re-reads the tombstone this transaction is about to write.
	if err := lockDocumentID(ctx, tx, table, rowID); err != nil {
		return err
	}

	ct, err := tx.Exec(ctx, "DELETE FROM "+table+" WHERE id = $1 AND updated_at = $2", rowID, expectedUpdatedAt)
	if err != nil {
		return fmt.Errorf("delete %s: %w", table, err)
	}
	if ct.RowsAffected() == 0 {
		return classifyConditionalWrite(ctx, tx, table, rowID)
	}
	// Same transaction as the delete: a tombstone that could be lost would leave the id resurrectable.
	if _, err := tx.Exec(ctx, tombstoneInsert, table, rowID); err != nil {
		return fmt.Errorf("record %s tombstone: %w", table, err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// =============================================================================
// Playlist groups (same row shape as playlists; membership in playlist_group_members)
// =============================================================================

// insertMissingPlaylistsBatch inserts the referenced playlists this feed does not have yet and builds the
// item index for exactly those rows. Membership ingestion may *link* a playlist but must never *modify*
// one: group/channel creation is open, so an ON CONFLICT DO UPDATE here would let anyone who can create a
// channel host a document carrying a victim's playlist id and overwrite that playlist's body, slug, owner
// set and item index on this feed. A referenced id that already exists is therefore left exactly as
// stored, and the remote document's contents are ignored (cross-feed divergence is reconciled elsewhere).
//
// Input may repeat the same id (membership order); DISTINCT ON keeps the first occurrence so the row and
// its item index are always derived from the same body. Only newly inserted rows get an index, which is
// why nothing is cleared first — an existing playlist's index must survive untouched.
// insertMissingPlaylistsBatch inserts referenced playlists this feed does not hold and records what each
// remote URI resolved to.
func insertMissingPlaylistsBatch(ctx context.Context, tx pgx.Tx, playlists []store.IngestedPlaylist) error {
	// One statement: the data-modifying CTE reports which ids it actually inserted (ON CONFLICT DO NOTHING
	// returns nothing for rows that already existed), and the outer INSERT indexes only those.
	const insertMissing = `
WITH input AS (
	SELECT DISTINCT ON (x.id) x.id, x.slug, x.body::jsonb AS body
	FROM unnest($1::uuid[], $2::text[], $3::text[]) WITH ORDINALITY AS x(id, slug, body, ord)
	ORDER BY x.id, x.ord
), inserted AS (
	INSERT INTO playlists (id, slug, body)
	SELECT id, slug, body FROM input
	ON CONFLICT (id) DO NOTHING
	RETURNING id, created_at
), playlist_items AS (
	SELECT inserted.id, inserted.created_at, CASE
		WHEN jsonb_typeof(input.body->'items') = 'array' THEN input.body->'items'
		ELSE '[]'::jsonb
	END AS items
	FROM inserted
	JOIN input ON input.id = inserted.id
)
INSERT INTO playlist_item_index (item_id, playlist_id, playlist_created_at, position, item)
SELECT (elem->>'id')::uuid, playlist_items.id, playlist_items.created_at, (ord - 1)::int, elem
FROM playlist_items, jsonb_array_elements(playlist_items.items) WITH ORDINALITY AS t(elem, ord)`

	if len(playlists) == 0 {
		return nil
	}

	ids := make([]uuid.UUID, len(playlists))
	slugs := make([]string, len(playlists))
	bodies := make([]string, len(playlists))
	for i, p := range playlists {
		if err := requireDocument(p.Raw, "ingested playlist body"); err != nil {
			return err
		}
		ids[i] = p.ID
		slugs[i] = p.Slug
		bodies[i] = string(p.Raw)
	}
	// A tombstoned id is one this feed deleted, so no row exists and the statement below would insert it.
	// Ingestion must not become a side door around the create-path tombstone guard.
	if err := lockDocumentIDs(ctx, tx, "playlists", ids); err != nil {
		return err
	}
	if err := requireNoneTombstoned(ctx, tx, "playlists", ids); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, insertMissing, ids, slugs, bodies); err != nil {
		// ON CONFLICT (id) DO NOTHING absorbs id collisions, but a referenced playlist that is new to this
		// feed can still carry a slug some other live playlist already holds. That is a conflict with
		// existing state, not a server fault.
		if conflict := createConflict(err, "referenced playlist"); conflict != nil {
			return conflict
		}
		return fmt.Errorf("insert referenced playlists: %w", err)
	}
	// Record source mappings for every remote reference in this batch, not only the rows just inserted: a
	// playlist can already be stored (created directly, or ingested from another URI) and still be the
	// first thing a given URI resolves to. That case is precisely the one the mapping has to cover, since
	// otherwise the next ingest of that URI would fetch again to learn an id already known here.
	return recordPlaylistSources(ctx, tx, playlists)
}

// insertPlaylistGroupMembersBatch writes membership rows in playlist order.
// unnest($2::uuid[]) preserves order; WITH ORDINALITY gives 1-based ord → position ord-1.
func insertPlaylistGroupMembersBatch(ctx context.Context, tx pgx.Tx, groupID uuid.UUID, playlists []store.IngestedPlaylist) error {
	const q = `
INSERT INTO playlist_group_members (playlist_group_id, playlist_id, position)
SELECT $1, x.playlist_id, (x.ord - 1)::int
FROM unnest($2::uuid[]) WITH ORDINALITY AS x(playlist_id, ord)`

	if len(playlists) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(playlists))
	for i, p := range playlists {
		ids[i] = p.ID
	}
	_, err := tx.Exec(ctx, q, groupID, ids)
	return err
}

// CreatePlaylistGroup implements store.Store.
//
// Process (single tx): batch-upsert all referenced playlists, insert the group row, and create membership (delete not needed - new group).
func (s *Store) CreatePlaylistGroup(ctx context.Context, in *store.PlaylistGroupInput) error {
	const insertGroup = `
INSERT INTO playlist_groups (id, slug, body)
VALUES ($1, $2, $3::jsonb)`

	if in == nil {
		return fmt.Errorf("nil playlist group input")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockDocumentID(ctx, tx, "playlist_groups", in.ID); err != nil {
		return err
	}
	if err := requireNotTombstoned(ctx, tx, "playlist_groups", in.ID); err != nil {
		return err
	}

	if err := insertMissingPlaylistsBatch(ctx, tx, in.Playlists); err != nil {
		return fmt.Errorf("upsert playlists: %w", err)
	}

	if err := requireDocument(in.Raw, "playlist group body"); err != nil {
		return err
	}
	groupJSON := []byte(in.Raw)

	if _, err := tx.Exec(ctx, insertGroup, in.ID, in.Slug, groupJSON); err != nil {
		if conflict := createConflict(err, "playlist-group"); conflict != nil {
			return conflict
		}
		return fmt.Errorf("insert playlist_group: %w", err)
	}

	if err := insertPlaylistGroupMembersBatch(ctx, tx, in.ID, in.Playlists); err != nil {
		return fmt.Errorf("insert playlist_group_members: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// GetPlaylistGroup implements store.Store.
func (s *Store) GetPlaylistGroup(ctx context.Context, idOrSlug string) (*store.PlaylistGroupRecord, error) {
	const (
		byID = `
SELECT id, slug, body, created_at, updated_at
FROM playlist_groups
WHERE id = $1`

		bySlug = `
SELECT id, slug, body, created_at, updated_at
FROM playlist_groups
WHERE slug = $1`
	)

	id, err := uuid.Parse(idOrSlug)
	var row pgx.Row
	if err == nil {
		row = s.pool.QueryRow(ctx, byID, id)
	} else {
		row = s.pool.QueryRow(ctx, bySlug, idOrSlug)
	}

	var rec store.PlaylistGroupRecord
	var raw []byte
	if err := row.Scan(&rec.ID, &rec.Slug, &raw, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w", store.ErrNotFound)
		}
		return nil, fmt.Errorf("select playlist_group: %w", err)
	}
	if rec.Raw, rec.Body, err = scanDocument[playlistgroup.Group](raw, "playlist-group body"); err != nil {
		return nil, err
	}
	return &rec, nil
}

// ListPlaylistGroups implements store.Store (same pagination rules as ListPlaylists).
func (s *Store) ListPlaylistGroups(ctx context.Context, p *store.ListPlaylistsParams) ([]store.PlaylistGroupRecord, string, error) {
	const (
		firstPage = `
SELECT id, slug, body, created_at, updated_at
FROM playlist_groups
ORDER BY created_at %s, id %s
LIMIT $1`

		afterCursor = `
SELECT id, slug, body, created_at, updated_at
FROM playlist_groups
WHERE (created_at, id) %s ($2::timestamptz, $3::uuid)
ORDER BY created_at %s, id %s
LIMIT $1`
	)

	if p == nil {
		return nil, "", fmt.Errorf("nil list params")
	}
	limit, err := store.ResolveListLimit(p.Limit)
	if err != nil {
		return nil, "", err
	}
	order := p.Sort.SQLOrder()
	tupleOp := p.Sort.TupleAfterCursorOp()

	var rows pgx.Rows
	if p.Cursor == "" {
		q := fmt.Sprintf(firstPage, order, order)
		rows, err = s.pool.Query(ctx, q, limit+1)
	} else {
		created, id, derr := decodeCursor(p.Cursor)
		if derr != nil {
			return nil, "", fmt.Errorf("cursor: %w", derr)
		}
		q := fmt.Sprintf(afterCursor, tupleOp, order, order)
		rows, err = s.pool.Query(ctx, q, limit+1, created, id)
	}
	if err != nil {
		return nil, "", fmt.Errorf("list playlist_groups: %w", err)
	}
	defer rows.Close()

	var out []store.PlaylistGroupRecord
	for rows.Next() {
		var rec store.PlaylistGroupRecord
		var raw []byte
		if err := rows.Scan(&rec.ID, &rec.Slug, &raw, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, "", fmt.Errorf("scan: %w", err)
		}
		if rec.Raw, rec.Body, err = scanDocument[playlistgroup.Group](raw, "playlist-group body"); err != nil {
			return nil, "", err
		}
		out = append(out, rec)
	}

	nextCursor := ""
	if len(out) > limit {
		last := out[limit-1]
		out = out[:limit]
		nextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, nextCursor, rows.Err()
}

// UpdatePlaylistGroup implements store.Store (updated_at set by trigger; membership replaced).
//
// Process (single tx): batch-upsert all referenced playlists, update the group body, clear and rebuild membership.
func (s *Store) UpdatePlaylistGroup(ctx context.Context, idOrSlug string, in *store.PlaylistGroupInput, expectedUpdatedAt time.Time) error {
	const (
		updateByID     = `UPDATE playlist_groups SET body = $2::jsonb, slug = COALESCE(NULLIF($2::jsonb->>'slug', ''), slug) WHERE id = $1 AND updated_at = $3`
		selectIDBySlug = `SELECT id FROM playlist_groups WHERE slug = $1`
		clearMembers   = `DELETE FROM playlist_group_members WHERE playlist_group_id = $1`
	)

	if in == nil {
		return fmt.Errorf("nil playlist group input")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Resolve row ID
	var rowID uuid.UUID
	if id, perr := uuid.Parse(idOrSlug); perr == nil {
		rowID = id
	} else {
		if err := tx.QueryRow(ctx, selectIDBySlug, idOrSlug).Scan(&rowID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w", store.ErrNotFound)
			}
			return fmt.Errorf("lookup playlist_group slug: %w", err)
		}
	}

	// Upsert playlists
	if err := insertMissingPlaylistsBatch(ctx, tx, in.Playlists); err != nil {
		return fmt.Errorf("upsert playlists: %w", err)
	}

	// Update group body
	if err := requireDocument(in.Raw, "playlist group body"); err != nil {
		return err
	}
	groupJSON := []byte(in.Raw)

	ct, err := tx.Exec(ctx, updateByID, rowID, groupJSON, expectedUpdatedAt)
	if err != nil {
		return fmt.Errorf("update playlist_group: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return classifyConditionalWrite(ctx, tx, "playlist_groups", rowID)
	}

	// Replace membership
	if _, err := tx.Exec(ctx, clearMembers, rowID); err != nil {
		return fmt.Errorf("clear playlist_group_members: %w", err)
	}
	if err := insertPlaylistGroupMembersBatch(ctx, tx, rowID, in.Playlists); err != nil {
		return fmt.Errorf("insert playlist_group_members: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ListPlaylistsInGroup implements store.Store.
func (s *Store) ListPlaylistsInGroup(ctx context.Context, idOrSlug string) ([]store.PlaylistRecord, error) {
	// First verify the group exists
	if _, err := s.GetPlaylistGroup(ctx, idOrSlug); err != nil {
		return nil, err
	}

	const (
		byID = `
SELECT p.id, p.slug, p.body, p.created_at, p.updated_at
FROM playlist_group_members m
JOIN playlists p ON p.id = m.playlist_id
WHERE m.playlist_group_id = $1
ORDER BY m.position`

		bySlug = `
SELECT p.id, p.slug, p.body, p.created_at, p.updated_at
FROM playlist_group_members m
JOIN playlists p ON p.id = m.playlist_id
JOIN playlist_groups g ON g.id = m.playlist_group_id
WHERE g.slug = $1
ORDER BY m.position`
	)

	id, err := uuid.Parse(idOrSlug)
	var rows pgx.Rows
	if err == nil {
		rows, err = s.pool.Query(ctx, byID, id)
	} else {
		rows, err = s.pool.Query(ctx, bySlug, idOrSlug)
	}
	if err != nil {
		return nil, fmt.Errorf("list playlists in group: %w", err)
	}
	defer rows.Close()

	var out []store.PlaylistRecord
	for rows.Next() {
		var rec store.PlaylistRecord
		var raw []byte
		if err := rows.Scan(&rec.ID, &rec.Slug, &raw, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if rec.Raw, rec.Body, err = scanDocument[playlist.Playlist](raw, "playlist body"); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeletePlaylistGroup implements store.Store.
func (s *Store) DeletePlaylistGroup(ctx context.Context, idOrSlug string, expectedUpdatedAt time.Time) error {
	return s.deleteDocumentRow(ctx, "playlist_groups", idOrSlug, expectedUpdatedAt)
}

// =============================================================================
// Channels (channels extension document + channel_members)
// =============================================================================

// insertChannelMembersBatch writes membership rows in playlist order (same unnest pattern as playlist groups).
func insertChannelMembersBatch(ctx context.Context, tx pgx.Tx, channelID uuid.UUID, playlists []store.IngestedPlaylist) error {
	const q = `
INSERT INTO channel_members (channel_id, playlist_id, position)
SELECT $1, x.playlist_id, (x.ord - 1)::int
FROM unnest($2::uuid[]) WITH ORDINALITY AS x(playlist_id, ord)`

	if len(playlists) == 0 {
		return nil
	}
	ids := make([]uuid.UUID, len(playlists))
	for i, p := range playlists {
		ids[i] = p.ID
	}
	_, err := tx.Exec(ctx, q, channelID, ids)
	return err
}

// CreateChannel implements store.Store.
//
// Process mirrors CreatePlaylistGroup: batch-upsert playlists, insert channel row, create channel_members from slice order.
func (s *Store) CreateChannel(ctx context.Context, in *store.ChannelInput) error {
	const insertChannel = `
INSERT INTO channels (id, slug, body)
VALUES ($1, $2, $3::jsonb)`

	if in == nil {
		return fmt.Errorf("nil channel input")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if err := lockDocumentID(ctx, tx, "channels", in.ID); err != nil {
		return err
	}
	if err := requireNotTombstoned(ctx, tx, "channels", in.ID); err != nil {
		return err
	}

	if err := insertMissingPlaylistsBatch(ctx, tx, in.Playlists); err != nil {
		return fmt.Errorf("upsert playlists: %w", err)
	}

	if err := requireDocument(in.Raw, "channel body"); err != nil {
		return err
	}
	chJSON := []byte(in.Raw)

	if _, err := tx.Exec(ctx, insertChannel, in.ID, in.Slug, chJSON); err != nil {
		if conflict := createConflict(err, "channel"); conflict != nil {
			return conflict
		}
		return fmt.Errorf("insert channel: %w", err)
	}

	if err := insertChannelMembersBatch(ctx, tx, in.ID, in.Playlists); err != nil {
		return fmt.Errorf("insert channel_members: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// GetChannel implements store.Store.
func (s *Store) GetChannel(ctx context.Context, idOrSlug string) (*store.ChannelRecord, error) {
	const (
		byID = `
SELECT id, slug, body, created_at, updated_at
FROM channels
WHERE id = $1`

		bySlug = `
SELECT id, slug, body, created_at, updated_at
FROM channels
WHERE slug = $1`
	)

	id, err := uuid.Parse(idOrSlug)
	var row pgx.Row
	if err == nil {
		row = s.pool.QueryRow(ctx, byID, id)
	} else {
		row = s.pool.QueryRow(ctx, bySlug, idOrSlug)
	}

	var rec store.ChannelRecord
	var raw []byte
	if err := row.Scan(&rec.ID, &rec.Slug, &raw, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, fmt.Errorf("%w", store.ErrNotFound)
		}
		return nil, fmt.Errorf("select channel: %w", err)
	}
	if rec.Raw, rec.Body, err = scanDocument[channels.Channel](raw, "channel body"); err != nil {
		return nil, err
	}
	return &rec, nil
}

// ListChannels implements store.Store (same pagination rules as ListPlaylists).
func (s *Store) ListChannels(ctx context.Context, p *store.ListPlaylistsParams) ([]store.ChannelRecord, string, error) {
	const (
		firstPage = `
SELECT id, slug, body, created_at, updated_at
FROM channels
ORDER BY created_at %s, id %s
LIMIT $1`

		afterCursor = `
SELECT id, slug, body, created_at, updated_at
FROM channels
WHERE (created_at, id) %s ($2::timestamptz, $3::uuid)
ORDER BY created_at %s, id %s
LIMIT $1`
	)

	if p == nil {
		return nil, "", fmt.Errorf("nil list params")
	}
	limit, err := store.ResolveListLimit(p.Limit)
	if err != nil {
		return nil, "", err
	}
	order := p.Sort.SQLOrder()
	tupleOp := p.Sort.TupleAfterCursorOp()

	var rows pgx.Rows
	if p.Cursor == "" {
		q := fmt.Sprintf(firstPage, order, order)
		rows, err = s.pool.Query(ctx, q, limit+1)
	} else {
		created, id, derr := decodeCursor(p.Cursor)
		if derr != nil {
			return nil, "", fmt.Errorf("cursor: %w", derr)
		}
		q := fmt.Sprintf(afterCursor, tupleOp, order, order)
		rows, err = s.pool.Query(ctx, q, limit+1, created, id)
	}
	if err != nil {
		return nil, "", fmt.Errorf("list channels: %w", err)
	}
	defer rows.Close()

	var out []store.ChannelRecord
	for rows.Next() {
		var rec store.ChannelRecord
		var raw []byte
		if err := rows.Scan(&rec.ID, &rec.Slug, &raw, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, "", fmt.Errorf("scan: %w", err)
		}
		if rec.Raw, rec.Body, err = scanDocument[channels.Channel](raw, "channel body"); err != nil {
			return nil, "", err
		}
		out = append(out, rec)
	}

	nextCursor := ""
	if len(out) > limit {
		last := out[limit-1]
		out = out[:limit]
		nextCursor = encodeCursor(last.CreatedAt, last.ID)
	}
	return out, nextCursor, rows.Err()
}

// UpdateChannel implements store.Store (updated_at set by trigger; membership replaced).
//
// Process (single tx): batch-upsert all referenced playlists, update the channel body, clear and rebuild membership.
func (s *Store) UpdateChannel(ctx context.Context, idOrSlug string, in *store.ChannelInput, expectedUpdatedAt time.Time) error {
	const (
		updateByID     = `UPDATE channels SET body = $2::jsonb, slug = COALESCE(NULLIF($2::jsonb->>'slug', ''), slug) WHERE id = $1 AND updated_at = $3`
		selectIDBySlug = `SELECT id FROM channels WHERE slug = $1`
		clearMembers   = `DELETE FROM channel_members WHERE channel_id = $1`
	)

	if in == nil {
		return fmt.Errorf("nil channel input")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Resolve row ID
	var rowID uuid.UUID
	if id, perr := uuid.Parse(idOrSlug); perr == nil {
		rowID = id
	} else {
		if err := tx.QueryRow(ctx, selectIDBySlug, idOrSlug).Scan(&rowID); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return fmt.Errorf("%w", store.ErrNotFound)
			}
			return fmt.Errorf("lookup channel slug: %w", err)
		}
	}

	// Upsert playlists
	if err := insertMissingPlaylistsBatch(ctx, tx, in.Playlists); err != nil {
		return fmt.Errorf("upsert playlists: %w", err)
	}

	// Update channel body
	if err := requireDocument(in.Raw, "channel body"); err != nil {
		return err
	}
	chJSON := []byte(in.Raw)

	ct, err := tx.Exec(ctx, updateByID, rowID, chJSON, expectedUpdatedAt)
	if err != nil {
		return fmt.Errorf("update channel: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return classifyConditionalWrite(ctx, tx, "channels", rowID)
	}

	// Replace membership
	if _, err := tx.Exec(ctx, clearMembers, rowID); err != nil {
		return fmt.Errorf("clear channel_members: %w", err)
	}
	if err := insertChannelMembersBatch(ctx, tx, rowID, in.Playlists); err != nil {
		return fmt.Errorf("insert channel_members: %w", err)
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit: %w", err)
	}
	return nil
}

// ListPlaylistsInChannel implements store.Store.
func (s *Store) ListPlaylistsInChannel(ctx context.Context, idOrSlug string) ([]store.PlaylistRecord, error) {
	// First verify the channel exists
	if _, err := s.GetChannel(ctx, idOrSlug); err != nil {
		return nil, err
	}

	const (
		byID = `
SELECT p.id, p.slug, p.body, p.created_at, p.updated_at
FROM channel_members m
JOIN playlists p ON p.id = m.playlist_id
WHERE m.channel_id = $1
ORDER BY m.position`

		bySlug = `
SELECT p.id, p.slug, p.body, p.created_at, p.updated_at
FROM channel_members m
JOIN playlists p ON p.id = m.playlist_id
JOIN channels c ON c.id = m.channel_id
WHERE c.slug = $1
ORDER BY m.position`
	)

	id, err := uuid.Parse(idOrSlug)
	var rows pgx.Rows
	if err == nil {
		rows, err = s.pool.Query(ctx, byID, id)
	} else {
		rows, err = s.pool.Query(ctx, bySlug, idOrSlug)
	}
	if err != nil {
		return nil, fmt.Errorf("list playlists in channel: %w", err)
	}
	defer rows.Close()

	var out []store.PlaylistRecord
	for rows.Next() {
		var rec store.PlaylistRecord
		var raw []byte
		if err := rows.Scan(&rec.ID, &rec.Slug, &raw, &rec.CreatedAt, &rec.UpdatedAt); err != nil {
			return nil, fmt.Errorf("scan: %w", err)
		}
		if rec.Raw, rec.Body, err = scanDocument[playlist.Playlist](raw, "playlist body"); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteChannel implements store.Store.
func (s *Store) DeleteChannel(ctx context.Context, idOrSlug string, expectedUpdatedAt time.Time) error {
	return s.deleteDocumentRow(ctx, "channels", idOrSlug, expectedUpdatedAt)
}

// GetChannelRegistry implements store.Store.
// Returns ordered publishers and their channel URLs (ordered by publisher position, then URL position).
func (s *Store) GetChannelRegistry(ctx context.Context) ([]store.RegistryPublisher, []store.RegistryPublisherChannel, error) {
	const (
		pubQuery = `
			SELECT id, name, position, did, created_at, updated_at
			FROM registry_publishers
			ORDER BY position ASC
		`
		chanQuery = `
			SELECT c.id, c.publisher_id, c.channel_url, c.position, c.created_at
			FROM registry_publisher_channels c
			INNER JOIN registry_publishers p ON p.id = c.publisher_id
			ORDER BY p.position ASC, c.position ASC
		`
	)

	pubs := []store.RegistryPublisher{}
	rows, err := s.pool.Query(ctx, pubQuery)
	if err != nil {
		return nil, nil, fmt.Errorf("get registry publishers: %w", err)
	}
	defer rows.Close()

	for rows.Next() {
		var p store.RegistryPublisher
		var did sql.NullString
		if err := rows.Scan(&p.ID, &p.Name, &p.Position, &did, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, nil, fmt.Errorf("scan registry publisher: %w", err)
		}
		if did.Valid {
			s := did.String
			p.DID = &s
		}
		pubs = append(pubs, p)
	}
	if err := rows.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate registry publishers: %w", err)
	}

	chans := []store.RegistryPublisherChannel{}
	rows2, err := s.pool.Query(ctx, chanQuery)
	if err != nil {
		return nil, nil, fmt.Errorf("get registry channels: %w", err)
	}
	defer rows2.Close()

	for rows2.Next() {
		var c store.RegistryPublisherChannel
		if err := rows2.Scan(&c.ID, &c.PublisherID, &c.ChannelURL, &c.Position, &c.CreatedAt); err != nil {
			return nil, nil, fmt.Errorf("scan registry channel: %w", err)
		}
		chans = append(chans, c)
	}
	if err := rows2.Err(); err != nil {
		return nil, nil, fmt.Errorf("iterate registry channels: %w", err)
	}

	return pubs, chans, nil
}

// ReplaceChannelRegistry implements store.Store.
// Atomically replaces the entire registry (DELETE + INSERT in one transaction).
// Publishers and channels must have positions set (0-indexed).
func (s *Store) ReplaceChannelRegistry(ctx context.Context, publishers []store.RegistryPublisher, channels []store.RegistryPublisherChannel) error {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("replace registry: begin tx: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Delete existing registry data.
	if _, err := tx.Exec(ctx, "DELETE FROM registry_publisher_channels"); err != nil {
		return fmt.Errorf("replace registry: delete channels: %w", err)
	}
	if _, err := tx.Exec(ctx, "DELETE FROM registry_publishers"); err != nil {
		return fmt.Errorf("replace registry: delete publishers: %w", err)
	}

	// Insert publishers.
	const pubInsert = `
		INSERT INTO registry_publishers (id, name, position, did, created_at, updated_at)
		VALUES ($1, $2, $3, $4, now(), now())
	`
	for _, p := range publishers {
		var didArg any
		if p.DID != nil && *p.DID != "" {
			didArg = *p.DID
		}
		if _, err := tx.Exec(ctx, pubInsert, p.ID, p.Name, p.Position, didArg); err != nil {
			return fmt.Errorf("replace registry: insert publisher %q: %w", p.Name, err)
		}
	}

	// Insert channels.
	const chanInsert = `
		INSERT INTO registry_publisher_channels (id, publisher_id, channel_url, position, created_at)
		VALUES ($1, $2, $3, $4, now())
	`
	for _, c := range channels {
		if _, err := tx.Exec(ctx, chanInsert, c.ID, c.PublisherID, c.ChannelURL, c.Position); err != nil {
			return fmt.Errorf("replace registry: insert channel %q: %w", c.ChannelURL, err)
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("replace registry: commit: %w", err)
	}
	return nil
}

// =============================================================================
// List cursor (opaque token for keyset pagination)
// =============================================================================

type cursorPayload struct {
	CreatedAt time.Time `json:"t"`
	ID        uuid.UUID `json:"id"`
}

// encodeCursor builds the next-page token: base64url(JSON { t: created_at, id }).
func encodeCursor(t time.Time, id uuid.UUID) string {
	p := cursorPayload{CreatedAt: t, ID: id}
	b, _ := json.Marshal(p)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeCursor(s string) (time.Time, uuid.UUID, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, uuid.Nil, err
	}
	var p cursorPayload
	if err := json.Unmarshal(b, &p); err != nil {
		return time.Time{}, uuid.Nil, err
	}
	return p.CreatedAt, p.ID, nil
}

type playlistItemCursorPayload struct {
	T   time.Time `json:"t"`   // playlist row created_at
	Pos int       `json:"pos"` // item position in playlist
	IID uuid.UUID `json:"iid"` // item id (tie-break)
}

func encodePlaylistItemCursor(plCreated time.Time, pos int, itemID uuid.UUID) string {
	p := playlistItemCursorPayload{T: plCreated, Pos: pos, IID: itemID}
	b, _ := json.Marshal(p)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodePlaylistItemCursor(s string) (plCreated time.Time, pos int, itemID uuid.UUID, err error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return time.Time{}, 0, uuid.Nil, err
	}
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(b, &raw); err != nil {
		return time.Time{}, 0, uuid.Nil, err
	}
	if _, ok := raw["ics"]; ok {
		return time.Time{}, 0, uuid.Nil, fmt.Errorf("stale playlist-item cursor")
	}
	if _, ok := raw["pid"]; ok {
		return time.Time{}, 0, uuid.Nil, fmt.Errorf("stale playlist-item cursor")
	}
	var wire playlistItemCursorPayload
	if err := json.Unmarshal(b, &wire); err != nil {
		return time.Time{}, 0, uuid.Nil, err
	}
	if wire.IID == uuid.Nil {
		return time.Time{}, 0, uuid.Nil, fmt.Errorf("invalid playlist-item cursor")
	}
	return wire.T, wire.Pos, wire.IID, nil
}
