-- Migration 013, Postgres-flavored. Mirrors the SQLite version ().
-- BIGINT epoch-seconds timestamps to match the rest of the schema.

CREATE TABLE IF NOT EXISTS organizations (
    org_id              TEXT PRIMARY KEY,
    name                TEXT NOT NULL,
    created_by_user_id  TEXT NOT NULL,
    created_at          BIGINT NOT NULL,
    updated_at          BIGINT NOT NULL
);

CREATE TABLE IF NOT EXISTS organization_members (
    org_id            TEXT NOT NULL,
    user_id           TEXT NOT NULL,
    role              TEXT NOT NULL CHECK (role IN ('admin', 'write', 'read')),
    email             TEXT,
    added_by_user_id  TEXT,
    added_at          BIGINT NOT NULL,
    PRIMARY KEY (org_id, user_id),
    FOREIGN KEY (org_id) REFERENCES organizations(org_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_organization_members_user
    ON organization_members(user_id);

CREATE TABLE IF NOT EXISTS organization_invites (
    invite_id            TEXT PRIMARY KEY,
    org_id               TEXT NOT NULL,
    email                TEXT NOT NULL,
    role                 TEXT NOT NULL CHECK (role IN ('admin', 'write', 'read')),
    token                TEXT NOT NULL UNIQUE,
    invited_by_user_id   TEXT NOT NULL,
    created_at           BIGINT NOT NULL,
    expires_at           BIGINT NOT NULL,
    accepted_at          BIGINT,
    accepted_by_user_id  TEXT,
    FOREIGN KEY (org_id) REFERENCES organizations(org_id) ON DELETE CASCADE
);

CREATE INDEX IF NOT EXISTS idx_organization_invites_org
    ON organization_invites(org_id);
CREATE INDEX IF NOT EXISTS idx_organization_invites_token
    ON organization_invites(token);
CREATE INDEX IF NOT EXISTS idx_organization_invites_pending
    ON organization_invites(email)
    WHERE accepted_at IS NULL;

ALTER TABLE projects ADD COLUMN IF NOT EXISTS tenant_id TEXT
    REFERENCES organizations(org_id);
CREATE INDEX IF NOT EXISTS idx_projects_tenant ON projects(tenant_id);

-- Backfill: one org per existing project. owner_user_id becomes the
-- org's admin. Conditional on the project actually having a user
-- attached; projects with NULL/empty owner stay tenant-less
-- (the auth chain already refuses unauthenticated dashboard requests
-- so these orphan rows aren't reachable).
INSERT INTO organizations (org_id, name, created_by_user_id, created_at, updated_at)
SELECT
    'org_' || project_id,
    COALESCE(name, 'Personal'),
    COALESCE(owner_user_id, ''),
    EXTRACT(EPOCH FROM NOW())::BIGINT,
    EXTRACT(EPOCH FROM NOW())::BIGINT
FROM projects
WHERE owner_user_id IS NOT NULL AND owner_user_id != ''
ON CONFLICT (org_id) DO NOTHING;

INSERT INTO organization_members (org_id, user_id, role, email, added_by_user_id, added_at)
SELECT
    'org_' || p.project_id,
    p.owner_user_id,
    'admin',
    p.owner_email,
    NULL,
    EXTRACT(EPOCH FROM NOW())::BIGINT
FROM projects p
WHERE p.owner_user_id IS NOT NULL AND p.owner_user_id != ''
ON CONFLICT (org_id, user_id) DO NOTHING;

UPDATE projects
SET tenant_id = 'org_' || project_id
WHERE (owner_user_id IS NOT NULL AND owner_user_id != '')
  AND tenant_id IS NULL;

INSERT INTO schema_migrations (version) VALUES (13) ON CONFLICT (version) DO NOTHING;
