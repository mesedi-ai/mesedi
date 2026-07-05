-- 046_project_patterns_last_matched_at.sql (Postgres twin)
-- See migrations/046_project_patterns_last_matched_at.sql for the rationale.

ALTER TABLE project_patterns
    ADD COLUMN IF NOT EXISTS last_matched_at TIMESTAMPTZ NULL;

INSERT INTO schema_migrations (version) VALUES (46) ON CONFLICT DO NOTHING;
