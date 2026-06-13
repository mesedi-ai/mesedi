-- Migration 030: sessions table (#213 dashboard auth migration).
--
-- Replaces the API-key-as-session architectural hack: the dashboard
-- now mints a real HttpOnly cookie at signin, the cookie value maps
-- to a row here, and the auth middleware accepts EITHER the cookie
-- (dashboard path) or a Bearer API key (SDK path). API keys remain
-- the primary credential for programmatic use; sessions are scoped
-- specifically to the customer-facing browser flow.
--
-- Why a fresh table rather than overloading api_keys: API keys live
-- forever by design (SDK customers do not want their credentials
-- rotating without notice). Browser sessions need a different
-- contract: short expiry, sliding window, server-side revocation on
-- logout / member-removal / key-revocation. Different lifecycles get
-- different tables.
--
-- Why a hash, not the raw token. Same rationale as
-- magic_link_tokens (#196 migration 029) and api_keys: a database
-- leak that exposes the sessions table MUST NOT yield usable
-- session cookies. The raw token only ever lives in the customer's
-- browser; we cannot recover it server-side. Lookup: hash the
-- incoming cookie value, query token_hash, compare expires_at.
--
-- Schema:
--   token_hash    PRIMARY KEY. SHA-256 hex of the raw session token
--                 (which itself is "sess_" + 32 random hex chars =
--                 128 bits of entropy, written verbatim into the
--                 HttpOnly cookie).
--   user_id       Owner email (matches api_keys.user_id +
--                 organization_members.user_id). Indexed so we can
--                 cheaply revoke all of a user's sessions when
--                 their key is revoked or they are kicked from the
--                 org (#213 admin-removes-member kicks them out
--                 immediately).
--   project_id    The currently-active project this session is
--                 scoped to. Cascade-deletes alongside the project
--                 so a closed account leaves no orphan sessions.
--                 Multi-project Team customers switch their active
--                 project via POST /me/active-project which updates
--                 this column rather than re-issuing the cookie.
--   created_at    Mint time (UTC).
--   expires_at    Cutoff (UTC). Default 7 days from mint, extended
--                 to now + 7 days on each authenticated request
--                 (sliding window).
--   last_used_at  Updated by the auth middleware on every successful
--                 lookup. Drives the sliding-window refresh and the
--                 "see my sessions" UI in /app/settings (Batch 4).
--   user_agent    User-Agent string at mint time. Captured so the
--                 sessions UI can show "Chrome on macOS" without
--                 having to parse on read.
--   ip_address    Client IP at mint time. Useful for the same UI
--                 plus abuse-flag correlation (#82 timing analysis
--                 already uses IP).
--
-- Performance: the only hot-path query is "fetch by token_hash"
-- (the PRIMARY KEY itself). The indexes on user_id and expires_at
-- cover the two batch-revocation paths (kill all of one user's
-- sessions; sweep expired rows in the retention scheduler).

CREATE TABLE sessions (
    token_hash    TEXT PRIMARY KEY,
    user_id       TEXT NOT NULL,
    project_id    TEXT NOT NULL REFERENCES projects(project_id) ON DELETE CASCADE,
    created_at    TIMESTAMP NOT NULL,
    expires_at    TIMESTAMP NOT NULL,
    last_used_at  TIMESTAMP NOT NULL,
    user_agent    TEXT NOT NULL DEFAULT '',
    ip_address    TEXT NOT NULL DEFAULT ''
);

CREATE INDEX idx_sessions_user_id    ON sessions (user_id);
CREATE INDEX idx_sessions_expires_at ON sessions (expires_at);

INSERT INTO schema_migrations (version) VALUES (30);
