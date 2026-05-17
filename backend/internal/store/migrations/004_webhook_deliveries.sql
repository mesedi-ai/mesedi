-- 004_webhook_deliveries.sql
--
-- Webhook escalation slice 2 (task #83) — delivery log for every
-- attempted webhook POST. One row per attempt (including retries).
-- The dashboard's webhooks page reads this table to show "your last
-- 50 delivery attempts" so customers can debug a misbehaving receiver
-- without having to scrape the backend logs.
--
-- Status values:
--   pending   — row created, POST not yet attempted (rare; usually
--               only visible during retry backoff window)
--   delivered — receiver returned 2xx
--   failed    — final attempt returned non-2xx or transport error,
--               retries exhausted
--
-- attempt counts up from 1; a single (failure_group, webhook) pair
-- may produce up to 3 attempt rows in the worst case (initial +
-- 2 retries per the dispatcher's backoff policy).
--
-- response_body is truncated to ~2KB on insert to bound storage —
-- a misbehaving receiver returning 1MB of HTML on every error would
-- otherwise balloon the log table.

CREATE TABLE webhook_deliveries (
    delivery_id     TEXT PRIMARY KEY,
    webhook_id      TEXT NOT NULL,
    project_id      TEXT NOT NULL,
    failure_class   TEXT,
    signature       TEXT,
    group_id        TEXT,
    attempt         INTEGER NOT NULL DEFAULT 1,
    status          TEXT NOT NULL,
    http_status     INTEGER,
    error           TEXT,
    response_body   TEXT,
    duration_ms     INTEGER,
    created_at      TEXT NOT NULL,
    FOREIGN KEY (webhook_id) REFERENCES project_webhooks(webhook_id) ON DELETE CASCADE,
    FOREIGN KEY (project_id) REFERENCES projects(project_id) ON DELETE CASCADE
);

-- Per-webhook recency: dashboard's "recent deliveries for this
-- webhook" view.
CREATE INDEX idx_webhook_deliveries_webhook_recency
    ON webhook_deliveries (webhook_id, created_at DESC);

-- Per-project recency: dashboard's project-wide "recent webhook
-- activity" feed.
CREATE INDEX idx_webhook_deliveries_project_recency
    ON webhook_deliveries (project_id, created_at DESC);

INSERT OR IGNORE INTO schema_migrations (version) VALUES (4);
