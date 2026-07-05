-- 041_project_time_budget_ms.sql
--
-- Per-project time_budget detector threshold. Was hardcoded at
-- 60_000 ms in handlers.go (post-v0.0.1 fix). Real customer agents
-- vary by workload: a research agent legitimately runs 5+ minutes
-- and should not page on-call; a chat agent should never exceed
-- 30s. This column lets each project tune the threshold.
--
-- Default value (60_000 ms) matches the historical hardcoded
-- constant so existing projects see no behavior change on this
-- migration alone. Units are milliseconds to match the comparison
-- site (effectiveDurationMs).

ALTER TABLE projects
    ADD COLUMN time_budget_ms INTEGER NOT NULL DEFAULT 60000;

INSERT OR IGNORE INTO schema_migrations (version) VALUES (41);
