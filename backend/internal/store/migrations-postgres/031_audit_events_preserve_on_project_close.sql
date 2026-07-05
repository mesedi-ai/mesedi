-- 031: Preserve audit_events when projects are closed (PL pre-launch).
--
-- See sqlite migrations/031_audit_events_preserve_on_project_close.sql
-- for the design rationale, goals (G1 + G2 + G3), and retention posture.
-- Short version: drop the FK so audit rows outlive project deletion;
-- add project_name_snapshot + project_deleted_at so closed-project
-- audit history stays readable and filterable.
--
-- Postgres can ALTER TABLE to drop an FK directly, so this is a
-- four-statement migration vs. SQLite's table rebuild.

-- Step 1: Drop the FK so the row survives DeleteProjectCascade.
-- Use the default constraint name Postgres generates for unnamed
-- inline REFERENCES (table_column_fkey). IF EXISTS guards against
-- re-runs or environments where the constraint was already dropped.
ALTER TABLE audit_events
    DROP CONSTRAINT IF EXISTS audit_events_project_id_fkey;

-- Step 2: Add the snapshot columns. NULL on existing rows is
-- correct: every live project's audit rows have these NULL by
-- definition. The close handler sets them on its way through
-- DeleteProjectCascade.
ALTER TABLE audit_events
    ADD COLUMN IF NOT EXISTS project_name_snapshot TEXT;

ALTER TABLE audit_events
    ADD COLUMN IF NOT EXISTS project_deleted_at TIMESTAMPTZ;

-- Step 3: New index for the admin-side "search closed-project
-- audit history by actor email" query (R1 forensics + R2 customer
-- support response). Composite on (actor_email, project_deleted_at)
-- so a query for "email X across all closed projects" gets a
-- direct index hit.
CREATE INDEX IF NOT EXISTS idx_audit_events_actor_email_deleted_at
    ON audit_events (actor_email, project_deleted_at);

INSERT INTO schema_migrations (version) VALUES (31);
