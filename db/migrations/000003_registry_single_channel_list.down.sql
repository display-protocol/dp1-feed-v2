-- Revert 000003_registry_single_channel_list.up.sql
-- Restores the kind column; every existing URL becomes 'static' because the
-- original static/living split is not recoverable once the lists are merged.

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
