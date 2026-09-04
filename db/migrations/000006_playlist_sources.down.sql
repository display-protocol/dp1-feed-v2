-- Dropping the cache returns remote reference resolution to fetch-only, so a group or channel naming an
-- already-ingested remote playlist fails while that origin is unreachable instead of falling back to the
-- last known resolution.

DROP INDEX IF EXISTS playlist_sources_playlist_id_idx;
DROP TABLE IF EXISTS playlist_sources;
