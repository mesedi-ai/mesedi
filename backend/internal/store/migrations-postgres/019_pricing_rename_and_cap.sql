-- Migration 019: pricing alignment for the 4-tier pricing rewrite.
-- Postgres twin of migrations/019_pricing_rename_and_cap.sql; same shape.

UPDATE projects SET tier = 'team' WHERE tier = 'pro';

ALTER TABLE projects ADD COLUMN IF NOT EXISTS billing_cap_usd DOUBLE PRECISION NOT NULL DEFAULT 200;
