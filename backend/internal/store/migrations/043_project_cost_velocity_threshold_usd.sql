-- 043_project_cost_velocity_threshold_usd.sql
--
-- Per-project cost_velocity detector threshold in USD. Was hardcoded
-- at $0.001 in sqlite.go (an artificially low v0.0.1 demo value that
-- self-confessed in code as "production would either raise this OR
-- move to a baseline-relative detector"). The "Phase 5+" follow-up
-- never shipped and the hardcoded floor remained, so every real
-- agent execution tripped the detector and destroyed the signal.
--
-- This column lets each project tune the threshold. The default is
-- $1.00 (raised from the broken $0.001) — typical real-world LLM call
-- costs sit in the $0.001 – $0.10 range, so $1.00 captures "this
-- single execution was unusually expensive" without flooding on
-- every routine call.
--
-- IMPORTANT: This migration intentionally CHANGES detector behavior
-- on apply. The historical default ($0.001) was broken; preserving
-- it would have continued the bug. Existing projects that explicitly
-- want the old loud-alarm-on-everything behavior can set their
-- threshold to $0.01 (or lower) via PUT /me/cost-velocity-config.
--
-- Server-side bounds (enforced in handler):
--   floor   $0.01    (prevents fires-on-every-execution storage abuse)
--   ceiling $10,000  (overflow / typo safety)
--
-- NOT tier-capped: cost_velocity threshold is the customer's alarm
-- sensitivity, not a Mesedi-side cost vector. Same reasoning as
-- provider_incident_min_tenants — see tier_caps.go.
--
-- Units are USD with 2-decimal precision (REAL in SQLite). Comparison
-- site is the float64 cost computed by the SDK / future backend cost
-- computation.

ALTER TABLE projects
    ADD COLUMN cost_velocity_threshold_usd REAL NOT NULL DEFAULT 1.00;

INSERT OR IGNORE INTO schema_migrations (version) VALUES (43);
