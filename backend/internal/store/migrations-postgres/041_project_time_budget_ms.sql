-- 041_project_time_budget_ms.sql (Postgres twin)
-- See migrations/041_project_time_budget_ms.sql for the rationale.

ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS time_budget_ms INTEGER NOT NULL DEFAULT 60000;

INSERT INTO schema_migrations (version) VALUES (41) ON CONFLICT DO NOTHING;
