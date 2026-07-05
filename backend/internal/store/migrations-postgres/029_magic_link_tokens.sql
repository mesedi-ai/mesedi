-- Migration 029: magic_link_tokens for commit 2 (magic-link sign-in).
-- See migrations/029_magic_link_tokens.sql for full rationale.

CREATE TABLE magic_link_tokens (
    token_id    TEXT PRIMARY KEY,
    token_hash  TEXT NOT NULL UNIQUE,
    email       TEXT NOT NULL,
    created_at  TEXT NOT NULL,
    expires_at  TEXT NOT NULL,
    used_at     TEXT NOT NULL DEFAULT '',
    request_ip  TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_magic_link_tokens_email      ON magic_link_tokens(email);
CREATE INDEX idx_magic_link_tokens_expires_at ON magic_link_tokens(expires_at);

INSERT INTO schema_migrations (version) VALUES (29);
