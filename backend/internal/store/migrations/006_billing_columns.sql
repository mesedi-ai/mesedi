-- Migration 006: add billing columns to projects for Stripe integration.
--
-- Adds tier (hobby / pro / enterprise), the Stripe customer and
-- subscription identifiers a project becomes associated with after a
-- successful Checkout, the current billing period bounds (mirrored
-- from the Stripe subscription so the dashboard can render usage
-- without a Stripe round-trip), and a per-period execution counter.
--
-- This slice (#120) is plumbing + UI polish only. The counter is
-- incremented on each POST /executions but no enforcement runs:
--   - Hobby silent-drop at 5K execs/month — deferred to a follow-up
--     slice; until then Hobby usage is observed but not gated.
--   - Pro overage billing ($0.001 per execution past 100K) — also
--     deferred; will be implemented via Stripe metered-usage records
--     posted on each execution past the included quota.
--
-- The partial index on stripe_customer_id supports webhook event
-- lookup: when Stripe POSTs an event, the handler finds the affected
-- project by Stripe customer id without scanning the whole projects
-- table.
ALTER TABLE projects ADD COLUMN tier TEXT NOT NULL DEFAULT 'hobby';
ALTER TABLE projects ADD COLUMN stripe_customer_id TEXT;
ALTER TABLE projects ADD COLUMN stripe_subscription_id TEXT;
ALTER TABLE projects ADD COLUMN current_period_start INTEGER;
ALTER TABLE projects ADD COLUMN current_period_end INTEGER;
ALTER TABLE projects ADD COLUMN executions_this_period INTEGER NOT NULL DEFAULT 0;
CREATE INDEX IF NOT EXISTS idx_projects_stripe_customer
    ON projects(stripe_customer_id)
    WHERE stripe_customer_id IS NOT NULL;
