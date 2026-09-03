-- Dropping the mapping returns remote reference resolution to fetch-first, so re-ingesting a group or
-- channel that names an already-stored remote playlist depends on that origin being reachable again.

DROP INDEX IF EXISTS playlist_sources_playlist_id_idx;
DROP TABLE IF EXISTS playlist_sources;
