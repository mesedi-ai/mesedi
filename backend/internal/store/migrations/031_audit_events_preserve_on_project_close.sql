-- 031: Preserve audit_events when projects are closed (PL pre-launch).
--
-- Background: until now, audit_events had ON DELETE CASCADE on
-- project_id, so the entire audit history was erased the moment a
-- project was closed via /app/settings Danger Zone. That made:
--   G1 account-takeover forensics impossible (we could not even
--      tell a victim who pressed Close from what IP),
--   G2 SOC 2 / financial-compliance retention expectations
--      impossible to honor, and
--   G3 self-side abuse-pattern detection (open account, burn
--      credit, close before invoice) blind to the close step.
-- See task PL8 testing and the close-account email rewrite
-- in mailer.go SendAccountClosed for the original rationale.
--
-- This migration:
--   1. Drops the FK relationship between audit_events.project_id
--      and projects.project_id. The column stays (semantically
--      meaningful, queryable) but is no longer FK-constrained.
--      Audit events become immutable historical records that
--      outlive their source project.
--   2. Adds project_name_snapshot, populated by the close handler
--      on its way through DeleteProjectCascade, so audit rows
--      stay human-readable after the projects row is gone.
--   3. Adds project_deleted_at, NULL while the project is alive,
--      set to the close timestamp during the close handler so
--      live-vs-archived audit queries can filter cheaply.
--
-- Retention posture: rows survive forever by default; a
-- separate 7-year cleanup job (task ) and a GDPR purge
-- endpoint (task ) ship post-launch.
--
-- SQLite cannot ALTER TABLE to drop an FK, so we rebuild the
-- table. PRAGMA foreign_keys is OFF for the duration of this
-- migration (the runner sets it back ON after).

-- Step 1: Create new shape.
CREATE TABLE audit_events_new (
    event_id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL,
    actor_key_id TEXT,
    actor_key_name TEXT,
    actor_email TEXT,
    action TEXT NOT NULL,
    target_type TEXT,
    target_id TEXT,
    metadata_json TEXT,
    created_at TIMESTAMP NOT NULL,
    project_name_snapshot TEXT,
    project_deleted_at TIMESTAMP
);

-- Step 2: Copy existing rows. project_name_snapshot and
-- project_deleted_at stay NULL on the backfill, which is correct
-- (every live project's audit rows have those NULL by definition).
INSERT INTO audit_events_new (
    event_id, project_id, actor_key_id, actor_key_name, actor_email,
    action, target_type, target_id, metadata_json, created_at
)
SELECT
    event_id, project_id, actor_key_id, actor_key_name, actor_email,
    action, target_type, target_id, metadata_json, created_at
FROM audit_events;

-- Step 3: Replace old table.
DROP TABLE audit_events;
ALTER TABLE audit_events_new RENAME TO audit_events;

-- Step 4: Recreate the original index used by the customer-facing
-- audit log page (recent events for one project).
CREATE INDEX idx_audit_events_project_created_at
    ON audit_events (project_id, created_at DESC);

-- Step 5: New index for the admin-side "search closed-project
-- audit history by actor email" query (R1 forensics + R2 customer
-- support response). Composite on (actor_email, project_deleted_at)
-- so a query for "email X across all closed projects" gets a
-- direct index hit.
CREATE INDEX idx_audit_events_actor_email_deleted_at
    ON audit_events (actor_email, project_deleted_at);

INSERT INTO schema_migrations (version) VALUES (31);
