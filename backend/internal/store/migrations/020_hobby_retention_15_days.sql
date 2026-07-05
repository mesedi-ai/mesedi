-- Migration 020: align existing Hobby project retention to 15 days
-- (ship).
--
-- Context: tierRetentionCap was bumped from 30 -> 15 days for the
-- Hobby tier to match the updated /pricing card promise and create
-- a clearer upgrade spread (Hobby 15d vs Team 90d vs Enterprise
-- 3650d/indefinite). Before this migration, existing Hobby projects
-- had retention_days = NULL (indefinite, retains forever) or some
-- value between 1 and 30 set explicitly via the dashboard.
--
-- We backfill in two cases:
--
--   1. NULL retention_days on a Hobby project: set to 15. NULL today
--      means the project retains everything forever, which contradicts
--      the published pricing promise of "15-day data retention" on
--      Hobby. The nightly retention scheduler will start pruning data
--      older than 15 days on its next tick.
--
--   2. retention_days > 15 on a Hobby project: clamp to 15. Customer
--      explicitly chose 30 days back when the cap allowed it; we now
--      tighten them to the new ceiling. Same scheduler behavior.
--
-- Team / Enterprise projects are not touched: their tier caps did not
-- change in this slice and migration 019's tier rename already covered
-- the Pro -> Team relabel.
--
-- One-way migration: customers downgraded by this can re-raise their
-- retention up to 15 days via the dashboard settings page. Going back
-- to "indefinite" is not allowed on Hobby; upgrade to Enterprise.

UPDATE projects
SET retention_days = 15
WHERE tier = 'hobby'
  AND (retention_days IS NULL OR retention_days > 15);

INSERT INTO schema_migrations (version) VALUES (20);
