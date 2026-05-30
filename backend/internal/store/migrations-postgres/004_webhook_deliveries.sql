-- 004_webhook_deliveries.sql, Postgres-flavored.

CREATE TABLE IF NOT EXISTS webhook_deliveries (
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
    duration_ms     BIGINT,
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    FOREIGN KEY (webhook_id) REFERENCES project_webhooks(webhook_id) ON DELETE CASCADE,
    FOREIGN KEY (project_id) REFERENCES projects(project_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_webhook_recency
    ON webhook_deliveries (webhook_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_project_recency
    ON webhook_deliveries (project_id, created_at DESC);

INSERT INTO schema_migrations (version) VALUES (4) ON CONFLICT (version) DO NOTHING;
