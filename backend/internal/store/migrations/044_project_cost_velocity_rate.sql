-- 044_project_cost_velocity_rate.sql
--
-- Per-project cost_velocity RATE detector configuration. Adds two
-- columns to the projects table:
--
--   cost_velocity_rate_threshold_usd_per_min REAL
--     The $/minute burn-rate at or above which the rate detector
--     fires. Default 5.00 ($/min) — $300/hr — clearly anomalous for
--     real production workloads without flooding on routine bursts.
--
--   cost_velocity_rate_window_minutes INTEGER
--     The rolling lookback window over which spend is summed and
--     divided to compute the rate. Default 5 minutes — short enough
--     to catch sustained spikes within a deploy cycle, long enough
--     that a single outlier execution doesn't dominate the average.
--
-- Closes cost_velocity.G2 from the audit (marketing-vs-implementation
-- gap: marketing promised $/minute rate-based detection; only
-- per-execution absolute magnitude existed). Pairs with the absolute
-- threshold added in migration 043 — both detectors can fire on the
-- same execution because they answer different questions
-- (magnitude vs sustained rate).
--
-- Server-side bounds (enforced in handler, not the store):
--   threshold floor   $0.10 / min   (prevents fires-on-every-minute
--                                    storage abuse vector)
--   threshold ceiling $10000 / min  (typo / overflow safety)
--   window floor      1 minute      (any shorter is noise)
--   window ceiling    60 minutes    (any longer makes aggregator
--                                    scans pathological)
--
-- NOT tier-capped: same reasoning as cost_velocity absolute and
-- provider_incident_min_tenants — alarm sensitivity is the customer's
-- choice, not a Mesedi-side cost vector. See tier_caps.go for the
-- principle.

ALTER TABLE projects
    ADD COLUMN cost_velocity_rate_threshold_usd_per_min REAL NOT NULL DEFAULT 5.00;

ALTER TABLE projects
    ADD COLUMN cost_velocity_rate_window_minutes INTEGER NOT NULL DEFAULT 5;

INSERT OR IGNORE INTO schema_migrations (version) VALUES (44);
