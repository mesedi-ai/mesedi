-- Migration 007, Postgres-flavored.

ALTER TABLE projects ADD COLUMN IF NOT EXISTS granted_executions BIGINT NOT NULL DEFAULT 0;

INSERT INTO schema_migrations (version) VALUES (7) ON CONFLICT (version) DO NOTHING;
