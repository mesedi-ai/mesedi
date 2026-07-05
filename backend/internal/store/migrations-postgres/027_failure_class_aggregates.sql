-- Migration 027: failure_class_aggregates table ().
--
-- Postgres twin of migrations/027_failure_class_aggregates.sql.
-- See SQLite file for the design rationale.

CREATE TABLE failure_class_aggregates (
    period_year_month TEXT NOT NULL,
    failure_class TEXT NOT NULL,
    distinct_tenants_count BIGINT NOT NULL DEFAULT 0,
    distinct_projects_count BIGINT NOT NULL DEFAULT 0,
    failure_groups_count BIGINT NOT NULL DEFAULT 0,
    affected_executions_count BIGINT NOT NULL DEFAULT 0,
    aggregated_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (period_year_month, failure_class)
);

CREATE INDEX idx_failure_class_aggregates_period
    ON failure_class_aggregates (period_year_month DESC);

INSERT INTO schema_migrations (version) VALUES (27);
