-- Migration 027: failure_class_aggregates table (#212).
--
-- Aggregate counts of failure-class occurrences per month, designed
-- to survive account closure. The whole point of this table is that
-- a customer can close their account and the row stays — the
-- underlying failure_groups + executions cascade-delete, but the
-- counts captured here remain. That lets Mesedi publish monthly
-- LinkedIn trend reports without retaining customer-identifying
-- data past close.
--
-- Privacy posture:
--   - NO tenant_id, project_id, customer email, signatures, or
--     event payloads. Strictly aggregate counts.
--   - distinct_tenants_count is the k-anonymity gate. Reads with
--     k_threshold filter out rows where this count is below k
--     (default k=3). Single-customer signals are suppressed.
--
-- Aggregation cadence:
--   - Background worker re-aggregates the CURRENT month daily so
--     mid-month account closures don't lose data.
--   - Manual admin trigger via POST /admin/failure-class-aggregates/run
--     forces an immediate re-aggregation for any month, useful
--     before drafting a LinkedIn post.
--
-- Schema:
--   period_year_month   "YYYY-MM" UTC, derived from
--                       failure_groups.first_seen.
--   failure_class       e.g. "crashes", "prompt_injection", etc.
--   distinct_tenants_count    DISTINCT tenant_id count
--   distinct_projects_count   DISTINCT project_id count
--   failure_groups_count      COUNT(*) of failure_group rows
--   affected_executions_count SUM(affected_executions)
--   aggregated_at       when this row was last refreshed
--
-- Primary key is (period_year_month, failure_class). The worker uses
-- UPSERT (INSERT OR REPLACE in SQLite, ON CONFLICT in Postgres) to
-- update existing rows when re-aggregating.

CREATE TABLE failure_class_aggregates (
    period_year_month TEXT NOT NULL,
    failure_class TEXT NOT NULL,
    distinct_tenants_count INTEGER NOT NULL DEFAULT 0,
    distinct_projects_count INTEGER NOT NULL DEFAULT 0,
    failure_groups_count INTEGER NOT NULL DEFAULT 0,
    affected_executions_count INTEGER NOT NULL DEFAULT 0,
    aggregated_at TIMESTAMP NOT NULL,
    PRIMARY KEY (period_year_month, failure_class)
);

CREATE INDEX idx_failure_class_aggregates_period
    ON failure_class_aggregates (period_year_month DESC);

INSERT INTO schema_migrations (version) VALUES (27);
