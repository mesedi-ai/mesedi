-- Migration 036: request_log table (#256).
--
-- Persists one row per authenticated HTTP request that hits our
-- backend so the Terms commitment to "share the information we have
-- about the key's recent use" on a compromise report can be honored
-- for arbitrary URL traffic, not just for run-creation (#255).
--
-- Scope (Team-tier customers only): the request-log middleware
-- short-circuits and writes nothing when the authenticated project's
-- tier is Hobby or Enterprise. Hobby is excluded because (a) the
-- volume from free users would dominate the table and balloon Neon
-- storage cost, and (b) forensic logging is a Team-tier value-add
-- consistent with the rest of the pricing model. Enterprise gets a
-- bespoke retention/forensic arrangement contractually and does not
-- use this table.
--
-- Schema:
--   log_id       monotonic integer primary key; ordering doubles as
--                an insertion-order tiebreaker for rows with identical
--                received_at to the second.
--   project_id   project that authenticated the request. Non-null
--                (we only log authenticated requests).
--   api_key_id   the key that authenticated the request. Mirrors the
--                executions.api_key_id soft-reference posture: TEXT,
--                no FK, audit trail survives key revocation. Non-null.
--   ip_address   client IP as seen by Fly's proxy. Stored as TEXT to
--                accommodate both IPv4 dotted and IPv6 colon forms.
--   method       HTTP method (GET / POST / PATCH / DELETE).
--   path         URL path (no query string; we don't want secrets in
--                query strings landing in the audit log).
--   status_code  HTTP response code we returned.
--   received_at  UTC timestamp at request entry.
--
-- Indexes:
--   (api_key_id, received_at DESC) — the forensic query "what did
--     key X do between time A and B" hits this directly.
--   (project_id, received_at DESC) — broader scope for the customer
--     dashboard's "recent activity" surface (future).
--   (received_at) — supports the nightly retention purge.
--
-- Retention: 90 days, enforced by request_log_retention_scheduler in
-- the Go service. The scheduler runs daily and deletes rows where
-- received_at < now - 90d. Documented in the scheduler's comment.

CREATE TABLE request_log (
    log_id       INTEGER PRIMARY KEY AUTOINCREMENT,
    project_id   TEXT NOT NULL,
    api_key_id   TEXT NOT NULL,
    ip_address   TEXT,
    method       TEXT NOT NULL,
    path         TEXT NOT NULL,
    status_code  INTEGER NOT NULL,
    received_at  TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
);

CREATE INDEX idx_request_log_api_key_received
    ON request_log (api_key_id, received_at DESC);

CREATE INDEX idx_request_log_project_received
    ON request_log (project_id, received_at DESC);

CREATE INDEX idx_request_log_received_at
    ON request_log (received_at);

INSERT INTO schema_migrations (version) VALUES (36);
