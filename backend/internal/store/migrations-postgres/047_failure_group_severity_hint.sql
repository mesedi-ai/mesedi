-- 047_failure_group_severity_hint.sql (Postgres twin)
-- See migrations/047_failure_group_severity_hint.sql for the rationale.

ALTER TABLE failure_groups
    ADD COLUMN IF NOT EXISTS severity_hint TEXT NULL;

INSERT INTO schema_migrations (version) VALUES (47) ON CONFLICT DO NOTHING;
