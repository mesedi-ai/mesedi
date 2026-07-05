-- Migration 038 (Postgres twin): user_totp + user_backup_codes +
-- pending_2fa_tokens + sessions.passed_2fa for .
--
-- See the SQLite migration for the full rationale. Postgres-side
-- differences: BYTEA for the encrypted secret blob, TIMESTAMPTZ for
-- the various *_at columns, ON CONFLICT instead of INSERT OR IGNORE
-- on the schema_migrations row.

CREATE TABLE IF NOT EXISTS user_totp (
    user_id           TEXT PRIMARY KEY,
    secret_encrypted  BYTEA NOT NULL,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_used_at      TIMESTAMPTZ
);

CREATE TABLE IF NOT EXISTS user_backup_codes (
    code_hash    TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    used_at      TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_user_backup_codes_user
    ON user_backup_codes (user_id);

CREATE TABLE IF NOT EXISTS pending_2fa_tokens (
    token_hash    TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL,
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at    TIMESTAMPTZ NOT NULL,
    used_at       TIMESTAMPTZ
);

CREATE INDEX IF NOT EXISTS idx_pending_2fa_tokens_expires_at
    ON pending_2fa_tokens (expires_at);

ALTER TABLE sessions ADD COLUMN passed_2fa BOOLEAN NOT NULL DEFAULT FALSE;

INSERT INTO schema_migrations (version) VALUES (38) ON CONFLICT (version) DO NOTHING;
