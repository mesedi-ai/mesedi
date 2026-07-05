-- 033: Email-verification tokens (pre-launch).
--
-- One row per outstanding verification request. The token is a
-- url-safe random string (generated server-side, never recycled) that
-- ships out in the welcome+verify email. Clicking the link POSTs the
-- token back to /api/email-verify/confirm, which:
--   - looks the row up by token
--   - confirms it has not expired and has not been used
--   - inserts the email into verified_emails (method='email_link')
--   - stamps used_at on the token row
--
-- Tokens have a 24-hour TTL by default — long enough for a customer
-- who signed up before bed to click the link the next morning, short
-- enough that a stolen mailbox dump from a year ago can't be replayed.
-- A separate "resend verification" endpoint (rate-limited) lets a user
-- who waited too long get a fresh token without having to re-sign-up.
--
-- The FK to projects cascades on delete: if the customer closes the
-- account before verifying, the orphaned tokens get cleaned up
-- automatically.

CREATE TABLE email_verification_tokens (
    token       TEXT PRIMARY KEY,
    email       TEXT NOT NULL,
    project_id  TEXT NOT NULL REFERENCES projects(project_id) ON DELETE CASCADE,
    created_at  TIMESTAMP NOT NULL,
    expires_at  TIMESTAMP NOT NULL,
    used_at     TIMESTAMP
);

CREATE INDEX idx_email_verification_tokens_email
    ON email_verification_tokens (email);
CREATE INDEX idx_email_verification_tokens_expires
    ON email_verification_tokens (expires_at);

INSERT INTO schema_migrations (version) VALUES (33);
