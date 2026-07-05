-- Migration 038: user_totp + user_backup_codes + pending_2fa_tokens
-- + sessions.passed_2fa for customer-facing 2FA.
--
-- Optional TOTP-based two-factor authentication on the customer-
-- facing /signin flow. Customer enables in /app/settings, scans a
-- QR code into their authenticator app (Google Authenticator,
-- 1Password, Authy, etc.), then provides a 6-digit code on every
-- subsequent sign-in after SSO or magic-link identity verification.
--
-- Threat model: defense in depth. SSO-only customers may already
-- have 2FA at their identity provider (Google, GitHub); enabling it
-- at Mesedi adds a second layer specifically for "attacker gains
-- access to the customer's email or SSO account" scenarios. Magic-
-- link-only customers gain a defense against email account
-- compromise.
--
-- Why separate tables for secrets vs adding columns to sessions:
-- TOTP secrets are per-user (long-lived), sessions are per-browser
-- (short-lived). Different lifecycles, different tables.

-- Per-user TOTP secret. user_id matches api_keys.user_id +
-- organization_members.user_id (it's the customer's email).
-- secret_encrypted holds the AES-256-GCM ciphertext of the TOTP
-- shared secret. Encryption key lives in MESEDI_TOTP_ENCRYPTION_KEY
-- (Fly secret). Without the key the stored secret is useless even
-- if the DB is exfiltrated.
CREATE TABLE IF NOT EXISTS user_totp (
    user_id           TEXT PRIMARY KEY,
    secret_encrypted  BLOB NOT NULL,
    created_at        TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    last_used_at      TEXT
);

-- Per-user backup codes. One-time-use bypass for the TOTP app when
-- the customer loses their phone. Hashed for the same reason
-- sessions and api_keys are hashed: a DB leak must not yield usable
-- codes. The raw codes are only ever shown once at enrollment +
-- once on regenerate; the dashboard MUST instruct customers to
-- save them outside Mesedi.
CREATE TABLE IF NOT EXISTS user_backup_codes (
    code_hash    TEXT PRIMARY KEY,
    user_id      TEXT NOT NULL,
    created_at   TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    used_at      TEXT
);

CREATE INDEX IF NOT EXISTS idx_user_backup_codes_user
    ON user_backup_codes (user_id);

-- Pending 2FA tokens. After /signin verifies the customer's email
-- via SSO or magic link, when 2FA is enabled the backend mints one
-- of these instead of a session and returns it to the dashboard.
-- The customer enters their TOTP code; the dashboard posts
-- (pending_token, code) to /auth/2fa-verify; on success a real
-- session is minted.
--
-- Lifetime: 5 minutes (TTL on expires_at). One-time use (used_at
-- stamped on first successful verify; subsequent attempts on the
-- same token are rejected).
CREATE TABLE IF NOT EXISTS pending_2fa_tokens (
    token_hash    TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL,
    created_at    TEXT NOT NULL DEFAULT CURRENT_TIMESTAMP,
    expires_at    TEXT NOT NULL,
    used_at       TEXT
);

CREATE INDEX IF NOT EXISTS idx_pending_2fa_tokens_expires_at
    ON pending_2fa_tokens (expires_at);

-- Add passed_2fa flag to sessions. Existing sessions default to 0;
-- if a user has no TOTP secret in user_totp, the auth middleware
-- ignores this flag (2FA not enabled for this user). When a user
-- enables 2FA, their current session is upgraded to passed_2fa=1
-- atomically inside the setup-verify handler so they aren't
-- immediately kicked out by their own enrollment.
ALTER TABLE sessions ADD COLUMN passed_2fa INTEGER NOT NULL DEFAULT 0;

INSERT OR IGNORE INTO schema_migrations (version) VALUES (38);
