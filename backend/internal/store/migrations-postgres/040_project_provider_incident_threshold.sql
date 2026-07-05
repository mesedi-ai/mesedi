-- 040_project_provider_incident_threshold.sql (Postgres twin)
-- See migrations/040_project_provider_incident_threshold.sql for
-- the design rationale.

ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS provider_incident_min_tenants INTEGER NOT NULL DEFAULT 2;

INSERT INTO schema_migrations (version) VALUES (40) ON CONFLICT DO NOTHING;
