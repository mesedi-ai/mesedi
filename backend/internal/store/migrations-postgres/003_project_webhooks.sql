-- 003_project_webhooks.sql, Postgres-flavored.
--
-- Reconstructed from the live SQLite schema (the original .sql file was
-- deleted from the repo before this Postgres migration was authored;
-- the dev/prod SQLite DBs still carry the table because it was applied
-- in the past). The table holds per-project webhook configurations for
-- the failure-class escalation dispatcher (task #83).
--
-- Differences from SQLite original:
--   * enabled INTEGER NOT NULL DEFAULT 1 -> BOOLEAN NOT NULL DEFAULT TRUE
--     Postgres has a real boolean type; encoding/json round-trips
--     bool <-> SQL boolean cleanly via database/sql.
--   * created_at TEXT NOT NULL -> TIMESTAMPTZ NOT NULL DEFAULT NOW()
--     The original stored timestamps as RFC3339 text; Postgres uses
--     timestamptz natively. Migrating data preserves the values
--     via time.Parse in the data-migration tool.

CREATE TABLE IF NOT EXISTS project_webhooks (
    webhook_id        TEXT PRIMARY KEY,
    project_id        TEXT NOT NULL,
    name              TEXT NOT NULL DEFAULT '',
    url               TEXT NOT NULL,
    secret            TEXT NOT NULL,
    enabled_classes   TEXT,
    enabled           BOOLEAN NOT NULL DEFAULT TRUE,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (project_id) REFERENCES projects(project_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_project_webhooks_project
    ON project_webhooks (project_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_project_webhooks_enabled
    ON project_webhooks (project_id)
    WHERE enabled = TRUE;

INSERT INTO schema_migrations (version) VALUES (3) ON CONFLICT (version) DO NOTHING;
