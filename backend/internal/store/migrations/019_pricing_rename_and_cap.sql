-- Migration 019: pricing alignment for the 4-tier pricing rewrite.
--
-- 1) Rename existing tier='pro' rows to tier='team' so the database
--    speaks the same vocabulary as the marketing/pricing pages. Any
--    in-flight Stripe subscription / customer is preserved; only the
--    string label changes. Idempotent: if tier is already 'team' the
--    UPDATE matches zero rows.
--
-- 2) Add billing_cap_usd column to projects. This is the monthly hard
--    cap on overage spend. When a project's per-period overage cost
--    crosses this number, the ingest path silent-drops new executions
--    with a 402 "billing cap reached" response.
--
--    Default $200 across all projects: that's the documented Hobby
--    default. Team customers can keep $200 or raise it later via the
--    follow-up "configurable cap UI" slice; the column already exists
--    here so we don't need a second migration.
--
-- Overage cost is NOT stored: it's computed on the fly at every
-- read site as max(0, executions_this_period - included) * tier_rate.
-- One source of truth, no drift risk.

UPDATE projects SET tier = 'team' WHERE tier = 'pro';

ALTER TABLE projects ADD COLUMN billing_cap_usd REAL NOT NULL DEFAULT 200;
