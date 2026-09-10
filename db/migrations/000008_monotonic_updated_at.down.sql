-- Restore the plain now() updated_at maintainer from 000001. Existing values are left as they are; they
-- remain valid timestamps, only the strict-increase guarantee for future updates is given up.
CREATE OR REPLACE FUNCTION dp1_feed_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
