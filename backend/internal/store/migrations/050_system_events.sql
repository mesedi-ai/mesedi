-- 050_system_events.sql
--
-- Dedicated table for system-actor operational events ().
-- Separates "customer-visible admin actions" (audit_events) from
-- "system operational events" (system_events) so the audit log UI
-- stops surfacing system-actor=config_fallback rows that customers
-- don't recognize.
--
-- New events that flow into this table starting with this migration:
--   - action="config_fallback" target_type="project_config"
--     target_id=<config_key>  (when a per-project config read hits
--     an error and falls back to the hardcoded default)
--
-- Old config_fallback rows already in audit_events stay there as a
-- historical record — there is no backfill. The dashboard chip
-- queries system_events going forward; the audit_events trail
-- remains queryable for ops without leaking into the customer view.
--
-- Schema mirrors the audit_events shape minus the actor_* columns
-- since system events have no human actor. payload_json captures
-- the same metadata structure recordAuditEventForProject already
-- writes (error string, fallback value).

CREATE TABLE IF NOT EXISTS system_events (
    event_id       TEXT PRIMARY KEY,
    project_id     TEXT NOT NULL,
    actor          TEXT NOT NULL,
    action         TEXT NOT NULL,
    target_type    TEXT NOT NULL,
    target_id      TEXT NOT NULL,
    payload_json   TEXT,
    created_at     TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_system_events_project_created
    ON system_events(project_id, created_at DESC);

CREATE INDEX IF NOT EXISTS idx_system_events_project_action_target
    ON system_events(project_id, action, target_type, target_id);

INSERT OR IGNORE INTO schema_migrations (version) VALUES (50);
