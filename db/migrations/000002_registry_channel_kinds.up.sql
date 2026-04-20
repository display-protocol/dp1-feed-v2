-- -----------------------------------------------------------------------------
-- Extend curated channel registry: optional publisher DID and static vs living
-- channel URL lists (kind on registry_publisher_channels).
-- Existing rows become kind = 'static' (same URLs as before).
-- -----------------------------------------------------------------------------

ALTER TABLE registry_publishers
    ADD COLUMN IF NOT EXISTS did TEXT NULL;

ALTER TABLE registry_publisher_channels
    DROP CONSTRAINT IF EXISTS registry_publisher_channels_publisher_position_unique;

ALTER TABLE registry_publisher_channels
    ADD COLUMN kind TEXT NOT NULL DEFAULT 'static';

ALTER TABLE registry_publisher_channels
    ADD CONSTRAINT registry_publisher_channels_kind_check CHECK (kind IN ('static', 'living'));

ALTER TABLE registry_publisher_channels
    ADD CONSTRAINT registry_publisher_channels_publisher_kind_position_unique UNIQUE (publisher_id, kind, position);

ALTER TABLE registry_publisher_channels
    ALTER COLUMN kind DROP DEFAULT;
