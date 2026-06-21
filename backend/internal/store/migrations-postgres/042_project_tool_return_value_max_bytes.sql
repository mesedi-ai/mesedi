-- 042_project_tool_return_value_max_bytes.sql (Postgres twin)
-- See migrations/042_project_tool_return_value_max_bytes.sql for
-- the rationale.

ALTER TABLE projects
    ADD COLUMN IF NOT EXISTS tool_return_value_max_bytes INTEGER NOT NULL DEFAULT 8192;

INSERT INTO schema_migrations (version) VALUES (42) ON CONFLICT DO NOTHING;
