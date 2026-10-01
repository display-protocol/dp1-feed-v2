-- Indexes for the attribution filters on the list endpoints: ?curator= on playlists, playlist-groups and
-- channels, and ?publisher= on channels.
--
-- The filters read the signed document as stored (body), not a derived column. That keeps them a pure read
-- feature: no write path has to maintain anything and no backfill is needed, and the answer is by
-- construction what the served document says. Each index expression must stay textually identical to the
-- predicate in internal/store/pg/store.go (curatorsKeyFilter, groupCuratorFilter, publisherKeyFilter), or
-- the planner cannot use it.
--
-- Index types are chosen because every indexed value is client-supplied and unbounded in length: the DP-1
-- entity `key` pattern is only ^did:[a-z]+:.+$ and a group's `curator` is a free string. A btree entry
-- larger than about 2.7 kB fails the INSERT, which would turn an oversized but otherwise valid document
-- into a 500 on create. Both index types below store hashes instead of the values:
--   - GIN with jsonb_path_ops stores a hash per path-and-value, and serves the @> containment the curators
--     filter uses. The default jsonb_ops class would store the strings themselves and reintroduce the limit.
--   - HASH stores a 32-bit hash per row and serves equality, which is all the scalar filters need.
--
-- Built without CONCURRENTLY: golang-migrate sends the file as one multi-statement Exec, which PostgreSQL
-- runs as a single implicit transaction, where CONCURRENTLY is not allowed. Each build
-- blocks writes to its table for its duration, which is short at this feed's table sizes.

CREATE INDEX IF NOT EXISTS idx_playlists_curators
    ON playlists USING GIN ((body -> 'curators') jsonb_path_ops);

CREATE INDEX IF NOT EXISTS idx_channels_curators
    ON channels USING GIN ((body -> 'curators') jsonb_path_ops);

CREATE INDEX IF NOT EXISTS idx_channels_publisher_key
    ON channels USING HASH ((body -> 'publisher' ->> 'key'));

CREATE INDEX IF NOT EXISTS idx_playlist_groups_curator
    ON playlist_groups USING HASH ((body ->> 'curator'));
