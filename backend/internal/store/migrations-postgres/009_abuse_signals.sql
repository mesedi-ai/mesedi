-- Migration 009, Postgres-flavored. Same partial-index trick that
-- worked in SQLite carries over verbatim; Postgres supports partial
-- indexes natively. detected_at / notified_at / suspended_at /
-- resolved_at are BIGINT epoch millis to match SQLite storage.

CREATE TABLE IF NOT EXISTS abuse_signals (
    signal_id        TEXT PRIMARY KEY,
    project_id       TEXT NOT NULL,
    kind             TEXT NOT NULL,
    severity         TEXT NOT NULL,
    detail           TEXT,
    detected_at      BIGINT NOT NULL,
    notified_at      BIGINT,
    suspended_at     BIGINT,
    resolved_at      BIGINT,
    resolved_by      TEXT,
    resolution_note  TEXT,
    FOREIGN KEY (project_id) REFERENCES projects(project_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_abuse_signals_project ON abuse_signals(project_id);
CREATE INDEX IF NOT EXISTS idx_abuse_signals_unresolved
    ON abuse_signals(detected_at)
    WHERE resolved_at IS NULL;

ALTER TABLE projects ADD COLUMN IF NOT EXISTS suspended_at BIGINT;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS suspension_reason TEXT;

INSERT INTO schema_migrations (version) VALUES (9) ON CONFLICT (version) DO NOTHING;
