-- Restore the curated channel registry tables.
--
-- Structure only. The curated rows existed nowhere else, so stepping back recreates empty tables and the
-- registry reads as having no publishers. Nothing in the application references these tables at this
-- version either; the definitions exist so the schema can be migrated back across this step cleanly.
--
-- This reproduces the shape as of 000003 — the DID column added in 000002 kept, the static/living `kind`
-- split collapsed back to one ordered list per publisher.

CREATE TABLE IF NOT EXISTS registry_publishers (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name TEXT NOT NULL UNIQUE,
    position INT NOT NULL UNIQUE,
    did TEXT,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE INDEX IF NOT EXISTS idx_registry_publishers_position ON registry_publishers ("position");

-- Same trigger the other document tables use, so updated_at is maintained by the database rather than by
-- callers (see 000001).
DROP TRIGGER IF EXISTS tr_registry_publishers_updated_at ON registry_publishers;
CREATE TRIGGER tr_registry_publishers_updated_at
    BEFORE INSERT OR UPDATE ON registry_publishers
    FOR EACH ROW EXECUTE FUNCTION dp1_feed_set_updated_at();

CREATE TABLE IF NOT EXISTS registry_publisher_channels (
    id UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    publisher_id UUID NOT NULL REFERENCES registry_publishers (id) ON DELETE CASCADE,
    channel_url TEXT NOT NULL,
    position INT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT registry_publisher_channels_publisher_position_unique UNIQUE (publisher_id, "position")
);

CREATE INDEX IF NOT EXISTS idx_registry_publisher_channels_publisher
    ON registry_publisher_channels (publisher_id, "position");
