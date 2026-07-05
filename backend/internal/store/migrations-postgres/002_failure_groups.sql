-- 002_failure_groups.sql, Postgres-flavored.
-- Translated from migrations/002_failure_groups.sql.

CREATE TABLE IF NOT EXISTS failure_groups (
    group_id            TEXT PRIMARY KEY,
    project_id          TEXT NOT NULL,
    failure_class       TEXT NOT NULL,
    signature           TEXT NOT NULL,
    first_seen          TEXT NOT NULL,
    last_seen           TEXT NOT NULL,
    event_count         BIGINT NOT NULL DEFAULT 0,
    affected_executions BIGINT NOT NULL DEFAULT 0,
    cost_wasted_usd     DOUBLE PRECISION,
    sample_execution_id TEXT,
    FOREIGN KEY (project_id) REFERENCES projects(project_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_failure_groups_project_recency
    ON failure_groups (project_id, last_seen DESC);

CREATE UNIQUE INDEX IF NOT EXISTS idx_failure_groups_signature
    ON failure_groups (project_id, failure_class, signature);

ALTER TABLE executions ADD COLUMN IF NOT EXISTS failure_group_id TEXT;

CREATE INDEX IF NOT EXISTS idx_executions_failure_group
    ON executions (failure_group_id)
    WHERE failure_group_id IS NOT NULL;

INSERT INTO schema_migrations (version) VALUES (2) ON CONFLICT (version) DO NOTHING;
