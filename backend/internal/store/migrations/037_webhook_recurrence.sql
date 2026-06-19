-- 037_webhook_recurrence.sql
--
-- Per-webhook recurrence notification policy. Today's dispatcher fires
-- a webhook exactly once per (project, failure_class, signature) — the
-- first time a failure group is created. Subsequent occurrences of the
-- same failure are silently absorbed. Some customers want more
-- visibility into recurrences and some want less; this migration
-- carries the per-webhook policy that lets them choose.
--
-- recurrence_mode values:
--   'off'          — only fire on new failure groups (current behavior,
--                    and the default for every pre-existing webhook so
--                    the migration is a no-op for live customers).
--   'every_event'  — fire on every recurrence with no throttling.
--                    Risky for high-volume failures; the dashboard
--                    warns customers when they pick this. Eventual
--                    burst-cap lives in #250 (post-launch).
--   'throttled'    — fire on first recurrence in each rolling window,
--                    suppress further recurrences until the window
--                    elapses. recurrence_window_seconds picks the
--                    window length.
--
-- recurrence_window_seconds is non-NULL only when recurrence_mode is
-- 'throttled'. Default 3600s (one hour). Common UI choices: 300,
-- 900, 3600, 14400, 86400. SQLite stores no enum; the dispatcher
-- treats any value < 60 as "60s minimum" defensively.
--
-- webhook_recurrence_state records the last-fired timestamp for each
-- (webhook, failure_group) so 'throttled' mode can decide whether
-- the window has elapsed. PK is the composite (webhook_id, group_id);
-- one row per pair, upserted on every fire. CASCADE deletes follow
-- the parent webhook or project, so cleanup is automatic.

ALTER TABLE project_webhooks ADD COLUMN recurrence_mode TEXT NOT NULL DEFAULT 'off';
ALTER TABLE project_webhooks ADD COLUMN recurrence_window_seconds INTEGER;

CREATE TABLE IF NOT EXISTS webhook_recurrence_state (
    webhook_id     TEXT NOT NULL,
    group_id       TEXT NOT NULL,
    last_fired_at  TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (webhook_id, group_id),
    FOREIGN KEY (webhook_id) REFERENCES project_webhooks(webhook_id) ON DELETE CASCADE
);

-- Dispatcher hot-path lookup: "when did this webhook last fire for
-- this failure group?" The PK already covers (webhook_id, group_id)
-- so no additional index is required for the existence/timestamp
-- query.

INSERT OR IGNORE INTO schema_migrations (version) VALUES (37);
