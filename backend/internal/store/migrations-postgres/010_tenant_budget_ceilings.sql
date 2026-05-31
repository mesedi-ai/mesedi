-- Migration 010, Postgres-flavored. Mirrors the SQLite version.
-- BIGINT for epoch-millis timestamps (same convention as 009),
-- DOUBLE PRECISION for the ceiling amount.

CREATE TABLE IF NOT EXISTS tenant_budget_ceilings (
    owner_user_id       TEXT PRIMARY KEY,
    monthly_ceiling_usd DOUBLE PRECISION NOT NULL,
    breach_action       TEXT NOT NULL DEFAULT 'warn',
    notify_email        TEXT,
    notify_webhook_url  TEXT,
    created_at          BIGINT NOT NULL,
    updated_at          BIGINT NOT NULL,
    last_evaluated_at   BIGINT,
    breached_at         BIGINT
);

CREATE INDEX IF NOT EXISTS idx_tenant_budget_ceilings_evaluated
    ON tenant_budget_ceilings(last_evaluated_at);

INSERT INTO schema_migrations (version) VALUES (10) ON CONFLICT (version) DO NOTHING;
