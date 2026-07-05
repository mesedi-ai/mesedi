-- Migration 017: execution lifecycle rework for human-in-the-loop pauses.
-- Postgres twin of migrations/017_executions_paused_state.sql.
--
-- See the SQLite version for the full rationale. The only differences:
--   * BIGINT instead of SQLite's INTEGER for total_paused_ms (SQLite
--     stores all integers as variable-width affinity but Postgres
--     needs an explicit type; we pick BIGINT because accumulated
--     paused duration on a long-running HITL execution can easily
--     exceed INT4's ~24-day cap).
--   * TIMESTAMP WITH TIME ZONE for paused_at (Postgres best practice
--     for any timestamp we'll compare to NOW()).

ALTER TABLE executions ADD COLUMN paused_at TIMESTAMP WITH TIME ZONE;
ALTER TABLE executions ADD COLUMN total_paused_ms BIGINT NOT NULL DEFAULT 0;
ALTER TABLE executions ADD COLUMN pause_count INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_executions_paused_at
    ON executions (paused_at)
    WHERE paused_at IS NOT NULL;

INSERT INTO schema_migrations (version) VALUES (17);
