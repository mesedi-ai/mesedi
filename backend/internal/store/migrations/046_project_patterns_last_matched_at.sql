-- 046_project_patterns_last_matched_at.sql
--
-- Wave 2.1.d.1: per-pattern telemetry. Adds last_matched_at so the
-- dashboard can surface a "dormant" badge on rules that haven't
-- fired (added but never matched) vs rules that fire regularly.
--
-- Updated synchronously by IncrementPatternMatchCount alongside
-- the existing match_count bump. NULL means the pattern has never
-- matched (or matched before this column existed; treated identically
-- as 'dormant').
--
-- Stored as TEXT (RFC3339) to match the existing created_at
-- convention on this table. Postgres twin uses TIMESTAMPTZ.

ALTER TABLE project_patterns
    ADD COLUMN last_matched_at TEXT NULL;

INSERT OR IGNORE INTO schema_migrations (version) VALUES (46);
