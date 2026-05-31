-- Migration 014: api_keys.user_id for per-member API keys (#263 RBAC).
-- See migrations/014_api_keys_user_id.sql for full rationale.

ALTER TABLE api_keys ADD COLUMN user_id TEXT;
CREATE INDEX idx_api_keys_user_id ON api_keys(user_id);

INSERT INTO schema_migrations (version) VALUES (14);
