-- Backfill playlist_sources for memberships that predate it.
--
-- Migration 000006 created the mapping empty, so only references ingested afterwards benefited. An
-- upgraded deployment still holding groups and channels with remote references would have kept fetching
-- their origins on the next PUT — exactly the outage coupling 000006 exists to remove — until each one
-- happened to be re-ingested. The mapping is derivable from what is already stored, so there is no reason
-- to make operators wait for that.
--
-- How the pairing is recovered: membership rows are written with position = index of the URI in the
-- document's "playlists" array (see insertPlaylistGroupMembersBatch / insertChannelMembersBatch), so
-- position joins a stored URI to the playlist it resolved to. jsonb_array_elements_text WITH ORDINALITY
-- gives 1-based ordinals, hence ord - 1.
--
-- Only remote references are recorded. A same-origin URL carries its id or slug in the path and needs no
-- mapping, and storing one would be misleading; they are excluded by requiring a scheme-qualified URI
-- that does not point at this feed's own playlist API. This deliberately errs toward recording nothing
-- rather than a wrong mapping: public_base_url is not visible to SQL, so a same-origin URL cannot be
-- identified here with certainty. The '%/api/v1/playlists/%' filter drops any URL shaped like this feed's
-- own playlist route regardless of host, which may skip a genuinely remote DP-1 feed's URL. That costs
-- one fetch on first re-ingest, which then records the mapping correctly — strictly better than pointing
-- a URI at the wrong playlist, which nothing would later correct.
--
-- Conflicting history: the same URI may appear in several documents and, if an origin changed between
-- ingests, resolve to different playlists. ON CONFLICT DO NOTHING plus a deterministic ORDER BY makes the
-- outcome reproducible rather than arbitrary: the oldest membership wins, which is the closest available
-- analogue of the first-ingest-wins rule the runtime path follows. Re-running this migration is a no-op.

INSERT INTO playlist_sources (uri, playlist_id)
SELECT DISTINCT ON (refs.uri) refs.uri, refs.playlist_id
FROM (
    SELECT
        ref.uri,
        pgm.playlist_id,
        g.created_at
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

    SELECT
        ref.uri,
        cm.playlist_id,
        c.created_at
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
WHERE refs.uri ~ '^https?://'
  AND refs.uri NOT LIKE '%/api/v1/playlists/%'
ORDER BY refs.uri, refs.created_at, refs.playlist_id
ON CONFLICT (uri) DO NOTHING;
