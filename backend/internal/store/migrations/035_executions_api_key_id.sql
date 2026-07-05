-- Migration 035: api_key_id column on executions ().
--
-- Backs the Terms commitment to "share the information we have about
-- the key's recent use" by recording which API key authenticated the
-- request that created each execution. Without this column, the only
-- attribution we have is project_id, which conflates a leaked key's
-- traffic with the customer's other legitimate keys on the same
-- project.
--
-- Scope: executions table only. Events inherit attribution via
-- execution_id; the forensic query (events for key X) is a single
-- join through the existing events.execution_id index. Mirrors the
-- tenant_id pattern from migration 016.
--
-- Soft reference (TEXT, no FK): matches migration 024's actor_key_id
-- pattern on audit_events. The audit trail must survive key
-- revocation, so we record the key's ID as a string rather than a
-- hard foreign key. Revoking a key from api_keys does not
-- cascade-delete its execution history; the forensic report for
-- that key remains queryable forever.
--
-- Backfill: existing rows stay NULL. We never recorded the
-- authenticating key on past executions and cannot reconstruct it.
-- New executions from the next deploy populate the column. The
-- forensic query treats NULL as "pre-attribution, unknown".
--
-- Index: partial, covers the forensic query "all executions for
-- key X in time window T1..T2" and excludes NULL rows so it does
-- not bloat with pre-attribution history.

ALTER TABLE executions ADD COLUMN api_key_id TEXT;

CREATE INDEX idx_executions_api_key_started
    ON executions (api_key_id, started_at DESC)
    WHERE api_key_id IS NOT NULL;

INSERT INTO schema_migrations (version) VALUES (35);
