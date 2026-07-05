-- 033: Email-verification tokens (pre-launch). Postgres twin of
-- the SQLite migration; see that file for the rationale.

CREATE TABLE IF NOT EXISTS email_verification_tokens (
    token       TEXT PRIMARY KEY,
    email       TEXT NOT NULL,
    project_id  TEXT NOT NULL REFERENCES projects(project_id) ON DELETE CASCADE,
    created_at  TIMESTAMP NOT NULL,
    expires_at  TIMESTAMP NOT NULL,
    used_at     TIMESTAMP
);

CREATE INDEX IF NOT EXISTS idx_email_verification_tokens_email
    ON email_verification_tokens (email);
CREATE INDEX IF NOT EXISTS idx_email_verification_tokens_expires
    ON email_verification_tokens (expires_at);

INSERT INTO schema_migrations (version) VALUES (33);
