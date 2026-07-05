-- Migration 016: executions.tenant_id for cost-per-tenant attribution.
--
-- SaaS deployments running multi-tenant agents (one Mesedi project
-- serving N customers, each customer represented by a tenant_id in
-- the host application) need cost attribution at the tenant grain,
-- not the project grain. Without this, a project's cost_velocity
-- alert tells the operator that SOMETHING is expensive but not
-- WHICH end-user drove the spend.
--
-- The simplest workable schema is a nullable TEXT column on
-- `executions` and a lookup index. Events inherit their tenant
-- through their parent execution (events.execution_id FK +
-- executions.tenant_id JOIN), so we deliberately do NOT add the
-- column to events as well. Denormalizing would risk drift between
-- the two columns; the JOIN cost is negligible in practice because
-- the existing idx_events_execution_sequence index already speeds
-- per-execution event scans.
--
-- Backfill semantics:
--   - Existing executions get tenant_id IS NULL, which the
--     cost-by-tenant report renders as "(unattributed)". This lets
--     dashboards distinguish "no tenant ever supplied" from "tenant
--     was supplied as empty string."
--   - New executions accept an optional tenant_id field on POST
--     /executions; callers without multi-tenancy can omit it.
--
-- The lookup index is conditional (WHERE tenant_id IS NOT NULL) on
-- SQLite-native syntax so single-tenant projects pay zero index
-- bloat.

ALTER TABLE executions ADD COLUMN tenant_id TEXT;

CREATE INDEX IF NOT EXISTS idx_executions_project_tenant
    ON executions (project_id, tenant_id)
    WHERE tenant_id IS NOT NULL;

INSERT INTO schema_migrations (version) VALUES (16);
