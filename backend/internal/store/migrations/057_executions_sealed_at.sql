-- Migration 057: add sealed_at to executions.
--
-- Background: the checkpoint chain anchors a Merkle digest over an
-- execution's events into a public transparency log, hourly. To do
-- that it must decide WHICH INTERVAL each execution belongs to, and
-- that decision has to be stable forever.
--
-- The obvious candidates both fail:
--
--   started_at — an execution beginning at 12:59 would land in the
--     12:00-13:00 checkpoint while it is still running. We would
--     anchor a digest over a PARTIAL event record; the remaining
--     events then arrive, the root changes, and a verifier comparing
--     it to the anchored value sees TAMPERING. A false tamper alarm
--     is worse than no alarm: an auditor who watches the chain cry
--     wolf stops believing it when it is right.
--
--   ended_at — better, but it is a MUTABLE column, updated by three
--     separate paths in this codebase. If interval membership were
--     computed from it, a later correction to ended_at would silently
--     move an already-anchored execution into a different interval
--     and break the chain retroactively.
--
-- sealed_at is written ONCE, by a background pass, when an execution
-- is eligible: it has ended and then settled for a grace period so
-- stragglers have landed, or it has exceeded a timeout and is sealed
-- as-is with whatever status it carries. It is never recomputed. A
-- chain can only anchor facts that do not change, and this column
-- exists to be that fact.
--
-- Executions that never end matter as much as the rest. Left
-- unsealed forever they would be an omission nobody could see, which
-- is precisely the attack the whole chain exists to expose. The
-- timeout gives them a defined fate instead.
--
-- No backfill. Every execution that predates this migration stays
-- unsealed and outside the chain, deliberately: the chain starts on a
-- stated date, and a checkpoint claiming to cover a period before the
-- mechanism existed would be a claim nobody could check.
--
-- Reversal: DROP COLUMN sealed_at and the two indexes. Safe; nothing
-- else references the column.

ALTER TABLE executions ADD COLUMN sealed_at TIMESTAMP;

-- Serves the per-interval leaf query: all executions sealed in
-- [from, to), grouped by project. sealed_at leads because the range
-- predicate is on it.
CREATE INDEX IF NOT EXISTS idx_executions_sealed_at
    ON executions(sealed_at, project_id);

-- Serves the seal pass. PARTIAL on purpose: unsealed rows are a small
-- recent working set against a table that grows without bound, so a
-- plain index would degrade toward a scan as history accumulates,
-- while this one stays proportional to the work actually outstanding.
CREATE INDEX IF NOT EXISTS idx_executions_unsealed
    ON executions(ended_at) WHERE sealed_at IS NULL;

INSERT INTO schema_migrations (version) VALUES (57);
