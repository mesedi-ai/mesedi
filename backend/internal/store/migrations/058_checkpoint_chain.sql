-- Migration 058: checkpoint chain persistence.
--
-- The chain has to survive a restart. To extend itself it needs its own
-- previous checkpoint hash AND the transparency-log entry that
-- checkpoint was anchored in, because each checkpoint names its
-- predecessor's log entry — that is what stitches the chain to a log
-- Mesedi does not control, rather than only to itself.
--
-- TWO TABLES, AND ONE COLUMN THAT LOOKS REDUNDANT AND IS NOT
--
-- checkpoints holds every field the checkpoint hash covers, plus the
-- anchoring state that only arrives after submission.
--
-- checkpoint_tenant_leaves holds every field the leaf hash covers, plus
-- `position`. That column is the one worth defending. The interval's
-- Merkle root depends on the ORDER of its leaves, and a SELECT without
-- an explicit ordering column returns rows in whatever order the engine
-- finds convenient — which can differ between SQLite and Postgres, and
-- between two runs on the same engine after a vacuum. Reconstructing
-- the tree from an unspecified order would compute a different root
-- than the one anchored, and a verifier would report TAMPERING on data
-- nobody touched.
--
-- WHY THE HASH IS STORED RATHER THAN ALWAYS RECOMPUTED
--
-- Both hashes are stored, but nothing trusts them: the verifier
-- recomputes and compares. They are stored so that a mismatch between
-- the stored value and the recomputed one is itself detectable — which
-- is how you find out that a row was edited underneath you, as opposed
-- to finding out only that the chain no longer verifies.
--
-- ANCHORING STATE IS NULLABLE ON PURPOSE
--
-- A checkpoint exists before it is anchored. The gap between building
-- one and getting a log entry back is exactly where a crash can land,
-- and the design's answer is to anchor late rather than abandon the
-- interval, so an un-anchored checkpoint has to be a representable
-- state that can be resumed rather than an error.
--
-- Reversal: DROP both tables. Nothing else references them.

CREATE TABLE IF NOT EXISTS checkpoints (
    seq                   INTEGER PRIMARY KEY,
    format                TEXT    NOT NULL,
    prev_checkpoint_hash  TEXT    NOT NULL,
    prev_log_entry_id     TEXT    NOT NULL DEFAULT '',
    interval_start        TIMESTAMP NOT NULL,
    interval_end          TIMESTAMP NOT NULL,
    tenant_leaf_count     INTEGER NOT NULL,
    -- Empty string for an interval with no leaves. NOT the empty-tree
    -- root: that value is a real 64-hex digest and would be
    -- indistinguishable from a genuine root over missing data.
    merkle_root           TEXT    NOT NULL,
    cumulative_count      INTEGER NOT NULL,
    created_at_unattested TIMESTAMP NOT NULL,
    hash                  TEXT    NOT NULL UNIQUE,

    -- Anchoring state, written after the digest reaches the log.
    anchored_at           TIMESTAMP,
    log_entry_id          TEXT    NOT NULL DEFAULT '',
    ledger_backend        TEXT    NOT NULL DEFAULT ''
);

-- Finding the un-anchored tail on resume after a crash.
CREATE INDEX IF NOT EXISTS idx_checkpoints_unanchored
    ON checkpoints(seq) WHERE anchored_at IS NULL;

CREATE TABLE IF NOT EXISTS checkpoint_tenant_leaves (
    checkpoint_seq   INTEGER NOT NULL REFERENCES checkpoints(seq) ON DELETE CASCADE,
    project_id       TEXT    NOT NULL,
    -- Explicit leaf order. See the header: the root depends on it and
    -- row order from a SELECT is not a contract.
    position         INTEGER NOT NULL,
    interval_root    TEXT    NOT NULL,
    execution_count  INTEGER NOT NULL,
    cumulative_count INTEGER NOT NULL,
    prev_leaf_hash   TEXT    NOT NULL,
    leaf_hash        TEXT    NOT NULL,
    PRIMARY KEY (checkpoint_seq, project_id)
);

-- Walking one project's sub-chain: its leaves in interval order. This
-- is what makes a tenant dropped from one interval's tree detectable,
-- because the next leaf names a predecessor that appears nowhere.
CREATE INDEX IF NOT EXISTS idx_cp_leaves_project
    ON checkpoint_tenant_leaves(project_id, checkpoint_seq);

INSERT INTO schema_migrations (version) VALUES (58);
