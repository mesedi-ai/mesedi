-- Migration 057: add sealed_at to executions.
--
-- Postgres twin of migrations/057_executions_sealed_at.sql. See that
-- file for the full reasoning; semantics are identical and both
-- dialects accept the same ALTER TABLE and partial-index syntax.
--
-- Short version of why the column exists at all: the checkpoint chain
-- must decide which hourly interval each execution belongs to, and
-- that decision must never change afterwards. started_at would anchor
-- digests over still-running executions, so the root would change
-- after anchoring and look like tampering. ended_at is mutable —
-- three separate paths update it — so a later correction would move an
-- already-anchored execution between intervals. sealed_at is written
-- once by a background pass and never recomputed, which is the only
-- kind of fact a chain can anchor.
--
-- This file existing at all is the point of the exercise: readiness
-- .SchemaStatus counts embedded migrations PER DIALECT, so shipping
-- 057 to only one directory would report the other backend as behind.
-- Applying a change to one store and not the other is what caused the
-- migration-056 production outage.
--
-- TIMESTAMPTZ rather than SQLite's TIMESTAMP, matching the dialect
-- convention already used throughout this directory.
--
-- No backfill. Executions predating this migration stay unsealed and
-- outside the chain, because a checkpoint claiming to cover a period
-- before the mechanism existed is a claim nobody can check.

ALTER TABLE executions ADD COLUMN sealed_at TIMESTAMPTZ;

CREATE INDEX IF NOT EXISTS idx_executions_sealed_at
    ON executions(sealed_at, project_id);

CREATE INDEX IF NOT EXISTS idx_executions_unsealed
    ON executions(ended_at) WHERE sealed_at IS NULL;

INSERT INTO schema_migrations (version) VALUES (57);
