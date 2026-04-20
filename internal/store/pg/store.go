// Package pg implements store.Store on PostgreSQL using pgxpool (CRUD, keyset pagination, transactional group/channel ingest).
package pg

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
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
func (s *Store) CreatePlaylist(ctx context.Context, id uuid.UUID, slug string, body *playlist.Playlist) error {
	const insertPlaylist = `
INSERT INTO playlists (id, slug, body)
VALUES ($1, $2, $3::jsonb)
RETURNING created_at`

	if body == nil {
		return fmt.Errorf("nil playlist body")
	}
	bodyJSON, err := utils.EncodeJSONB(body)
	if err != nil {
		return err
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	// Commit succeeds → Rollback becomes no-op. On failure, Rollback aborts the tx.
	defer func() { _ = tx.Rollback(ctx) }()

	var createdAt time.Time
	if err := tx.QueryRow(ctx, insertPlaylist, id, slug, bodyJSON).Scan(&createdAt); err != nil {
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
	pl, err := utils.DecodeJSONB[playlist.Playlist](raw, "playlist body")
	if err != nil {
		return nil, err
	}
	rec.Body = pl
	return &rec, nil
}

// GetPlaylistItems implements store.Store.
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
		pl, err := utils.DecodeJSONB[playlist.Playlist](raw, "playlist body")
		if err != nil {
			return nil, "", err
		}
		rec.Body = pl
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
func (s *Store) UpdatePlaylist(ctx context.Context, idOrSlug string, body *playlist.Playlist) error {
	const (
		updateByID     = `UPDATE playlists SET body = $2::jsonb WHERE id = $1 RETURNING created_at`
		selectIDBySlug = `SELECT id FROM playlists WHERE slug = $1`
		clearItemIndex = `DELETE FROM playlist_item_index WHERE playlist_id = $1`
	)

	if body == nil {
		return fmt.Errorf("nil playlist body")
	}
	bodyJSON, err := utils.EncodeJSONB(body)
	if err != nil {
		return err
	}

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
	err = tx.QueryRow(ctx, updateByID, rowID, bodyJSON).Scan(&playlistCreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("%w", store.ErrNotFound)
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

// DeletePlaylist implements store.Store.
func (s *Store) DeletePlaylist(ctx context.Context, idOrSlug string) error {
	const (
		byID   = `DELETE FROM playlists WHERE id = $1`
		bySlug = `DELETE FROM playlists WHERE slug = $1`
	)

	id, err := uuid.Parse(idOrSlug)
	var ct pgconn.CommandTag
	if err == nil {
		ct, err = s.pool.Exec(ctx, byID, id)
	} else {
		ct, err = s.pool.Exec(ctx, bySlug, idOrSlug)
	}
	if err != nil {
		return fmt.Errorf("delete playlist: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w", store.ErrNotFound)
	}
	return nil
}

// =============================================================================
// Playlist groups (same row shape as playlists; membership in playlist_group_members)
// =============================================================================

// upsertPlaylistsBatch bulk-upserts playlists from a slice.
// Input may repeat the same id (group/channel membership order); DISTINCT ON keeps the first
// occurrence per id for this statement so ON CONFLICT never touches the same row twice.
func upsertPlaylistsBatch(ctx context.Context, tx pgx.Tx, playlists []store.IngestedPlaylist) error {
	const q = `
INSERT INTO playlists (id, slug, body)
SELECT DISTINCT ON (x.id) x.id, x.slug, x.body::jsonb
FROM unnest($1::uuid[], $2::text[], $3::text[]) WITH ORDINALITY AS x(id, slug, body, ord)
ORDER BY x.id, x.ord
ON CONFLICT (id) DO UPDATE SET
	slug = EXCLUDED.slug,
	body = EXCLUDED.body`

	if len(playlists) == 0 {
		return nil
	}

	ids := make([]uuid.UUID, len(playlists))
	slugs := make([]string, len(playlists))
	bodies := make([]string, len(playlists))
	for i, p := range playlists {
		ids[i] = p.ID
		slugs[i] = p.Slug
		b, err := utils.EncodeJSONB(p.Body)
		if err != nil {
			return err
		}
		bodies[i] = string(b)
	}
	_, err := tx.Exec(ctx, q, ids, slugs, bodies)
	return err
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

	if err := upsertPlaylistsBatch(ctx, tx, in.Playlists); err != nil {
		return fmt.Errorf("upsert playlists: %w", err)
	}

	groupJSON, err := utils.EncodeJSONB(in.Body)
	if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, insertGroup, in.ID, in.Slug, groupJSON); err != nil {
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
	g, err := utils.DecodeJSONB[playlistgroup.Group](raw, "playlist-group body")
	if err != nil {
		return nil, err
	}
	rec.Body = g
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
		g, err := utils.DecodeJSONB[playlistgroup.Group](raw, "playlist-group body")
		if err != nil {
			return nil, "", err
		}
		rec.Body = g
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
func (s *Store) UpdatePlaylistGroup(ctx context.Context, idOrSlug string, in *store.PlaylistGroupInput) error {
	const (
		updateByID     = `UPDATE playlist_groups SET body = $2::jsonb WHERE id = $1`
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
	if err := upsertPlaylistsBatch(ctx, tx, in.Playlists); err != nil {
		return fmt.Errorf("upsert playlists: %w", err)
	}

	// Update group body
	groupJSON, err := utils.EncodeJSONB(in.Body)
	if err != nil {
		return err
	}

	ct, err := tx.Exec(ctx, updateByID, rowID, groupJSON)
	if err != nil {
		return fmt.Errorf("update playlist_group: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w", store.ErrNotFound)
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
		pl, err := utils.DecodeJSONB[playlist.Playlist](raw, "playlist body")
		if err != nil {
			return nil, err
		}
		rec.Body = pl
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeletePlaylistGroup implements store.Store.
func (s *Store) DeletePlaylistGroup(ctx context.Context, idOrSlug string) error {
	const (
		byID   = `DELETE FROM playlist_groups WHERE id = $1`
		bySlug = `DELETE FROM playlist_groups WHERE slug = $1`
	)

	id, err := uuid.Parse(idOrSlug)
	var ct pgconn.CommandTag
	if err == nil {
		ct, err = s.pool.Exec(ctx, byID, id)
	} else {
		ct, err = s.pool.Exec(ctx, bySlug, idOrSlug)
	}
	if err != nil {
		return fmt.Errorf("delete playlist_group: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w", store.ErrNotFound)
	}
	return nil
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

	if err := upsertPlaylistsBatch(ctx, tx, in.Playlists); err != nil {
		return fmt.Errorf("upsert playlists: %w", err)
	}

	chJSON, err := utils.EncodeJSONB(in.Body)
	if err != nil {
		return err
	}

	if _, err := tx.Exec(ctx, insertChannel, in.ID, in.Slug, chJSON); err != nil {
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
	ch, err := utils.DecodeJSONB[channels.Channel](raw, "channel body")
	if err != nil {
		return nil, err
	}
	rec.Body = ch
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
		ch, err := utils.DecodeJSONB[channels.Channel](raw, "channel body")
		if err != nil {
			return nil, "", err
		}
		rec.Body = ch
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
func (s *Store) UpdateChannel(ctx context.Context, idOrSlug string, in *store.ChannelInput) error {
	const (
		updateByID     = `UPDATE channels SET body = $2::jsonb WHERE id = $1`
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
	if err := upsertPlaylistsBatch(ctx, tx, in.Playlists); err != nil {
		return fmt.Errorf("upsert playlists: %w", err)
	}

	// Update channel body
	chJSON, err := utils.EncodeJSONB(in.Body)
	if err != nil {
		return err
	}

	ct, err := tx.Exec(ctx, updateByID, rowID, chJSON)
	if err != nil {
		return fmt.Errorf("update channel: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w", store.ErrNotFound)
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
		pl, err := utils.DecodeJSONB[playlist.Playlist](raw, "playlist body")
		if err != nil {
			return nil, err
		}
		rec.Body = pl
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// DeleteChannel implements store.Store.
func (s *Store) DeleteChannel(ctx context.Context, idOrSlug string) error {
	const (
		byID   = `DELETE FROM channels WHERE id = $1`
		bySlug = `DELETE FROM channels WHERE slug = $1`
	)

	id, err := uuid.Parse(idOrSlug)
	var ct pgconn.CommandTag
	if err == nil {
		ct, err = s.pool.Exec(ctx, byID, id)
	} else {
		ct, err = s.pool.Exec(ctx, bySlug, idOrSlug)
	}
	if err != nil {
		return fmt.Errorf("delete channel: %w", err)
	}
	if ct.RowsAffected() == 0 {
		return fmt.Errorf("%w", store.ErrNotFound)
	}
	return nil
}

// GetChannelRegistry implements store.Store.
// Returns ordered publishers and their channel URLs (ordered by publisher position, then kind static before living, then URL position).
func (s *Store) GetChannelRegistry(ctx context.Context) ([]store.RegistryPublisher, []store.RegistryPublisherChannel, error) {
	const (
		pubQuery = `
			SELECT id, name, position, did, created_at, updated_at
			FROM registry_publishers
			ORDER BY position ASC
		`
		chanQuery = `
			SELECT c.id, c.publisher_id, c.channel_url, c.kind, c.position, c.created_at
			FROM registry_publisher_channels c
			INNER JOIN registry_publishers p ON p.id = c.publisher_id
			ORDER BY p.position ASC,
				CASE c.kind WHEN 'static' THEN 0 ELSE 1 END,
				c.position ASC
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
		if err := rows2.Scan(&c.ID, &c.PublisherID, &c.ChannelURL, &c.Kind, &c.Position, &c.CreatedAt); err != nil {
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
		INSERT INTO registry_publisher_channels (id, publisher_id, channel_url, kind, position, created_at)
		VALUES ($1, $2, $3, $4, $5, now())
	`
	for _, c := range channels {
		if _, err := tx.Exec(ctx, chanInsert, c.ID, c.PublisherID, c.ChannelURL, c.Kind, c.Position); err != nil {
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
