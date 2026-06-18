-- Migration 034: billing_events table (#261).
--
-- See migrations/034_billing_events.sql for the design rationale.
-- This file is the Postgres mirror.

CREATE TABLE billing_events (
    event_id TEXT PRIMARY KEY,
    project_id TEXT NOT NULL REFERENCES projects(project_id) ON DELETE CASCADE,
    stripe_customer_id TEXT NOT NULL,
    kind TEXT NOT NULL,
    severity TEXT NOT NULL,
    stripe_object_id TEXT NOT NULL,
    amount_cents BIGINT NOT NULL DEFAULT 0,
    currency TEXT NOT NULL DEFAULT '',
    detail_json TEXT,
    received_at TIMESTAMPTZ NOT NULL,
    resolved_at TIMESTAMPTZ,
    resolved_by TEXT,
    resolution_note TEXT
);

CREATE INDEX idx_billing_events_project_received
    ON billing_events (project_id, received_at DESC);

CREATE INDEX idx_billing_events_unresolved
    ON billing_events (received_at)
    WHERE resolved_at IS NULL;

INSERT INTO schema_migrations (version) VALUES (34);
