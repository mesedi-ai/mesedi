-- Migration 018: failure_groups AI-assisted root-cause analysis.
-- Postgres twin of migrations/018_failure_groups_ai_analysis.sql.
--
-- See the SQLite version for the full rationale. Only difference:
-- TIMESTAMP WITH TIME ZONE for analyzed_at (Postgres best practice
-- for any timestamp we'll compare to NOW()).

ALTER TABLE failure_groups ADD COLUMN analysis_markdown TEXT;
ALTER TABLE failure_groups ADD COLUMN analyzed_at TIMESTAMP WITH TIME ZONE;
ALTER TABLE failure_groups ADD COLUMN analysis_model TEXT;

INSERT INTO schema_migrations (version) VALUES (18);
