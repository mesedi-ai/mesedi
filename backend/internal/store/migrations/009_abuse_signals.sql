-- Migration 009: abuse_signals audit table + projects.suspended_at column.
--
-- Backs the ToS commitment to "suspension or termination for cause"
-- with a 24h notification window. Every detected abuse event lands
-- in this table; the background suspension worker reads from it.
--
-- State machine for a single signal:
--   detected_at   set on insert. Always non-null.
--   notified_at   set when the 24h-warning email is sent. Until then,
--                 the signal is in "fresh" state.
--   suspended_at  set when the project is auto-suspended (notified_at
--                 + 24h with no human resolution). The auth middleware
--                 then refuses authenticated requests for the project.
--   resolved_at   set when a human (operator) dismisses the signal.
--                 Once resolved, no further state transitions fire.
--
-- detail is a JSON-encoded payload (varies per kind). The exact shape
-- is owned by the abuse-detector code, not the schema, so new
-- detectors can add fields without a migration.
--
-- We index by (resolved_at IS NULL) on detected_at so the background
-- worker's "fetch unresolved" scan is cheap even as the table grows.
-- SQLite supports partial indexes; if/when we migrate to Postgres,
-- the same partial index syntax carries over verbatim.
CREATE TABLE abuse_signals (
    signal_id        TEXT PRIMARY KEY,
    project_id       TEXT NOT NULL,
    kind             TEXT NOT NULL,
    severity         TEXT NOT NULL,
    detail           TEXT,
    detected_at      INTEGER NOT NULL,
    notified_at      INTEGER,
    suspended_at     INTEGER,
    resolved_at      INTEGER,
    resolved_by      TEXT,
    resolution_note  TEXT,
    FOREIGN KEY (project_id) REFERENCES projects(project_id) ON DELETE CASCADE
);

CREATE INDEX idx_abuse_signals_project ON abuse_signals(project_id);
CREATE INDEX idx_abuse_signals_unresolved
    ON abuse_signals(detected_at)
    WHERE resolved_at IS NULL;

-- Project-level suspension. NULL = active. Non-null timestamp = the
-- project's authenticated requests are refused with 403. Set by the
-- background worker (auto-suspension after 24h notice) or by an
-- admin via the manual suspend endpoint.
--
-- suspension_reason is a short human-readable string (e.g. "abuse:
-- rate_limit_sustained"). Useful for the admin dashboard and for
-- the customer-facing message when their request is rejected.
ALTER TABLE projects ADD COLUMN suspended_at INTEGER;
ALTER TABLE projects ADD COLUMN suspension_reason TEXT;
