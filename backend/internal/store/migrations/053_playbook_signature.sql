-- Migration 053: playbook-signature staleness tracking.
--
-- AI analyses are produced by injecting the canonical playbook MD
-- into the Claude prompt (Wave K). Playbook content changes
-- post-deploy invalidate the AI analysis cached on each
-- failure_group: the cached text was anchored on a now-stale
-- interpretation framework.
--
-- This migration adds the signature columns the staleness-detection
-- mechanism needs:
--
--   failure_groups.analysis_playbook_signature
--     SHA-256 of the playbook content used at the time the cached
--     failure_groups.analysis_markdown was generated. The dashboard
--     compares this to the in-binary signature of the current
--     playbook for this failure_class; mismatch surfaces the "Re-
--     analyze to refresh" badge.
--
--   ai_analyses.playbook_signature
--     Same value, also stored on the per-call history row so the
--     founder UI can show which analyses were produced against which
--     playbook content. Useful for auditing AI-analysis drift over
--     time independently of the latest-cached pointer.
--
-- Both columns are TEXT, nullable. Existing rows backfill to NULL.
-- The dashboard treats NULL signatures as "outdated, recommend re-
-- analyze" (the column didn't exist when those analyses ran, so
-- their staleness is genuinely unknown). New analyses written after
-- this migration carry their signatures.
--
-- Reversal: DROP COLUMN on both tables. No data loss because the
-- columns are derived from playbook content + analysis_markdown.

ALTER TABLE failure_groups ADD COLUMN analysis_playbook_signature TEXT;
ALTER TABLE ai_analyses ADD COLUMN playbook_signature TEXT;

INSERT INTO schema_migrations (version) VALUES (53);
