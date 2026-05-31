-- Migration 012, Postgres-flavored. Mirrors the SQLite version (#262).

ALTER TABLE projects ADD COLUMN IF NOT EXISTS retention_days INTEGER;

INSERT INTO schema_migrations (version) VALUES (12) ON CONFLICT (version) DO NOTHING;
