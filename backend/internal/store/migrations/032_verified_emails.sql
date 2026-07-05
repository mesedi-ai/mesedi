-- 032: Email verification (pre-launch).
--
-- Adds a verified_emails table that tracks which addresses have proved
-- they own the inbox. Customer-facing endpoints gate on this so a typo
-- / throwaway / spoofed-address signup cannot reach the dashboard.
--
-- Verification methods:
--   email_link      raw-email signup user clicked the link in their
--                   welcome email
--   sso_google      Google OAuth callback succeeded; the IdP attests
--                   the user owns the email
--   sso_github      GitHub OAuth callback succeeded; ditto
--   grandfathered   pre-existing rows backfilled at migration time so
--                   Robert + every pre-launch test account is not gated
--                   out at the moment this ships
--
-- Schema is intentionally tiny: one row per lowercase-trimmed email
-- across the whole system. Verifying once unlocks every project that
-- email owns, which matches how customers think about identity. When a
-- users table eventually arrives this table folds into it; until then
-- email is the identifier.

CREATE TABLE verified_emails (
    email       TEXT PRIMARY KEY,
    verified_at TIMESTAMP NOT NULL,
    method      TEXT NOT NULL
);

-- Backfill: grandfather every existing project owner so the launch
-- of email verification does not lock anyone out. LOWER + TRIM matches
-- the normalization the signup handler already does on owner_email.
-- The DISTINCT collapses the case where multiple projects share an
-- owner.
INSERT OR IGNORE INTO verified_emails (email, verified_at, method)
SELECT DISTINCT LOWER(TRIM(owner_email)), CURRENT_TIMESTAMP, 'grandfathered'
FROM projects
WHERE owner_email IS NOT NULL AND TRIM(owner_email) != '';

INSERT INTO schema_migrations (version) VALUES (32);
