-- 051_organization_defaults.sql
--
-- Org-level defaults for the 3 per-project threshold sidecars
-- (time_budget_ms, provider_incident_min_tenants,
-- tool_return_value_max_bytes). Closes the cascading-defaults gap
-- (#276.a) that previously forced a 50-project enterprise customer
-- to set every threshold 50 times.
--
-- Resolution chain at execution-close (handlers.go):
--   1. project-level value (existing project_* tables)
--   2. org_id's row in organization_defaults (this migration)
--   3. hardcoded constant (DefaultTimeBudgetMs etc.)
--
-- Normalized: one row per (org_id, default_key) override. Orgs
-- that never set defaults have ZERO rows; the resolver falls
-- through to the hardcoded constant in that case. Storage growth
-- bounded by orgs * (3 defaults), not orgs * total-tunables.
--
-- value_json is the default value JSON-encoded at the API layer:
--   - time_budget_ms              → "60000"
--   - provider_incident_min_tenants → "2"
--   - tool_return_value_max_bytes → "8192"
-- Validators at the API layer enforce the same tier caps used at
-- the project-level — the store only sees values that already
-- passed validation.

CREATE TABLE IF NOT EXISTS organization_defaults (
    org_id       TEXT NOT NULL,
    default_key  TEXT NOT NULL,
    value_json   TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    updated_at   TEXT NOT NULL,
    PRIMARY KEY (org_id, default_key)
);

CREATE INDEX IF NOT EXISTS idx_organization_defaults_org
    ON organization_defaults(org_id);

INSERT OR IGNORE INTO schema_migrations (version) VALUES (51);
