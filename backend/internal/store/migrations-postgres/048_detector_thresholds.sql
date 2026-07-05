-- 048_detector_thresholds.sql (Postgres twin)
-- See migrations/048_detector_thresholds.sql for the rationale.

CREATE TABLE IF NOT EXISTS detector_thresholds (
    project_id     TEXT NOT NULL,
    detector       TEXT NOT NULL,
    threshold_key  TEXT NOT NULL,
    value_json     TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    PRIMARY KEY (project_id, detector, threshold_key)
);

CREATE INDEX IF NOT EXISTS idx_detector_thresholds_project_detector
    ON detector_thresholds(project_id, detector);

INSERT INTO schema_migrations (version) VALUES (48) ON CONFLICT DO NOTHING;
