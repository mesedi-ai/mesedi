-- Migration 058: checkpoint chain persistence.
--
-- Postgres twin of migrations/058_checkpoint_chain.sql. Read that file
-- for the reasoning; the semantics are identical. Dialect differences:
-- TIMESTAMPTZ rather than TIMESTAMP, and BIGINT rather than INTEGER for
-- the sequence and counters, because Postgres INTEGER is 32-bit and
-- these are int64 in Go.
--
-- The one column that looks redundant and is not: `position`. The
-- interval's Merkle root depends on leaf ORDER, and a SELECT without an
-- explicit ordering column returns rows in whatever order the engine
-- finds convenient — which can differ between SQLite and Postgres, and
-- between two runs on the same engine after a VACUUM. Reconstructing a
-- tree from an unspecified order computes a different root than the one
-- anchored, and a verifier reports TAMPERING on data nobody touched.
-- The cross-dialect case is not hypothetical: this codebase runs both.
--
-- This file existing is not optional. readiness.SchemaStatus counts
-- embedded migrations per dialect, and shipping a migration to one
-- directory only is what caused the migration-056 production outage.
--
-- Reversal: DROP both tables.

CREATE TABLE IF NOT EXISTS checkpoints (
    seq                   BIGINT PRIMARY KEY,
    format                TEXT   NOT NULL,
    prev_checkpoint_hash  TEXT   NOT NULL,
    prev_log_entry_id     TEXT   NOT NULL DEFAULT '',
    interval_start        TIMESTAMPTZ NOT NULL,
    interval_end          TIMESTAMPTZ NOT NULL,
    tenant_leaf_count     INTEGER NOT NULL,
    -- Empty string for an interval with no leaves, NOT the empty-tree
    -- root, which is a real 64-hex digest and would be
    -- indistinguishable from a genuine root over missing data.
    merkle_root           TEXT   NOT NULL,
    cumulative_count      BIGINT NOT NULL,
    created_at_unattested TIMESTAMPTZ NOT NULL,
    hash                  TEXT   NOT NULL UNIQUE,

    anchored_at           TIMESTAMPTZ,
    log_entry_id          TEXT   NOT NULL DEFAULT '',
    ledger_backend        TEXT   NOT NULL DEFAULT ''
);

CREATE INDEX IF NOT EXISTS idx_checkpoints_unanchored
    ON checkpoints(seq) WHERE anchored_at IS NULL;

CREATE TABLE IF NOT EXISTS checkpoint_tenant_leaves (
    checkpoint_seq   BIGINT NOT NULL REFERENCES checkpoints(seq) ON DELETE CASCADE,
    project_id       TEXT   NOT NULL,
    position         INTEGER NOT NULL,
    interval_root    TEXT   NOT NULL,
    execution_count  INTEGER NOT NULL,
    cumulative_count BIGINT NOT NULL,
    prev_leaf_hash   TEXT   NOT NULL,
    leaf_hash        TEXT   NOT NULL,
    PRIMARY KEY (checkpoint_seq, project_id)
);

CREATE INDEX IF NOT EXISTS idx_cp_leaves_project
    ON checkpoint_tenant_leaves(project_id, checkpoint_seq);

INSERT INTO schema_migrations (version) VALUES (58);
