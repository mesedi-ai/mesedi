-- 044_project_cost_velocity_rate.sql (Postgres twin)
-- See migrations/044_project_cost_velocity_rate.sql for the rationale.

ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS cost_velocity_rate_threshold_usd_per_min REAL NOT NULL DEFAULT 5.00;

ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS cost_velocity_rate_window_minutes INTEGER NOT NULL DEFAULT 5;

INSERT INTO schema_migrations (version) VALUES (44) ON CONFLICT DO NOTHING;
