-- 049_project_detector_allowlist.sql
--
-- Per-project allowlist entries for the three detectors that share
-- the Allowlist primitive (crashes, tool_failures, validator_failures).
-- Closes the audit gaps crashes.G3 + tool_failures.G4 + validator_failures.G5
-- with one shared primitive — same Wave 2.1-shape close-out as
-- project_patterns.
--
-- Normalized: one row per allowlist entry, indexable by
-- (project_id, detector) so the detector hot path reads in
-- O(matching rows). Customers can have variable numbers of
-- allowlist entries (5-200 expected; PROJECT_ALLOWLIST_MAX
-- enforces a server-side ceiling).
--
-- Semantics:
--   - When the detector is about to call GroupX(executionID,
--     projectID, signature), it first checks
--     CheckAllowlistMatch(projectID, detector, signature). If a
--     row matches (project_id, detector, allowlist_key=signature),
--     the GroupX call is SKIPPED — no failure_group is created
--     for this execution+detector combination.
--   - The match_count column is incremented synchronously on
--     match so the dashboard can show "this entry suppressed N
--     failures" per row.
--   - allowlist_key is whatever the detector's signature looks
--     like today: crashes → exception_type, tool_failures →
--     tool_name, validator_failures → validator_name. If
--     granular signatures ship later (tool_failures.G3 /
--     validator_failures.G3), the column stays opaque-string —
--     the new signature shape just becomes the new
--     allowlist_key, no schema change.
--   - reason is a free-text customer-facing note ("known
--     timeout on slow endpoint", "expected validation failure on
--     X path") — surfaces in the dashboard editor.
--
-- detector values are constrained to the three detectors that
-- share this primitive (crashes, tool_failures, validator_failures)
-- at the API layer. A future fourth detector that wants allowlist
-- support drops in by adding its name to the API allow-list — no
-- schema change.

CREATE TABLE IF NOT EXISTS project_detector_allowlist (
    allowlist_id   TEXT PRIMARY KEY,
    project_id     TEXT NOT NULL,
    detector       TEXT NOT NULL,
    allowlist_key  TEXT NOT NULL,
    reason         TEXT NOT NULL DEFAULT '',
    created_by     TEXT NOT NULL DEFAULT '',
    created_at     TEXT NOT NULL,
    match_count    INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_project_detector_allowlist_project_detector
    ON project_detector_allowlist(project_id, detector);

CREATE INDEX IF NOT EXISTS idx_project_detector_allowlist_lookup
    ON project_detector_allowlist(project_id, detector, allowlist_key);

INSERT OR IGNORE INTO schema_migrations (version) VALUES (49);
