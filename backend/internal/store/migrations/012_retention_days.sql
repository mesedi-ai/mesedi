-- Migration 012: per-project data retention ().
--
-- Adds retention_days column to projects. Semantics:
--   NULL          = indefinite (no pruning)  -- the existing behavior
--                   for every row ; preserved for Enterprise
--                   customers who need full audit history
--   integer > 0   = number of days the backend keeps executions +
--                   events + failure_groups + webhook_deliveries
--                   before deleting. The retention scheduler runs
--                   nightly and deletes rows where started_at /
--                   created_at falls outside the window.
--
-- We DO NOT set a default for existing rows (NULL stays NULL) so
-- nobody's data evaporates without explicit opt-in. New projects
-- get NULL too; the dashboard settings page surfaces the option
-- so customers choose deliberately.
--
-- Why integer days (not seconds, not timestamptz cutoff): days is
-- the unit customers think in for retention windows ("30 days", "1
-- year", "indefinite") and the scheduler resolves it against
-- time.Now() each run. No persistent cutoff timestamp to keep in
-- sync with billing-period rollovers.
--
-- Cascade behavior: project_id is the FK target on executions,
-- events (via execution_id), failure_groups, project_webhooks, and
-- webhook_deliveries. The scheduler only DELETEs from executions;
-- the ON DELETE CASCADE constraints already cleanly remove the
-- downstream rows.

ALTER TABLE projects ADD COLUMN retention_days INTEGER;

INSERT INTO schema_migrations (version) VALUES (12);
