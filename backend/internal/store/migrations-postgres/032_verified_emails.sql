-- 032: Email verification (pre-launch). See sqlite migration for
-- the full rationale; this is the Postgres twin.

CREATE TABLE IF NOT EXISTS verified_emails (
    email       TEXT PRIMARY KEY,
    verified_at TIMESTAMP NOT NULL,
    method      TEXT NOT NULL
);

INSERT INTO verified_emails (email, verified_at, method)
SELECT DISTINCT LOWER(TRIM(owner_email)), CURRENT_TIMESTAMP, 'grandfathered'
FROM projects
WHERE owner_email IS NOT NULL AND TRIM(owner_email) != ''
ON CONFLICT (email) DO NOTHING;

INSERT INTO schema_migrations (version) VALUES (32);
