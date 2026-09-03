-- No-op by design.
--
-- This migration only inserted rows that were derivable from existing memberships, and nothing here
-- distinguishes a backfilled row from one recorded by a later ingest. Deleting the derivable set would
-- therefore also discard mappings the running feed has since written and depends on, so reversing the
-- backfill is left to 000006's down migration, which drops the table outright.

SELECT 1;
