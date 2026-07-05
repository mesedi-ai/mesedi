-- 047_failure_group_severity_hint.sql
--
-- validator_failures.G1: SDK accepts a `severity` parameter on
-- validator_result(...) but the backend detector ignored it.
-- This migration adds severity_hint to failure_groups so the
-- handler can persist the SDK-supplied value at GroupValidatorFailure
-- time. The severity resolution chain becomes:
--
--   1. per-project class override (project_class_severities) — wins
--   2. severity_hint (this column) — used when no class override
--   3. severity.Default(failureClass) — fallback
--
-- Scope: only validator_failures populates this column today
-- (no other detector's SDK helper accepts a severity hint). Future
-- detectors that gain SDK severity hints reuse this column.
--
-- Forward-only. Existing failure_groups have NULL severity_hint
-- and continue to resolve via class override OR class default.

ALTER TABLE failure_groups
    ADD COLUMN severity_hint TEXT NULL;

INSERT OR IGNORE INTO schema_migrations (version) VALUES (47);
