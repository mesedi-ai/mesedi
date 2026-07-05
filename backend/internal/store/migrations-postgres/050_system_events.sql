-- 050_system_events.sql (Postgres twin)
-- See migrations/050_system_events.sql for the rationale.

CREATE TABLE IF NOT EXISTS system_events (
    event_id       TEXT PRIMARY KEY,
    project_id     TEXT NOT NULL,
    actor          TEXT NOT NULL,
    action         TEXT NOT NULL,
    target_type    TEXT NOT NULL,
    target_id      TEXT NOT NULL,
    payload_json   TEXT,
    created_at     TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_system_events_project_created
    ON system_events(project_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_system_events_project_action_target
    ON system_events(project_id, action, target_type, target_id);

INSERT INTO schema_migrations (version) VALUES (50) ON CONFLICT DO NOTHING;
