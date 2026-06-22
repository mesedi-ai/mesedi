-- 045_project_patterns.sql
--
-- Per-project custom pattern storage for the three security detectors
-- (prompt_injection, data_leakage, sandbox_escape). Closes the
-- audit gaps prompt_injection.G1 + data_leakage.G1 + sandbox_escape.G2
-- with one shared primitive.
--
-- Normalized: one row per pattern, indexable by (project_id, detector)
-- so the detector hot path reads in O(matching rows). Customers
-- can have variable numbers of patterns (5-200 expected; PROJECT_PATTERN_MAX
-- enforces a server-side ceiling).
--
-- Semantics:
--   - Additive vs built-ins: customer patterns AUGMENT Mesedi's curated
--     pattern set; do NOT replace. Detectors fire on either match.
--   - Each pattern is RE2-validated at the API layer before insert;
--     the column stores the raw RE2 source string.
--   - `enabled` lets customers disable a noisy pattern without losing
--     the rule definition.
--   - `match_count` is a running tally surfaced for telemetry; Wave
--     2.1.d increments it from the detector hot path.
--
-- detector values are constrained to the three security detectors that
-- share this primitive. A future PII detector (or other pattern-based
-- detector) can be added to the allow-list at the API layer without
-- a schema change.

CREATE TABLE IF NOT EXISTS project_patterns (
    pattern_id   TEXT PRIMARY KEY,
    project_id   TEXT NOT NULL,
    detector     TEXT NOT NULL,
    pattern      TEXT NOT NULL,
    severity     TEXT NOT NULL DEFAULT 'medium',
    description  TEXT NOT NULL DEFAULT '',
    enabled      INTEGER NOT NULL DEFAULT 1,
    created_by   TEXT NOT NULL DEFAULT '',
    created_at   TEXT NOT NULL,
    match_count  INTEGER NOT NULL DEFAULT 0
);

CREATE INDEX IF NOT EXISTS idx_project_patterns_project_detector
    ON project_patterns(project_id, detector);

CREATE INDEX IF NOT EXISTS idx_project_patterns_enabled
    ON project_patterns(project_id, detector, enabled);

INSERT OR IGNORE INTO schema_migrations (version) VALUES (45);
