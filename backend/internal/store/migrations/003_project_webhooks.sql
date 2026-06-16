-- 003_project_webhooks.sql
--
-- Per-project webhook configurations for the failure-class escalation
-- dispatcher (task #83). Migration 004 (webhook_deliveries) declares
-- a foreign key against project_webhooks(webhook_id), so this MUST run
-- first; any fresh SQLite install that skips this file panics at
-- migration 011 with "no such table: project_webhooks".
--
-- History (2026-06-16 ports back): the original 003 SQLite migration
-- was deleted from the repo before the migrations-postgres/ twin was
-- authored. Development DBs carried the table forward because they
-- had applied 003 long before it was deleted, so the gap stayed
-- invisible until #231's store-test setup tried to spin up a fresh
-- SQLite database and faceplanted on migration 011. Reconstructed
-- here from the live SQLite schema + the Postgres twin's columns,
-- adapted back to SQLite conventions:
--   * BOOLEAN/TRUE -> INTEGER/1 (SQLite has no native boolean)
--   * TIMESTAMPTZ DEFAULT NOW() -> TEXT DEFAULT CURRENT_TIMESTAMP
--   * ON CONFLICT (version) DO NOTHING -> OR IGNORE
--
-- Columns:
--   enabled_classes — JSON-array string of failure_class names this
--     webhook should fire on. NULL means "all classes" (default-on);
--     the dispatcher reads + parses on each match.
--   enabled — soft-disable toggle so customers can park a webhook
--     without losing its config.
--   secret — HMAC signing secret for the X-Mesedi-Signature header
--     on every delivery POST. Per-webhook so a leaked secret only
--     compromises one endpoint.

CREATE TABLE IF NOT EXISTS project_webhooks (
    webhook_id        TEXT PRIMARY KEY,
    project_id        TEXT NOT NULL,
    name              TEXT NOT NULL DEFAULT '',
    url               TEXT NOT NULL,
    secret            TEXT NOT NULL,
    enabled_classes   TEXT,
    enabled           INTEGER NOT NULL DEFAULT 1,
    created_at        TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    FOREIGN KEY (project_id) REFERENCES projects(project_id) ON DELETE CASCADE
);

-- Recency query: "this project's webhooks, newest first" (the
-- dashboard's /app/webhooks page hits this on every load).
CREATE INDEX IF NOT EXISTS idx_project_webhooks_project
    ON project_webhooks (project_id, created_at DESC);

-- Partial index for the dispatcher hot path: list only enabled
-- webhooks for a project. SQLite supports partial indexes since
-- 3.8.0; the WHERE filter matches enabled=1 (SQLite boolean
-- convention).
CREATE INDEX IF NOT EXISTS idx_project_webhooks_enabled
    ON project_webhooks (project_id)
    WHERE enabled = 1;

INSERT OR IGNORE INTO schema_migrations (version) VALUES (3);
