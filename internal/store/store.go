// Package store defines persistence boundaries. Implementations live in store/pg (pgx).
// Callers depend on this interface so tests can swap storage without leaking *pgxpool.Pool.
package store

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/display-protocol/dp1-go/extension/channels"
	"github.com/display-protocol/dp1-go/playlist"
	"github.com/display-protocol/dp1-go/playlistgroup"
	"github.com/google/uuid"
)

// ErrNotFound is returned when a requested row does not exist.
var ErrNotFound = errors.New("not found")

// Documents are written and read as raw JSON, never re-marshaled through the typed dp1-go structs.
//
// Why: DP-1 §7.1 signs the JCS form of the *entire* document, so every signer's payload_hash is bound
// to the exact bytes they signed. A typed round-trip is lossy — `omitempty` drops present-but-empty
// values, unknown keys vanish, and numbers are re-typed — which silently changes the JCS payload and
// orphans every non-feed signature. Raw is therefore the source of truth; Body is a decoded view for
// internal reads (item index, membership, slug/id lookups) and must never be written back.
//
// The jsonb column reorders keys and normalises numeric text. Both are JCS-neutral (JCS sorts keys and
// canonicalises numbers itself), so stored bytes stay hash-equivalent to what was signed. Anything that
// is *not* JCS-neutral must not happen on the write path.

// PlaylistRecord is a stored DP-1 playlist document.
type PlaylistRecord struct {
	ID   uuid.UUID
	Slug string
	// Raw is the persisted document exactly as jsonb returns it; served verbatim.
	Raw json.RawMessage
	// Body is Raw decoded for internal reads; never persist it.
	Body      playlist.Playlist
	CreatedAt time.Time
	UpdatedAt time.Time
}

// PlaylistGroupRecord is a stored DP-1 playlist-group (exhibition) document.
type PlaylistGroupRecord struct {
	ID        uuid.UUID
	Slug      string
	Raw       json.RawMessage
	Body      playlistgroup.Group
	CreatedAt time.Time
	UpdatedAt time.Time
}

// ChannelRecord is a stored DP-1 channels extension document.
type ChannelRecord struct {
	ID        uuid.UUID
	Slug      string
	Raw       json.RawMessage
	Body      channels.Channel
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RegistryPublisher is a curated channel publisher with ordered channel URLs.
type RegistryPublisher struct {
	ID        uuid.UUID
	Name      string
	DID       *string // Optional DID; nil when unset in the database.
	Position  int
	CreatedAt time.Time
	UpdatedAt time.Time
}

// RegistryPublisherChannel is a channel URL belonging to a publisher.
type RegistryPublisherChannel struct {
	ID          uuid.UUID
	PublisherID uuid.UUID
	ChannelURL  string
	Position    int
	CreatedAt   time.Time
}

// PlaylistItemRecord is a denormalized playlist item from playlist_item_index.
type PlaylistItemRecord struct {
	ItemID     uuid.UUID
	PlaylistID uuid.UUID
	Position   int
	Item       playlist.PlaylistItem
}

// ListPlaylistsParams filters list results.
// ChannelFilter and PlaylistGroupFilter are mutually exclusive when both non-empty (HTTP returns 400).
type ListPlaylistsParams struct {
	// Limit is the maximum rows to return (validated in the store layer via ResolveListLimit).
	Limit int
	// Cursor is an opaque pagination token from a previous response (empty = first page).
	Cursor string
	// Sort orders by created_at (see SortAsc / SortDesc).
	Sort SortOrder
	// ChannelFilter, if non-empty, restricts to playlists that are members of that channel (UUID or slug).
	ChannelFilter string
	// PlaylistGroupFilter, if non-empty, restricts to playlists that are members of that group (UUID or slug).
	PlaylistGroupFilter string
}

// ListPlaylistItemsParams lists rows from playlist_item_index for stable keyset pagination.
// ChannelFilter and PlaylistGroupFilter are mutually exclusive when both non-empty (HTTP returns 400).
// Sort order: (1) playlist_created_at (denormalized from playlists.created_at on the index row), (2) position, (3) item_id.
type ListPlaylistItemsParams struct {
	Limit               int
	Cursor              string
	Sort                SortOrder
	ChannelFilter       string
	PlaylistGroupFilter string
}

// IngestedPlaylist is a playlist row to upsert, with its item index, while committing a group or channel.
// Order in the slice is membership order; if an ID repeats, the first body is authoritative.
// Raw is the member document verbatim (a fetched remote body or a stored local Raw), so the member's
// own signatures survive the upsert.
type IngestedPlaylist struct {
	ID   uuid.UUID
	Slug string
	Raw  json.RawMessage
}

// PlaylistGroupInput is passed to Store.CreatePlaylistGroup: the group row and playlists to upsert.
// Slice order is membership order (position = index). Raw is the signed group document.
type PlaylistGroupInput struct {
	ID        uuid.UUID
	Slug      string
	Raw       json.RawMessage
	Playlists []IngestedPlaylist
}

// ChannelInput is passed to Store.CreateChannel (same shape as PlaylistGroupInput for the channel row).
type ChannelInput struct {
	ID        uuid.UUID
	Slug      string
	Raw       json.RawMessage
	Playlists []IngestedPlaylist
}

// Store is the feed persistence contract (PostgreSQL implementation in store/pg).
//
// Gomock: generated type mocks.MockStore in internal/mocks/store_mock.go.
// Regenerate all mocks from repository root: go generate ./...
// (directives in internal/mocks/doc.go; uses go tool mockgen from go.mod tools.)
type Store interface {
	// Ping checks that the database is reachable.
	Ping(ctx context.Context) error

	// CreatePlaylist inserts a new playlist row from the signed document bytes and playlist_item_index rows
	// derived from its items (same transaction). raw must be a validated DP-1 playlist JSON object.
	CreatePlaylist(ctx context.Context, id uuid.UUID, slug string, raw json.RawMessage) error
	// GetPlaylist loads a playlist by UUID or slug.
	GetPlaylist(ctx context.Context, idOrSlug string) (*PlaylistRecord, error)
	// GetPlaylistItems returns all indexed items for a playlist, ordered by position.
	GetPlaylistItems(ctx context.Context, idOrSlug string) ([]PlaylistItemRecord, error)
	// ListPlaylistItems returns a page of indexed items across playlists (optional channel or playlist-group filter).
	ListPlaylistItems(ctx context.Context, p *ListPlaylistItemsParams) ([]PlaylistItemRecord, string, error)
	// GetPlaylistItem returns one indexed item by its item UUID.
	GetPlaylistItem(ctx context.Context, itemID uuid.UUID) (*PlaylistItemRecord, error)
	// ListPlaylists returns a page of playlists ordered by created_at and Sort.
	ListPlaylists(ctx context.Context, p *ListPlaylistsParams) ([]PlaylistRecord, string, error)
	// UpdatePlaylist replaces the stored document bytes and rebuilds playlist_item_index from its items (same transaction).
	// The slug column follows the document's "slug" when present (a row must be addressable by the slug it serves);
	// identity is pinned by the executor, so in practice this re-writes the same slug.
	UpdatePlaylist(ctx context.Context, idOrSlug string, raw json.RawMessage) error
	// DeletePlaylist removes a playlist row.
	DeletePlaylist(ctx context.Context, idOrSlug string) error

	// CreatePlaylistGroup upserts playlists and item indexes, inserts the group row, and creates ordered membership (single transaction).
	CreatePlaylistGroup(ctx context.Context, in *PlaylistGroupInput) error
	// GetPlaylistGroup loads a playlist-group by UUID or slug.
	GetPlaylistGroup(ctx context.Context, idOrSlug string) (*PlaylistGroupRecord, error)
	// ListPlaylistGroups returns a page ordered by created_at and Sort.
	ListPlaylistGroups(ctx context.Context, p *ListPlaylistsParams) ([]PlaylistGroupRecord, string, error)
	// UpdatePlaylistGroup upserts playlists and item indexes, updates the group row body (and slug, as UpdatePlaylist), and replaces ordered membership (single transaction).
	UpdatePlaylistGroup(ctx context.Context, idOrSlug string, in *PlaylistGroupInput) error
	// ListPlaylistsInGroup returns full playlist rows in membership order (position 0 first). ErrNotFound if the group does not exist.
	ListPlaylistsInGroup(ctx context.Context, idOrSlug string) ([]PlaylistRecord, error)
	// DeletePlaylistGroup removes a playlist-group row.
	DeletePlaylistGroup(ctx context.Context, idOrSlug string) error

	// CreateChannel upserts playlists and item indexes, inserts the channel row, and creates ordered membership (single transaction).
	CreateChannel(ctx context.Context, in *ChannelInput) error
	// GetChannel loads a channel by UUID or slug.
	GetChannel(ctx context.Context, idOrSlug string) (*ChannelRecord, error)
	// ListChannels returns a page ordered by created_at and Sort.
	ListChannels(ctx context.Context, p *ListPlaylistsParams) ([]ChannelRecord, string, error)
	// UpdateChannel upserts playlists and item indexes, updates the channel row body (and slug, as UpdatePlaylist), and replaces ordered membership (single transaction).
	UpdateChannel(ctx context.Context, idOrSlug string, in *ChannelInput) error
	// ListPlaylistsInChannel returns full playlist rows in membership order (position 0 first). ErrNotFound if the channel does not exist.
	ListPlaylistsInChannel(ctx context.Context, idOrSlug string) ([]PlaylistRecord, error)
	// DeleteChannel removes a channel row.
	DeleteChannel(ctx context.Context, idOrSlug string) error

	// GetChannelRegistry returns the curated channel registry (ordered publishers with their channel URLs).
	GetChannelRegistry(ctx context.Context) ([]RegistryPublisher, []RegistryPublisherChannel, error)
	// ReplaceChannelRegistry atomically replaces the entire registry (publishers + channels) in one transaction.
	ReplaceChannelRegistry(ctx context.Context, publishers []RegistryPublisher, channels []RegistryPublisherChannel) error
}

// TestProvider supplies a [Store] for integration contract tests and manages per-test cleanup.
// Implementations are database-specific (e.g. PostgreSQL in pg/pgtest).
type TestProvider interface {
	// NewStore returns a store instance backed by the test database.
	NewStore() Store

	// Close shuts down the test database and releases resources.
	// Called once after all tests complete (typically in TestMain defer).
	Close()

	// Cleanup resets database state (truncate tables, etc.) for the next test.
	// Implementations should be idempotent and fast (prefer TRUNCATE over DROP/CREATE).
	Cleanup(t testing.TB)
}
