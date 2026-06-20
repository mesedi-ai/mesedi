-- 039_execution_failure_group_membership.sql
--
-- Decouples executions from the "one execution -> one failure group"
-- schema invariant established in 002_failure_groups.sql. The
-- original model used a single failure_group_id column on the
-- executions row. That was fine when one detector classified each
-- execution. As the detector chain grew, multiple specific detectors
-- started firing on the same execution -- and the first one to call
-- groupExecutionInternal claimed the failure_group_id slot, after
-- which every later detector silently no-op'd via the idempotency
-- short-circuit.
--
-- Concrete consequence the integration suite surfaced (backend/test/
-- integration/test_detectors.py::test_identical_call_loop): an agent
-- that repeats the same short prompt 3+ times fires BOTH
-- token_waste AND identical_call_loop. Token_waste runs first in the
-- detector chain and claims failure_group_id. Identical_call_loop
-- runs later, sees failure_group_id is set, and silently drops on
-- the floor. The customer never sees identical_call_loop on their
-- dashboard despite marketing claiming it as a tracked failure
-- class. Same class of bug as the time_budget greedy-claim already
-- fixed.
--
-- This migration introduces an N:M link table that lets every
-- detector record its classification independently. The existing
-- executions.failure_group_id column stays as the denormalized
-- "primary" classification (first detector wins for the primary
-- slot) so existing queries keep working unchanged. New code can
-- JOIN through execution_failure_groups to surface every
-- classification a particular execution received.

CREATE TABLE execution_failure_groups (
    execution_id  TEXT NOT NULL,
    group_id      TEXT NOT NULL,
    -- 1 for the first classification recorded for this execution
    -- (matches the value still held in executions.failure_group_id).
    -- 0 for subsequent classifications. The split lets the dashboard
    -- keep showing a single "primary" failure_class chip on row
    -- summaries while also surfacing "+N more classifications"
    -- detail on the execution page.
    is_primary    INTEGER NOT NULL DEFAULT 0,
    classified_at TEXT NOT NULL,
    PRIMARY KEY (execution_id, group_id),
    FOREIGN KEY (execution_id) REFERENCES executions(execution_id) ON DELETE CASCADE,
    FOREIGN KEY (group_id) REFERENCES failure_groups(group_id) ON DELETE CASCADE
);

-- "All classifications for this execution" lookup. Used by the new
-- /executions/{id}/classifications endpoint and any dashboard surface
-- that wants to render the full set instead of just the primary.
CREATE INDEX idx_execution_failure_groups_execution
    ON execution_failure_groups (execution_id);

-- "All executions in this group" lookup. Replaces the single-column
-- scan on executions.failure_group_id for the failure-group detail
-- page so it surfaces executions whose SECONDARY classification was
-- this group, not just the ones who had it as primary.
CREATE INDEX idx_execution_failure_groups_group
    ON execution_failure_groups (group_id);

-- Backfill: every existing executions row with a non-null
-- failure_group_id becomes one membership record marked is_primary=1.
-- Pre-existing data retains its single classification without loss
-- and reads through the new link table return the same set the old
-- direct-column scan would have.
INSERT INTO execution_failure_groups (
    execution_id, group_id, is_primary, classified_at
)
SELECT
    execution_id,
    failure_group_id,
    1,
    COALESCE(ended_at, started_at)
FROM executions
WHERE failure_group_id IS NOT NULL;

INSERT OR IGNORE INTO schema_migrations (version) VALUES (39);
