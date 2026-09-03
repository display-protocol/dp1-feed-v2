-- playlist_sources: remembers which remote URI a stored playlist was ingested from.
--
-- Why this exists: ingestion is reference-only — once a playlist is stored here, a group or channel that
-- names it is only ever *linked* to the stored row, and the remote representation is never consulted
-- again. But a remote reference's identity was discoverable only by fetching it, so re-creating or
-- replacing a group during an upstream outage failed even though, by contract, nothing about the stored
-- playlist could change. The origin could not rewrite a member, yet it could still block one.
--
-- With this mapping the id is resolved from local state first and the fetch is skipped entirely for a
-- reference this feed has already ingested. That closes the availability hole and removes an outbound
-- request per known reference, which also lowers the fan-out ceiling on group/channel writes.
--
-- uri is the primary key: one URI resolves to exactly one playlist. The first successful ingest wins and
-- later ones do not overwrite it (ON CONFLICT DO NOTHING at the call site), which is the same
-- never-refresh rule the rest of ingestion follows — a URI that starts serving a different document must
-- not silently re-point an existing reference. The reverse direction is not unique: several URIs may map
-- to the same playlist, which is why playlist_id is not a key here.
--
-- ON DELETE CASCADE: when a playlist is deleted its id is tombstoned and must not be resurrected, so a
-- stale mapping pointing at a dead row would be worse than no mapping. Dropping it sends the next ingest
-- back through the full create bar, where the tombstone guard refuses the id.

CREATE TABLE IF NOT EXISTS playlist_sources (
    uri TEXT PRIMARY KEY,
    playlist_id UUID NOT NULL REFERENCES playlists (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Supports the cascade and any future "which URIs point here" lookup.
CREATE INDEX IF NOT EXISTS playlist_sources_playlist_id_idx ON playlist_sources (playlist_id);
