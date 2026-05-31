-- Migration 013: organizations + members + invites + projects.tenant_id (#263).
--
-- v0.1 of the multi-seat tenant model. This is the schema foundation
-- the Enterprise tier copy has promised since #261. Every Mesedi
-- project becomes a member of exactly one organization; users join
-- organizations as members with one of three roles.
--
-- =====================================================================
-- TENANT MODEL EVOLUTION
-- =====================================================================
-- Pre-#263 (today):  "tenant" === projects.owner_user_id. Every
--                    project is its own island; owner_user_id is the
--                    only thing that ties multiple projects together.
-- Post-#263 (after this migration runs):
--                    "tenant" === organizations.org_id. Multiple
--                    users can share one tenant via the
--                    organization_members table. owner_user_id is
--                    preserved as the row's original creator but the
--                    NEW authorization key is (tenant_id, role).
--
-- =====================================================================
-- BACKFILL SAFETY
-- =====================================================================
-- We CANNOT NULL the new projects.tenant_id column for any existing
-- row, because the org-rollup and budget-ceiling endpoints will pivot
-- to scope-by-tenant_id in a follow-up commit. So this migration
-- ALSO performs a backfill in three steps:
--
--   1. CREATE TABLE organizations + organization_members
--   2. ALTER TABLE projects ADD COLUMN tenant_id (nullable for now)
--   3. INSERT one organizations row per existing project. The new
--      org's name defaults to the project's name; the original
--      owner_user_id becomes the org's admin member.
--   4. UPDATE projects SET tenant_id = <new org's id>
--
-- After this migration, every existing project has tenant_id set and
-- one membership row in organization_members pinning the original
-- creator as admin. New signups (handled separately in the signup
-- handler) create their own org at project-create time.
--
-- =====================================================================
-- INVITES TABLE
-- =====================================================================
-- organization_invites is the token-based join flow. Admins create
-- invites with (email, role, expires_at); the invitee follows a link
-- carrying the token, which the public /invites/accept endpoint
-- consumes to insert a fresh organization_members row.

CREATE TABLE organizations (
    org_id         TEXT PRIMARY KEY,
    name           TEXT NOT NULL,
    -- created_by_user_id is the user_id of whoever first instantiated
    -- the org. For backfilled orgs this is projects.owner_user_id; for
    -- new orgs it's the signing-up user. Distinct from members because
    -- the founder can be transferred away later.
    created_by_user_id TEXT NOT NULL,
    created_at     INTEGER NOT NULL,
    updated_at     INTEGER NOT NULL
);

CREATE TABLE organization_members (
    org_id     TEXT NOT NULL,
    user_id    TEXT NOT NULL,
    role       TEXT NOT NULL CHECK (role IN ('admin', 'write', 'read')),
    -- email captured at invite time so the dashboard members list can
    -- render the row even before the user has signed in (during the
    -- "invite pending" window). Refreshed to the user's current email
    -- whenever they next authenticate.
    email      TEXT,
    -- added_by_user_id tracks who invited this person (admin of the
    -- org at the time). NULL for the original auto-backfilled
    -- founder; never NULL for invite-accepted rows.
    added_by_user_id TEXT,
    added_at   INTEGER NOT NULL,
    PRIMARY KEY (org_id, user_id),
    FOREIGN KEY (org_id) REFERENCES organizations(org_id) ON DELETE CASCADE
);

CREATE INDEX idx_organization_members_user ON organization_members(user_id);

CREATE TABLE organization_invites (
    invite_id  TEXT PRIMARY KEY,
    org_id     TEXT NOT NULL,
    email      TEXT NOT NULL,
    role       TEXT NOT NULL CHECK (role IN ('admin', 'write', 'read')),
    -- token is the unguessable random string the invitee receives in
    -- their email. Hashed at-rest? Not in v0.1: invites are short-lived
    -- (default 7-day expiry, see expires_at) and one-shot (accepted_at
    -- is set on first use, after which the token is dead).
    token      TEXT NOT NULL UNIQUE,
    invited_by_user_id TEXT NOT NULL,
    created_at INTEGER NOT NULL,
    expires_at INTEGER NOT NULL,
    accepted_at INTEGER,
    accepted_by_user_id TEXT,
    FOREIGN KEY (org_id) REFERENCES organizations(org_id) ON DELETE CASCADE
);

CREATE INDEX idx_organization_invites_org ON organization_invites(org_id);
CREATE INDEX idx_organization_invites_token ON organization_invites(token);
CREATE INDEX idx_organization_invites_pending
    ON organization_invites(email)
    WHERE accepted_at IS NULL;

-- Add tenant_id to projects. Nullable for the brief window between
-- this ALTER and the backfill INSERT below; after that, every row
-- has a value.
ALTER TABLE projects ADD COLUMN tenant_id TEXT REFERENCES organizations(org_id);
CREATE INDEX idx_projects_tenant ON projects(tenant_id);

-- =====================================================================
-- BACKFILL
-- =====================================================================
-- For every existing project, create an organization named after the
-- project, with the project's owner_user_id as the admin. The org_id
-- is derived from the project_id with an 'org_' prefix so the IDs
-- correlate visually in logs during the transition window.
--
-- We use strftime('%s', 'now') to get a UNIX timestamp; the columns
-- are INTEGER (epoch seconds) to match the rest of the schema.
INSERT INTO organizations (org_id, name, created_by_user_id, created_at, updated_at)
SELECT
    'org_' || project_id,
    COALESCE(name, 'Personal'),
    COALESCE(owner_user_id, ''),
    CAST(strftime('%s', 'now') AS INTEGER),
    CAST(strftime('%s', 'now') AS INTEGER)
FROM projects
WHERE owner_user_id IS NOT NULL AND owner_user_id != '';

INSERT INTO organization_members (org_id, user_id, role, email, added_by_user_id, added_at)
SELECT
    'org_' || p.project_id,
    p.owner_user_id,
    'admin',
    p.owner_email,
    NULL,
    CAST(strftime('%s', 'now') AS INTEGER)
FROM projects p
WHERE p.owner_user_id IS NOT NULL AND p.owner_user_id != '';

UPDATE projects
SET tenant_id = 'org_' || project_id
WHERE owner_user_id IS NOT NULL AND owner_user_id != '';

INSERT INTO schema_migrations (version) VALUES (13);
