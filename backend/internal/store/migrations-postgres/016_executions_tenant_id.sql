-- Migration 016: executions.tenant_id for cost-per-tenant attribution.
-- See migrations/016_executions_tenant_id.sql for full rationale.

ALTER TABLE executions ADD COLUMN tenant_id TEXT;

CREATE INDEX IF NOT EXISTS idx_executions_project_tenant
    ON executions (project_id, tenant_id)
    WHERE tenant_id IS NOT NULL;

INSERT INTO schema_migrations (version) VALUES (16);
