-- =============================================================================
-- dp1-feed-v2 initial schema
-- =============================================================================
-- Stores DP-1 documents (playlists, playlist-groups, channels) as JSONB plus
-- relational edges for ordered membership and fast item queries. Application
-- code signs and validates JSON; Postgres enforces referential integrity and
-- timestamps via triggers below.

-- -----------------------------------------------------------------------------
-- playlists: universal DP-1 playlist documents (v1.1+ multisig JSON in body)
-- -----------------------------------------------------------------------------
-- id/slug identify the resource in URLs and inside signed JSON. body is the
-- full stored document. created_at / updated_at are row metadata (document
-- "created" inside JSON may differ; see executor).
CREATE TABLE IF NOT EXISTS playlists (
    id UUID PRIMARY KEY,
    slug TEXT NOT NULL UNIQUE,
    body JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Composite index for keyset pagination (created_at, id) supports both WHERE tuple
-- comparison and ORDER BY for efficient cursor-based pagination.
CREATE INDEX IF NOT EXISTS idx_playlists_created_id ON playlists (created_at, id);

-- -----------------------------------------------------------------------------
-- playlist_groups: DP-1 playlist-group (exhibition) documents
-- -----------------------------------------------------------------------------
-- Same row shape as playlists: one signed JSON document per group.
CREATE TABLE IF NOT EXISTS playlist_groups (
    id UUID PRIMARY KEY,
    slug TEXT NOT NULL UNIQUE,
    body JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_playlist_groups_created_id ON playlist_groups (created_at, id);

-- -----------------------------------------------------------------------------
-- channels: DP-1 channels extension documents
-- -----------------------------------------------------------------------------
-- Same row shape; body follows the channels extension schema when enabled.
CREATE TABLE IF NOT EXISTS channels (
    id UUID PRIMARY KEY,
    slug TEXT NOT NULL UNIQUE,
    body JSONB NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_channels_created_id ON channels (created_at, id);

-- -----------------------------------------------------------------------------
-- playlist_group_members: ordered links group -> playlist
-- -----------------------------------------------------------------------------
-- position 0 is the first entry in the group document's playlists[] URI array.
-- PK (playlist_group_id, position) allows the same playlist_id at multiple
-- positions when the document repeats a URI. ON DELETE CASCADE removes links
-- when the group is deleted; RESTRICT on playlist prevents deleting a playlist
-- that is still referenced by any group.
CREATE TABLE IF NOT EXISTS playlist_group_members (
    playlist_group_id UUID NOT NULL REFERENCES playlist_groups (id) ON DELETE CASCADE,
    playlist_id UUID NOT NULL REFERENCES playlists (id) ON DELETE RESTRICT,
    position INT NOT NULL,
    PRIMARY KEY (playlist_group_id, position)
);

-- -----------------------------------------------------------------------------
-- channel_members: ordered links channel -> playlist
-- -----------------------------------------------------------------------------
-- Same semantics as playlist_group_members for channel documents.
CREATE TABLE IF NOT EXISTS channel_members (
    channel_id UUID NOT NULL REFERENCES channels (id) ON DELETE CASCADE,
    playlist_id UUID NOT NULL REFERENCES playlists (id) ON DELETE RESTRICT,
    position INT NOT NULL,
    PRIMARY KEY (channel_id, position)
);

-- Reverse lookup indexes for "which groups/channels contain this playlist?" queries.
CREATE INDEX IF NOT EXISTS idx_playlist_group_members_playlist_id ON playlist_group_members (playlist_id);
CREATE INDEX IF NOT EXISTS idx_channel_members_playlist_id ON channel_members (playlist_id);

-- -----------------------------------------------------------------------------
-- playlist_item_index: denormalized playlist items for GET /playlist-items
-- -----------------------------------------------------------------------------
-- Rebuilt whenever a playlist body changes (same transaction as playlist write).
-- item is the JSON object for one entry in the playlist's items[] array.
-- playlist_created_at is denormalized from playlists.created_at when rows are inserted (same value for all items of a playlist).
CREATE TABLE IF NOT EXISTS playlist_item_index (
    item_id UUID NOT NULL,
    playlist_id UUID NOT NULL REFERENCES playlists (id) ON DELETE CASCADE,
    playlist_created_at TIMESTAMPTZ NOT NULL,
    position INT NOT NULL,
    item JSONB NOT NULL,
    PRIMARY KEY (playlist_id, item_id)
);

-- Reverse lookup by item id (e.g. "which playlist contains this item?").
CREATE INDEX IF NOT EXISTS idx_playlist_item_index_item ON playlist_item_index (item_id);

-- Per-playlist item order (GetPlaylistItems and probes by playlist_id).
CREATE INDEX IF NOT EXISTS idx_playlist_item_index_playlist_position
    ON playlist_item_index (playlist_id ASC, position ASC);

-- Global GET /playlist-items list order: playlist created time, then position, then item id (matches application ORDER BY).
CREATE INDEX IF NOT EXISTS idx_playlist_item_index_created_position_item
    ON playlist_item_index (playlist_created_at ASC, position ASC, item_id ASC);

-- -----------------------------------------------------------------------------
-- Trigger function: bump updated_at on document tables
-- -----------------------------------------------------------------------------
-- Shared by playlists, playlist_groups, and channels. Application UPDATEs may
-- omit updated_at; the trigger sets it to now() on every insert/update.
CREATE OR REPLACE FUNCTION dp1_feed_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- -----------------------------------------------------------------------------
-- Triggers: attach updated_at maintainer to each document table
-- -----------------------------------------------------------------------------
DROP TRIGGER IF EXISTS tr_playlists_updated_at ON playlists;
CREATE TRIGGER tr_playlists_updated_at
    BEFORE INSERT OR UPDATE ON playlists
    FOR EACH ROW
    EXECUTE PROCEDURE dp1_feed_set_updated_at();

DROP TRIGGER IF EXISTS tr_playlist_groups_updated_at ON playlist_groups;
CREATE TRIGGER tr_playlist_groups_updated_at
    BEFORE INSERT OR UPDATE ON playlist_groups
    FOR EACH ROW
    EXECUTE PROCEDURE dp1_feed_set_updated_at();

DROP TRIGGER IF EXISTS tr_channels_updated_at ON channels;
CREATE TRIGGER tr_channels_updated_at
    BEFORE INSERT OR UPDATE ON channels
    FOR EACH ROW
    EXECUTE PROCEDURE dp1_feed_set_updated_at();

-- -----------------------------------------------------------------------------
-- registry_publishers: curated channel publishers (ordered)
-- -----------------------------------------------------------------------------
-- Stores a curated list of channel publishers, each with a name and ordered
-- list of channel URLs. Position preserves array ordering from API requests.
CREATE TABLE IF NOT EXISTS registry_publishers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL UNIQUE,
    position INT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_registry_publishers_position ON registry_publishers (position ASC);

DROP TRIGGER IF EXISTS tr_registry_publishers_updated_at ON registry_publishers;
CREATE TRIGGER tr_registry_publishers_updated_at
    BEFORE INSERT OR UPDATE ON registry_publishers
    FOR EACH ROW
    EXECUTE PROCEDURE dp1_feed_set_updated_at();

-- -----------------------------------------------------------------------------
-- registry_publisher_channels: channel URLs per publisher (ordered)
-- -----------------------------------------------------------------------------
-- Each publisher has an ordered list of channel URLs. Position maintains order
-- within the publisher's channel_urls array. CHECK constraint validates URL format.
CREATE TABLE IF NOT EXISTS registry_publisher_channels (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    publisher_id UUID NOT NULL REFERENCES registry_publishers (id) ON DELETE CASCADE,
    channel_url TEXT NOT NULL,
    position INT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT registry_publisher_channels_publisher_position_unique UNIQUE (publisher_id, position)
);

CREATE INDEX IF NOT EXISTS idx_registry_publisher_channels_publisher ON registry_publisher_channels (publisher_id, position ASC);
