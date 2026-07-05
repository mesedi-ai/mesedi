-- Migration 024: audit_events table (v1).
--
-- See migrations/024_audit_events.sql for the design rationale.
-- This file is the Postgres mirror.

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
    created_at TIMESTAMPTZ NOT NULL
);

CREATE INDEX idx_audit_events_project_created_at
    ON audit_events (project_id, created_at DESC);

INSERT INTO schema_migrations (version) VALUES (24);
