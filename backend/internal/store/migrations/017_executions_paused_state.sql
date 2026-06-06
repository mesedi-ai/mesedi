-- Migration 017: execution lifecycle rework for human-in-the-loop pauses.
--
-- Mesedi #18. The existing lifecycle model assumes an execution
-- transitions from `started` directly into one of five terminal
-- statuses (completed / crashed / halted / timeout /
-- validation_failed). That model breaks the moment an agent has to
-- wait on a human: a HITL pause might last seconds, minutes, hours,
-- or days, and the run is neither failed nor done during the wait.
--
-- This migration adds three columns that together describe the
-- "paused" lifecycle slice without disturbing any terminal-state
-- logic that already exists:
--
--   * paused_at        timestamp the execution entered the
--                      currently-active pause cycle. NULL when the
--                      execution is not paused. Cleared on resume.
--
--   * total_paused_ms  accumulated wall-clock milliseconds the
--                      execution has spent in paused state across
--                      every pause/resume cycle. Used by budget
--                      enforcement (#18b) to subtract paused time
--                      from time-budget checks, and by the HITL
--                      SLA detectors (#20 hitl_timeout, #21
--                      hitl_rejection_spike) to compute wait
--                      duration.
--
--   * pause_count      number of times the execution has paused.
--                      Useful telemetry for the hitl_rejection_spike
--                      detector (#21) which fires when a single
--                      execution accumulates an abnormal number of
--                      human-intervention cycles.
--
-- The new execution status value `awaiting_human` is encoded in
-- application-side enum (events.go ExecutionStatus); the DB stores
-- it as a TEXT value in the existing status column, so no schema
-- change is needed to accept it. The column itself remains
-- unconstrained TEXT.
--
-- Backfill semantics:
--   * Existing executions get paused_at=NULL, total_paused_ms=0,
--     pause_count=0. Equivalent to "never paused", which is the
--     correct state for every pre-#18 execution.
--   * Existing executions are not retroactively classified as
--     awaiting_human. Their terminal status is preserved as-is.
--
-- Index rationale: hitl_timeout (#20) needs to find executions that
-- have been paused for longer than the SLA. The partial index keeps
-- the lookup tight by only including currently-paused rows; finished
-- executions and never-paused rows incur zero index cost.

ALTER TABLE executions ADD COLUMN paused_at TIMESTAMP;
ALTER TABLE executions ADD COLUMN total_paused_ms INTEGER NOT NULL DEFAULT 0;
ALTER TABLE executions ADD COLUMN pause_count INTEGER NOT NULL DEFAULT 0;

CREATE INDEX IF NOT EXISTS idx_executions_paused_at
    ON executions (paused_at)
    WHERE paused_at IS NOT NULL;

INSERT INTO schema_migrations (version) VALUES (17);
