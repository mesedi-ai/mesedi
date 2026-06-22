-- 043_project_cost_velocity_threshold_usd.sql (Postgres twin)
-- See migrations/043_project_cost_velocity_threshold_usd.sql for the
-- rationale.

ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS cost_velocity_threshold_usd REAL NOT NULL DEFAULT 1.00;

INSERT INTO schema_migrations (version) VALUES (43) ON CONFLICT DO NOTHING;
