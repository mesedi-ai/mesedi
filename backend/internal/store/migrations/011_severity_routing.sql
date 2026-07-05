-- Migration 011: severity routing for webhooks ().
--
-- Two changes:
--   1. project_webhooks gains severity_filter (comma-separated subset
--      of "critical", "warning", "info"). Empty string = "fire on all
--      severities" so existing webhooks keep their current behavior
--      without a backfill step.
--   2. New project_class_severities table holds per-project overrides
--      of the failure-class-to-severity default map. Absent rows fall
--      back to the hardcoded default in internal/severity/defaults.go.
--
-- Why the default map lives in Go code, not a seeded SQL row: the
-- mapping is a product opinion (crashes are critical, drift is info)
-- and we want changes to ride the deploy lifecycle, not a one-shot
-- INSERT that becomes hard to update once customers have written
-- their own overrides on top.
--
-- State machine for project_class_severities:
--   no row exists       -> dispatcher uses Go-side default
--   row with severity   -> dispatcher uses the override
--   DELETE on row       -> dispatcher reverts to Go-side default

ALTER TABLE project_webhooks ADD COLUMN severity_filter TEXT NOT NULL DEFAULT '';

CREATE TABLE project_class_severities (
    project_id     TEXT NOT NULL,
    failure_class  TEXT NOT NULL,
    severity       TEXT NOT NULL,
    updated_at     INTEGER NOT NULL,
    PRIMARY KEY (project_id, failure_class),
    FOREIGN KEY (project_id) REFERENCES projects(project_id) ON DELETE CASCADE
);

CREATE INDEX idx_project_class_severities_project
    ON project_class_severities(project_id);

INSERT INTO schema_migrations (version) VALUES (11);
