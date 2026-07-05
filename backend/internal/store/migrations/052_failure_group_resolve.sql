-- 052_failure_group_resolve.sql
--
-- Customer-facing resolve action on failure groups. Adds two
-- nullable columns to failure_groups: resolved_at (when the
-- customer marked it resolved) and resolved_by (which user did
-- it). Default NULL = unresolved, the pre-migration state for
-- every existing row.
--
-- Default list behavior: HandleListFailureGroups hides resolved
-- groups (WHERE resolved_at IS NULL) so the customer's dashboard
-- stays clean. ?include_resolved=true brings them back so the
-- customer can review or unresolve.
--
-- Auto-reopen-on-recurrence is intentionally NOT implemented:
-- a new event arriving for a resolved group's signature still
-- clusters into the same group and updates last_seen, but the
-- resolved_at stays set. The customer chose to mark this fixed;
-- we honor that choice until they explicitly unresolve. Sentry
-- ships the same semantic.
--
-- No backfill needed; both columns default NULL.

ALTER TABLE failure_groups ADD COLUMN resolved_at DATETIME;
ALTER TABLE failure_groups ADD COLUMN resolved_by TEXT;
