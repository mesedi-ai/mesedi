-- Migration 035: api_key_id column on executions ().
--
-- See migrations/035_executions_api_key_id.sql for the design rationale.
-- This file is the Postgres mirror.

ALTER TABLE executions ADD COLUMN api_key_id TEXT;

CREATE INDEX idx_executions_api_key_started
    ON executions (api_key_id, started_at DESC)
    WHERE api_key_id IS NOT NULL;

INSERT INTO schema_migrations (version) VALUES (35);
