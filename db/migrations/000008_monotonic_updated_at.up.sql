-- Make updated_at a strictly increasing generation on the document tables.
--
-- updated_at is this feed's generation token: a replace or delete is conditional on the updated_at the
-- caller observed (409 if it moved), and a membership-ordered playlist list binds its cursor to the
-- container's updated_at so a channel or group replaced mid-pagination refuses the stale cursor instead
-- of serving an order that matches neither document. Both uses need every write to produce a value that
-- differs from the previous one. Plain now() does not guarantee that: it is the transaction start time,
-- so two replaces whose transactions began in the same microsecond store the same value, and a
-- transaction that started earlier but committed later (waiting on the row lock) stores a value OLDER
-- than the one it overwrote. In either case a token captured between the two writes still equals the
-- stored value after the second, and the check it guards passes when it must fail.
--
-- On UPDATE the trigger now takes the later of now() and the previous value plus one microsecond (the
-- column's resolution), so the value strictly increases across every update of a row regardless of how
-- transactions interleave. INSERT keeps now(). Nothing else changes: the column, its type, the triggers
-- and every reader stay as they are, which is why this is preferred to adding a separate generation
-- column that every table, cursor and concurrency check would then have to carry.
CREATE OR REPLACE FUNCTION dp1_feed_set_updated_at()
RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'UPDATE' AND OLD.updated_at IS NOT NULL AND OLD.updated_at >= now() THEN
        NEW.updated_at := OLD.updated_at + interval '1 microsecond';
    ELSE
        NEW.updated_at := now();
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
