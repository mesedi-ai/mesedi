-- 045_project_patterns.sql (Postgres twin)
-- See migrations/045_project_patterns.sql for the rationale.

CREATE TABLE IF NOT EXISTS project_patterns (
    pattern_id   TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL,
    detector     TEXT NOT NULL,
    pattern      TEXT NOT NULL,
    severity     TEXT NOT NULL DEFAULT 'medium',
    description  TEXT NOT NULL DEFAULT '',
    enabled      BOOLEAN NOT NULL DEFAULT TRUE,
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    match_count  BIGINT NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_project_patterns_project_detector
    ON project_patterns(project_id, detector);

CREATE INDEX IF NOT EXISTS idx_project_patterns_enabled
    ON project_patterns(project_id, detector, enabled);

INSERT INTO schema_migrations (version) VALUES (45) ON CONFLICT DO NOTHING;
