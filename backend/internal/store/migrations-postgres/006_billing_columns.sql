-- Migration 006, Postgres-flavored.
--
-- Original SQLite stored current_period_start/end as INTEGER epoch.
-- Postgres uses BIGINT for the same storage semantics, kept identical
-- so the data-migration tool can copy values byte-for-byte. The Go
-- code already treats them as int64 milliseconds (see store.Project).

ALTER TABLE projects ADD COLUMN IF NOT EXISTS tier TEXT NOT NULL DEFAULT 'hobby';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS stripe_customer_id TEXT;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS stripe_subscription_id TEXT;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS current_period_start BIGINT;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS current_period_end BIGINT;
ALTER TABLE projects ADD COLUMN IF NOT EXISTS executions_this_period BIGINT NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_projects_stripe_customer
    ON projects(stripe_customer_id)
    WHERE stripe_customer_id IS NOT NULL;

INSERT INTO schema_migrations (version) VALUES (6) ON CONFLICT (version) DO NOTHING;
