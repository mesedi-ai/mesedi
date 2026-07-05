-- 049_project_detector_allowlist.sql (Postgres twin)
-- See migrations/049_project_detector_allowlist.sql for the rationale.

CREATE TABLE IF NOT EXISTS project_detector_allowlist (
    allowlist_id   TEXT PRIMARY KEY,
    project_id     TEXT NOT NULL,
    detector       TEXT NOT NULL,
    allowlist_key  TEXT NOT NULL,
    reason         TEXT NOT NULL DEFAULT '',
    created_by     TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    match_count    BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_project_detector_allowlist_project_detector
    ON project_detector_allowlist(project_id, detector);

CREATE INDEX IF NOT EXISTS idx_project_detector_allowlist_lookup
    ON project_detector_allowlist(project_id, detector, allowlist_key);

INSERT INTO schema_migrations (version) VALUES (49) ON CONFLICT DO NOTHING;
