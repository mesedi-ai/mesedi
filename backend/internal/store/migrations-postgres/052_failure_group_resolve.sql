-- 052_failure_group_resolve.sql (Postgres twin of SQLite 052)
--
-- See migrations/052_failure_group_resolve.sql for the rationale.

ALTER TABLE failure_groups ADD COLUMN IF NOT EXISTS resolved_at TIMESTAMPTZ;
ALTER TABLE failure_groups ADD COLUMN IF NOT EXISTS resolved_by TEXT;
