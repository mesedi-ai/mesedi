-- Migration 029: magic_link_tokens for commit 2 (magic-link sign-in).
--
-- The dashboard's /login page lets a customer paste their email and
-- request a one-time sign-in link by email. The backend mints a fresh
-- token (32 random bytes, base64url), persists its SHA-256 hash + the
-- email, and Resend ships an email containing the raw token in the
-- click URL. When the customer clicks the link, the dashboard server
-- hits a verify route that:
--
--   1. Looks up the row by token_hash.
--   2. Rejects if expires_at < now or used_at is non-empty.
--   3. Marks used_at to "burn" the token (one click only).
--   4. Calls the backend's /signin endpoint with the verified email,
--      receives a session-grade API key, and writes it into a cookie
--      that the /login page reads back into localStorage.
--
-- Why a hash, not the raw token. Same reason api_keys hashes its
-- keys: a database leak that exposes magic_link_tokens MUST NOT yield
-- a usable sign-in token. The raw token only ever lives in the email
-- + the customer's browser; we cannot recover it server-side.
--
-- Why one row per request (not "upsert by email"). The customer may
-- click an old email AFTER requesting a fresh one. Both tokens should
-- be valid until either expires or is used; the resolver simply
-- accepts whichever matches. Old rows are reaped by the daily
-- retention job (delete WHERE expires_at < now() OR used_at != '').
--
-- Schema:
--   token_id        opaque, "mlt_<unix_nano>", primary key for joins
--                   and for the dashboard's audit trail.
--   token_hash      SHA-256 hex of the raw token. UNIQUE so a chance
--                   collision (cryptographically negligible) cannot
--                   double-resolve.
--   email           lowercased verified email (matches projects.owner_email).
--   created_at      mint time (UTC ISO-8601).
--   expires_at      cutoff (UTC ISO-8601). 15 minutes from mint, matching
--                   industry-standard "click within 15 min" UX.
--   used_at         non-empty after first click; empty = unused.
--   request_ip      client IP at mint time, for abuse-flag correlation
--                   when a single IP requests too many links in a window.

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
