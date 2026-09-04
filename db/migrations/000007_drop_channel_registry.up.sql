-- Drop the curated channel registry.
--
-- The registry was a hand-curated list of publishers and the channel URLs they vouch for, served
-- read-only at GET /api/v1/registry/channels. It is removed entirely: the endpoint, the models, the
-- store methods and now the tables.
--
-- Why it goes rather than staying as a read-only view: nothing could write it any more. The signatures-only
-- change removed the registry's write endpoint, because a curated list has no signed document to authorize
-- a replace against — it is the feed operator's opinion, not a DP-1 resource with an owner. That left a
-- public endpoint serving data no supported path could ever change, which is worse than not having it.
--
-- Dropping is deliberate rather than leaving the tables unreferenced: an orphaned schema invites a future
-- reader to assume the feature still exists, and the repository does not keep transitional artifacts.
--
-- IRREVERSIBLE for the rows. The down migration restores the table definitions so the schema can be
-- stepped back, but it cannot restore curated content: that data existed only here. Snapshot it before
-- applying this if the list is still wanted anywhere.
--
-- Order matters: registry_publisher_channels references registry_publishers.

DROP TABLE IF EXISTS registry_publisher_channels;
DROP TABLE IF EXISTS registry_publishers;
