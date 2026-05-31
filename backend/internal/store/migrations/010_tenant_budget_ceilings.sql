-- Migration 010: tenant_budget_ceilings table.
--
-- Enterprise-tier capability (#252): a customer sets a monthly
-- spend ceiling for their tenant (v0.1 tenant = owner_user_id).
-- The backend's scheduler evaluates current-month burn against
-- the ceiling every 5 minutes; on breach it fires email + webhook
-- notifications (and, in v1.1, automatically halts all active
-- executions across the tenant's projects).
--
-- State machine for a single ceiling row:
--   created_at        set on insert. Never updated.
--   updated_at        bumped on any column write.
--   last_evaluated_at set by the scheduler each time it checks burn
--                     against ceiling. Lets the dashboard show "last
--                     checked X seconds ago" so the customer knows
--                     the evaluator is live.
--   breached_at       set when burn first crosses ceiling. Reset to
--                     NULL at start of each calendar month (the
--                     scheduler does the reset when month rolls over).
--                     Used to dedupe notifications: we send email +
--                     webhook ONCE per breach, not every 5 minutes
--                     for the rest of the month.
--   breach_action     "warn" (notify only) or "halt" (notify + auto-
--                     halt all active executions). v0.1 only emits
--                     notifications regardless of value; v1.1 wires
--                     in the halt fan-out.
--
-- One row per tenant. owner_user_id is the unique key because the
-- v0.1 tenant model is single-user; when real multi-seat
-- organizations arrive, this becomes organization_id and the column
-- type stays TEXT.

CREATE TABLE tenant_budget_ceilings (
    owner_user_id      TEXT PRIMARY KEY,
    monthly_ceiling_usd REAL NOT NULL,
    breach_action      TEXT NOT NULL DEFAULT 'warn',
    notify_email       TEXT,
    notify_webhook_url TEXT,
    created_at         INTEGER NOT NULL,
    updated_at         INTEGER NOT NULL,
    last_evaluated_at  INTEGER,
    breached_at        INTEGER
);

CREATE INDEX idx_tenant_budget_ceilings_evaluated
    ON tenant_budget_ceilings(last_evaluated_at);

INSERT INTO schema_migrations (version) VALUES (10);
