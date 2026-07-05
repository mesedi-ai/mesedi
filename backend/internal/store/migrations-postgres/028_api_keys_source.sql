-- Migration 028: api_keys.source for session-grade keys ().
-- See migrations/028_api_keys_source.sql for full rationale.

ALTER TABLE api_keys ADD COLUMN source TEXT NOT NULL DEFAULT 'manual';

CREATE INDEX idx_api_keys_source ON api_keys(source);

INSERT INTO schema_migrations (version) VALUES (28);
