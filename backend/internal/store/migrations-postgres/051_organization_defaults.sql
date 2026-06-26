-- 051_organization_defaults.sql (Postgres twin)
-- See migrations/051_organization_defaults.sql for the rationale.

CREATE TABLE IF NOT EXISTS organization_defaults (
    org_id       TEXT NOT NULL,
    default_key  TEXT NOT NULL,
    value_json   TEXT NOT NULL,
    created_at   TIMESTAMPTZ NOT NULL,
    updated_at   TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (org_id, default_key)
);

CREATE INDEX IF NOT EXISTS idx_organization_defaults_org
    ON organization_defaults(org_id);

INSERT INTO schema_migrations (version) VALUES (51) ON CONFLICT DO NOTHING;
