-- Migration 030: sessions table (dashboard auth migration).
--
-- See migrations/030_sessions.sql for the design rationale.
-- This file is the Postgres mirror.

CREATE TABLE sessions (
    token_hash    TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL,
    project_id    TEXT NOT NULL REFERENCES projects(project_id) ON DELETE CASCADE,
    created_at    TIMESTAMPTZ NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL,
    last_used_at  TIMESTAMPTZ NOT NULL,
    user_agent    TEXT NOT NULL DEFAULT '',
    ip_address    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_sessions_user_id    ON sessions (user_id);
CREATE INDEX idx_sessions_expires_at ON sessions (expires_at);

INSERT INTO schema_migrations (version) VALUES (30);
