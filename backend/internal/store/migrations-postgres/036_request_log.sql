-- Migration 036: request_log table (#256).
--
-- See migrations/036_request_log.sql for the design rationale.
-- This file is the Postgres mirror.

CREATE TABLE request_log (
    log_id       BIGSERIAL PRIMARY KEY,
    project_id   TEXT NOT NULL,
    api_key_id   TEXT NOT NULL,
    ip_address   TEXT,
    method       TEXT NOT NULL,
    path         TEXT NOT NULL,
    status_code  INTEGER NOT NULL,
    received_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX idx_request_log_api_key_received
    ON request_log (api_key_id, received_at DESC);

CREATE INDEX idx_request_log_project_received
    ON request_log (project_id, received_at DESC);

CREATE INDEX idx_request_log_received_at
    ON request_log (received_at);

INSERT INTO schema_migrations (version) VALUES (36);
