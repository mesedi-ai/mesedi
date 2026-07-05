-- Migration 008, Postgres-flavored. Both expiration columns store epoch
-- milliseconds (BIGINT) so the data-migration tool can copy SQLite
-- INTEGER values byte-for-byte.

ALTER TABLE projects ADD COLUMN IF NOT EXISTS granted_executions_expires_at BIGINT;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS tier_expires_at BIGINT;

INSERT INTO schema_migrations (version) VALUES (8) ON CONFLICT (version) DO NOTHING;
