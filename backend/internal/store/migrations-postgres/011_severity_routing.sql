-- Migration 011, Postgres-flavored. Mirrors the SQLite version.

ALTER TABLE project_webhooks ADD COLUMN IF NOT EXISTS severity_filter TEXT NOT NULL DEFAULT '';

CREATE TABLE IF NOT EXISTS project_class_severities (
    project_id     TEXT NOT NULL,
    failure_class  TEXT NOT NULL,
    severity       TEXT NOT NULL,
    updated_at     BIGINT NOT NULL,
    PRIMARY KEY (project_id, failure_class),
    FOREIGN KEY (project_id) REFERENCES projects(project_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_project_class_severities_project
    ON project_class_severities(project_id);

INSERT INTO schema_migrations (version) VALUES (11) ON CONFLICT (version) DO NOTHING;
