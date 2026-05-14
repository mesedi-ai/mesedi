-- 002_failure_groups.sql
--
-- Phase 3a: crash detection — group identical crashes into failure_groups.
--
-- A `failure_group` is a deduplicated cluster of failures sharing the
-- same signature (e.g., "every execution that hits this NullPointerException
-- in this same code path"). Each crashed execution links to exactly one
-- failure_group via executions.failure_group_id.
--
-- Group identity is deterministic: SHA-256 of (project_id, failure_class,
-- signature), truncated to 16 hex chars and prefixed with "grp-". This
-- means the same crash signature on the same project ALWAYS maps to the
-- same group across runs and across restarts — no UUID coordination
-- needed. The unique index on (project_id, failure_class, signature) is
-- redundant with the deterministic group_id but kept for query
-- performance and as an integrity belt-and-suspenders.

CREATE TABLE failure_groups (
    group_id            TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL,
    failure_class       TEXT NOT NULL,
    signature           TEXT NOT NULL,
    first_seen          TEXT NOT NULL,
    last_seen           TEXT NOT NULL,
    event_count         INTEGER NOT NULL DEFAULT 0,
    affected_executions INTEGER NOT NULL DEFAULT 0,
    cost_wasted_usd     REAL,
    sample_execution_id TEXT,
    FOREIGN KEY (project_id) REFERENCES projects(project_id) ON DELETE CASCADE
);

-- Recency query: "show me this project's failure groups, newest first."
-- Phase 3b dashboard pages will hit this index hard.
CREATE INDEX idx_failure_groups_project_recency
    ON failure_groups (project_id, last_seen DESC);

-- Lookup index for the upsert path: "find this signature for this project".
CREATE UNIQUE INDEX idx_failure_groups_signature
    ON failure_groups (project_id, failure_class, signature);

-- Reverse-join: "list all executions in this failure_group" — needed by
-- the dashboard's group-detail view to show sample executions.
ALTER TABLE executions ADD COLUMN failure_group_id TEXT;

CREATE INDEX idx_executions_failure_group
    ON executions (failure_group_id)
    WHERE failure_group_id IS NOT NULL;

INSERT OR IGNORE INTO schema_migrations (version) VALUES (2);
