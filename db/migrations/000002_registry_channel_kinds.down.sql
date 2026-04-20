-- Revert 000002_registry_channel_kinds.up.sql
-- Rolling back drops all living-channel URLs (cannot merge into a single list safely).

DELETE FROM registry_publisher_channels WHERE kind = 'living';

ALTER TABLE registry_publisher_channels
    DROP CONSTRAINT IF EXISTS registry_publisher_channels_publisher_kind_position_unique;

ALTER TABLE registry_publisher_channels
    DROP CONSTRAINT IF EXISTS registry_publisher_channels_kind_check;

ALTER TABLE registry_publisher_channels
    DROP COLUMN IF EXISTS kind;

ALTER TABLE registry_publisher_channels
    ADD CONSTRAINT registry_publisher_channels_publisher_position_unique UNIQUE (publisher_id, position);

ALTER TABLE registry_publishers
    DROP COLUMN IF EXISTS did;
