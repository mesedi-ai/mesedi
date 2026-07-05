-- 048_detector_thresholds.sql
--
-- Per-project tunable thresholds for the six detectors covered by
-- Theme B (semantic_loop, token_waste, tool_schema_drift,
-- grounding_failure, drift, context_overflow). Closes the
-- hardcoded-threshold .G* gaps from each detector's audit by
-- letting customers override values through a single shared
-- primitive instead of separate per-detector columns.
--
-- Normalized: one row per (project_id, detector, threshold_key)
-- override. Customers who never tune have ZERO rows; the
-- detector hot path falls back to the validators-registry default
-- in that case. This means storage growth is bounded by
-- (projects * tunables-actually-used), not (projects * all-tunables).
--
-- value_json is the threshold value JSON-encoded at the API layer:
--   - int thresholds:    "3" or "2048"
--   - float thresholds:  "0.45" or "0.9"
-- The validators registry in backend/internal/api parses + bounds-
-- checks per (detector, threshold_key) before insert, so the store
-- only sees values that already passed validation.
--
-- (detector, threshold_key) values are constrained to the
-- registry-known list at the API layer. The store accepts any
-- string so a future detector / threshold can be added by
-- registering a new spec without a schema change.
--
-- Theme B sequence:
--   B.a (this migration) — storage + REST API + validators registry
--   B.b — wire 6 detectors to read per-project values
--   B.c — dashboard editor
--   B.d — telemetry + integration tests + customer docs

CREATE TABLE IF NOT EXISTS detector_thresholds (
    project_id     TEXT NOT NULL,
    detector       TEXT NOT NULL,
    threshold_key  TEXT NOT NULL,
    value_json     TEXT NOT NULL,
    created_at     TEXT NOT NULL,
    updated_at     TEXT NOT NULL,
    PRIMARY KEY (project_id, detector, threshold_key)
);

CREATE INDEX IF NOT EXISTS idx_detector_thresholds_project_detector
    ON detector_thresholds(project_id, detector);

INSERT OR IGNORE INTO schema_migrations (version) VALUES (48);
