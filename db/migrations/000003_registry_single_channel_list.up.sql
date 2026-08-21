-- -----------------------------------------------------------------------------
-- Collapse the curated registry's static/living channel lists back into a single
-- ordered list per publisher (revert of the kind split in 000002).
-- The optional publisher DID added in 000002 is kept.
--
-- Existing living URLs are preserved, not dropped: they are appended after the
-- publisher's static URLs and positions are renumbered so (publisher_id, position)
-- is unique again. Renumbering runs while no positional unique constraint exists,
-- so intermediate collisions cannot abort the migration.
-- -----------------------------------------------------------------------------

ALTER TABLE registry_publisher_channels
    DROP CONSTRAINT IF EXISTS registry_publisher_channels_publisher_kind_position_unique;

WITH renumbered AS (
    SELECT id,
           row_number() OVER (
               PARTITION BY publisher_id
               ORDER BY CASE kind WHEN 'static' THEN 0 ELSE 1 END, position ASC, id ASC
           ) - 1 AS new_position
    FROM registry_publisher_channels
)
UPDATE registry_publisher_channels c
SET position = r.new_position
FROM renumbered r
WHERE c.id = r.id;

ALTER TABLE registry_publisher_channels
    DROP CONSTRAINT IF EXISTS registry_publisher_channels_kind_check;

ALTER TABLE registry_publisher_channels
    DROP COLUMN IF EXISTS kind;

ALTER TABLE registry_publisher_channels
    ADD CONSTRAINT registry_publisher_channels_publisher_position_unique UNIQUE (publisher_id, position);
