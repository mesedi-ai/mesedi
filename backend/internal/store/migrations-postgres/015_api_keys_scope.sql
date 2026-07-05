-- Migration 015: api_keys.scope + api_keys.expires_at for admin-scope
-- keys + time-boxed credentials.
-- See migrations/015_api_keys_scope.sql for full rationale.

ALTER TABLE api_keys ADD COLUMN scope      TEXT NOT NULL DEFAULT 'customer';
ALTER TABLE api_keys ADD COLUMN expires_at TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_api_keys_scope ON api_keys(scope);

INSERT INTO schema_migrations (version) VALUES (15);
