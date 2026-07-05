-- Migration 053: playbook-signature staleness tracking.
-- Postgres twin of migrations/053_playbook_signature.sql.
--
-- See the SQLite version for the full rationale. Schema identical.

ALTER TABLE failure_groups ADD COLUMN analysis_playbook_signature TEXT;
ALTER TABLE ai_analyses ADD COLUMN playbook_signature TEXT;

INSERT INTO schema_migrations (version) VALUES (53);
