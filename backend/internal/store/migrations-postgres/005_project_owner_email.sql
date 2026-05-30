-- Migration 005, Postgres-flavored.

ALTER TABLE projects ADD COLUMN IF NOT EXISTS owner_email TEXT;

CREATE INDEX IF NOT EXISTS idx_projects_owner_email
    ON projects(owner_email)
    WHERE owner_email IS NOT NULL;

INSERT INTO schema_migrations (version) VALUES (5) ON CONFLICT (version) DO NOTHING;
