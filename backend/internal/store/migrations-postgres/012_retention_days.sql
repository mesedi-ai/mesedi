-- Migration 012, Postgres-flavored. Mirrors the SQLite version ().

ALTER TABLE projects ADD COLUMN IF NOT EXISTS retention_days INTEGER;

INSERT INTO schema_migrations (version) VALUES (12) ON CONFLICT (version) DO NOTHING;
