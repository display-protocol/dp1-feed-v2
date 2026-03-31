-- =============================================================================
-- Revert 000001_init_schema.up.sql (full teardown)
-- =============================================================================
-- Order matters:
--   1. Drop triggers first so rows are no longer calling the function on DML.
--   2. Drop the trigger function (nothing references it after triggers are gone).
--   3. Drop tables from dependents to referenced parents (FKs): children that
--      reference playlists/groups/channels before those parent tables.

-- Remove updated_at triggers from document tables (playlists, groups, channels, registry).
DROP TRIGGER IF EXISTS tr_registry_publishers_updated_at ON registry_publishers;
DROP TRIGGER IF EXISTS tr_channels_updated_at ON channels;
DROP TRIGGER IF EXISTS tr_playlist_groups_updated_at ON playlist_groups;
DROP TRIGGER IF EXISTS tr_playlists_updated_at ON playlists;

-- Trigger function used only by the triggers above.
DROP FUNCTION IF EXISTS dp1_feed_set_updated_at();

-- playlist_item_index references playlists; membership tables reference playlists + parent doc tables; registry children before parents.
DROP TABLE IF EXISTS registry_publisher_channels;
DROP TABLE IF EXISTS registry_publishers;
DROP TABLE IF EXISTS playlist_item_index;
DROP TABLE IF EXISTS channel_members;
DROP TABLE IF EXISTS playlist_group_members;
DROP TABLE IF EXISTS channels;
DROP TABLE IF EXISTS playlist_groups;
DROP TABLE IF EXISTS playlists;
