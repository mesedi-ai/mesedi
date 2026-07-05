-- Migration 024: audit_events table (v1).
--
-- Backs the Cloud Team pricing-page claim of "Audit logs (who did
-- what, when)." Every state-changing customer admin action writes
-- a row here so Team customers can see the full audit trail of
-- their own admins: API key mints/revokes, webhook create/delete,
-- billing cap changes, subscription downgrades, account close, and
-- payment-method removal at v1.
--
-- Schema:
--   event_id        Mesedi-issued "audit_<random>" identifier
--   project_id      project the action targeted; cascade-delete
--   actor_key_id    API key that performed the action (FK soft;
--                    keys can be revoked but audit history must
--                    survive — so this is a string, not a FK)
--   actor_key_name  human-readable key name at the time of action
--                    (captured to survive future key renames)
--   actor_email     owner email of the API key at the time of
--                    action (captured to survive owner-email
--                    changes; helps the audit reader)
--   action          dotted slug, e.g. "api_key.create",
--                    "billing.cap_update". Stable identifiers so
--                    the UI can localize / icon them.
--   target_type     resource type, e.g. "api_key", "webhook",
--                    "project", "billing_cap"
--   target_id       resource id (may be NULL for actions that
--                    don't have a single target, e.g.
--                    "billing.account_closed")
--   metadata_json   free-form context, e.g.
--                    {"before": {"cap": 200}, "after": {"cap": 500}}
--                    Schema is per-action; UI introspects.
--   created_at      UTC timestamp of the event
--
-- Performance: the only common query is "list for one project
-- ordered by recency" so a composite index on (project_id,
-- created_at DESC) covers the hot path.

CREATE TABLE audit_events (
    event_id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(project_id) ON DELETE CASCADE,
    actor_key_id TEXT,
    actor_key_name TEXT,
    actor_email TEXT,
    action TEXT NOT NULL,
    target_type TEXT,
    target_id TEXT,
    metadata_json TEXT,
    created_at TIMESTAMP NOT NULL
);

CREATE INDEX idx_audit_events_project_created_at
    ON audit_events (project_id, created_at DESC);

INSERT INTO schema_migrations (version) VALUES (24);
