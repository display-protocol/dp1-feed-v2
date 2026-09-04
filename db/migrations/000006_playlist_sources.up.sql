-- playlist_sources: last known resolution of a remote playlist URI.
--
-- Why this exists: a remote reference's identity is discoverable only by fetching it, so re-creating or
-- replacing a group or channel failed whenever the referencing origin was unreachable — even though the
-- stored member could not change, because ingestion never refreshes one. This table is the fallback that
-- keeps such a write working: when the fetch fails, the URI resolves to whatever it last resolved to.
--
-- It is deliberately a CACHE, not an authority. Resolution fetches first and only consults this table when
-- the fetch fails, so a URI that legitimately starts serving a different playlist is picked up normally.
-- An earlier revision consulted the mapping *before* fetching, which silently pinned a URI to whatever it
-- first resolved to, globally and permanently: since creation is open, the first caller to reference a URI
-- fixed its meaning for every later curator, and a publisher re-pointing their own URL was never seen
-- again. Fetch-first keeps the outage protection without that.
--
-- Because it is last-known-good rather than first-seen, writes UPSERT (see recordPlaylistSources): a
-- successful resolution refreshes the row so the fallback stays current. Nothing else depends on the row,
-- so a stale entry can only ever be consulted when the origin is down, and is corrected on the next
-- successful fetch.
--
-- uri_hash is the primary key, not uri. Postgres cannot index a btree entry larger than about 2704 bytes,
-- and URIs arrive inside client-submitted documents with no length limit of their own, so keying on the
-- text turned a long incompressible URI into a failed INSERT and a 500 for client input.
--
-- The hash is written by the application rather than being a generated column: convert_to() is STABLE,
-- not IMMUTABLE, so sha256(convert_to(uri, 'UTF8')) is rejected in a generated-column or index
-- expression. It is fine in the plain INSERT below, which is why the seed can compute it in SQL while the
-- runtime path computes it in Go (see recordPlaylistSources / GetPlaylistBySourceURI). uri is still
-- stored, for operators and for the "which URIs point here" direction — not unique, since several URIs
-- may resolve to one playlist, which is why playlist_id is not a key.
--
-- ON DELETE CASCADE: a deleted playlist's id is tombstoned and must not be resurrected, so a mapping
-- pointing at a dead row would be worse than no mapping. Dropping it sends the next ingest back through
-- the full create bar, where the tombstone guard refuses the id.

CREATE TABLE IF NOT EXISTS playlist_sources (
    uri TEXT NOT NULL,
    uri_hash BYTEA PRIMARY KEY,
    playlist_id UUID NOT NULL REFERENCES playlists (id) ON DELETE CASCADE,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- Supports the cascade and the "which URIs point at this playlist" direction.
CREATE INDEX IF NOT EXISTS playlist_sources_playlist_id_idx ON playlist_sources (playlist_id);

-- Seed the cache from memberships that predate this table.
--
-- Without this the fallback is empty on upgrade, so an outage immediately after deploying would still
-- break writes for every existing remote reference until each happened to be re-ingested. The pairing is
-- recoverable from what is already stored: membership rows are written with position = index of the URI
-- in the document's "playlists" array (see insertPlaylistGroupMembersBatch / insertChannelMembersBatch),
-- so position joins a stored URI to the playlist it resolved to. jsonb_array_elements_text WITH ORDINALITY
-- gives 1-based ordinals, hence ord - 1.
--
-- Newest membership wins, matching the last-known-good rule the runtime path follows. Ordering is by the
-- containing document's updated_at, not created_at: membership is rewritten wholesale on every PUT, so an
-- older group replaced yesterday holds a more recent resolution than a newer group untouched since
-- creation. Ordering by creation time would then pick the staler mapping. playlist_id breaks ties so the
-- result is reproducible.
--
-- The scheme match is case-insensitive (~*). Runtime scheme checks use EqualFold, so a stored "HTTPS://…"
-- reference is valid and must be seeded; a case-sensitive match silently skipped it and left exactly the
-- kind of reference this seed exists to protect without a fallback.
--
-- References are trimmed FIRST, because runtime trims before it fetches and before it looks the URI up.
-- Seeding the raw value therefore missed twice over: a leading space failed the scheme match outright, and
-- a trailing space produced a hash that the trimmed runtime lookup could never find. Trimming here — before
-- the scheme match, the length check, deduplication, ordering and the hash — is what makes a seeded row
-- reachable. The trimmed value is also what gets stored, matching what runtime records.
--
-- btrim covers ASCII whitespace; Go's strings.TrimSpace additionally trims Unicode spaces such as U+00A0.
-- A reference padded with those is not seeded, which costs one fetch on its next ingest and then records
-- the row correctly. The cache is self-healing, so an approximate warm start is the right trade against
-- reproducing Go's exact whitespace table in SQL.
--
-- Same-origin URLs are NOT filtered out. They are inert here: resolveOnePlaylistRef checks
-- isLocalPlaylistURL before it ever consults this table, so such a row is never read. An earlier revision
-- excluded anything shaped like '%/api/v1/playlists/%' to avoid recording them, which silently excluded
-- every *remote* DP-1 feed as well — those use the same route shape — and so skipped almost exactly the
-- population the seed exists for.

INSERT INTO playlist_sources (uri, uri_hash, playlist_id)
SELECT DISTINCT ON (refs.uri) refs.uri, sha256(convert_to(refs.uri, 'UTF8')), refs.playlist_id
FROM (
    SELECT btrim(ref.uri, E' \t\n\r\f\v') AS uri, pgm.playlist_id, g.updated_at
    FROM playlist_groups g
    CROSS JOIN LATERAL jsonb_array_elements_text(
        CASE WHEN jsonb_typeof(g.body -> 'playlists') = 'array'
             THEN g.body -> 'playlists'
             ELSE '[]'::jsonb END
    ) WITH ORDINALITY AS ref(uri, ord)
    JOIN playlist_group_members pgm
      ON pgm.playlist_group_id = g.id
     AND pgm.position = (ref.ord - 1)::int

    UNION ALL

    SELECT btrim(ref.uri, E' \t\n\r\f\v') AS uri, cm.playlist_id, c.updated_at
    FROM channels c
    CROSS JOIN LATERAL jsonb_array_elements_text(
        CASE WHEN jsonb_typeof(c.body -> 'playlists') = 'array'
             THEN c.body -> 'playlists'
             ELSE '[]'::jsonb END
    ) WITH ORDINALITY AS ref(uri, ord)
    JOIN channel_members cm
      ON cm.channel_id = c.id
     AND cm.position = (ref.ord - 1)::int
) AS refs
WHERE refs.uri ~* '^https?://'
  -- Skip anything too long to be a real URL. Nothing here can fail on length (the key is a hash), but a
  -- multi-kilobyte string is junk rather than a reference worth caching.
  AND length(refs.uri) <= 2048
ORDER BY refs.uri, refs.updated_at DESC, refs.playlist_id
ON CONFLICT (uri_hash) DO NOTHING;
