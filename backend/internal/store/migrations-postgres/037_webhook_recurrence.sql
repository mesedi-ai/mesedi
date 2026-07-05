-- 037_webhook_recurrence.sql (Postgres twin of the SQLite migration)
--
-- Per-webhook recurrence notification policy. See the SQLite
-- migration for the full rationale. The Postgres version uses native
-- TIMESTAMPTZ for last_fired_at and ON CONFLICT for the migration
-- row, matching the convention established by earlier Postgres
-- migrations.
--
-- recurrence_mode values: 'off' | 'every_event' | 'throttled'. Stored
-- as TEXT here too (no CHECK constraint yet; the dispatcher validates
-- on read so unknown values fail open to 'off' rather than blowing
-- up an in-flight delivery).

ALTER TABLE project_webhooks ADD COLUMN recurrence_mode TEXT NOT NULL DEFAULT 'off';
ALTER TABLE project_webhooks ADD COLUMN recurrence_window_seconds INTEGER;

CREATE TABLE IF NOT EXISTS webhook_recurrence_state (
    webhook_id     TEXT NOT NULL,
    group_id       TEXT NOT NULL,
    last_fired_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (webhook_id, group_id),
    FOREIGN KEY (webhook_id) REFERENCES project_webhooks(webhook_id) ON DELETE CASCADE
);

INSERT INTO schema_migrations (version) VALUES (37) ON CONFLICT (version) DO NOTHING;
