-- Migration 054: align existing Hobby project retention to 7 days.
--
-- Postgres twin of migrations/054_align_hobby_retention_to_7_days.sql.
-- See that file for full context. Semantics are identical; the SQL
-- shape here is a plain UPDATE with no dialect-specific tricks.
--
-- Context: HobbyDefaultRetentionDays was cut from 15 to 7 in an
-- earlier slice (see backend/internal/api/billing.go comment) to
-- match the PostHog free tier and create a real storage-cost delta
-- between Cloud Hobby (7 days) and Cloud Team (90 days). Migration
-- 020 previously clamped Hobby projects to 15, so any Hobby row
-- written before this migration reflects the older 15-day cap.
--
-- Forward-going enforcement lives in
-- backend/internal/api/tier_change_cascade.go — every future tier
-- flip runs applyTierChangeCascade which re-clamps retention to the
-- destination tier's cap. This migration is the one-shot fix for
-- projects that already downgraded before that cascade was wired in.

UPDATE projects
SET retention_days = 7
WHERE tier = 'hobby'
  AND (retention_days IS NULL OR retention_days > 7);

INSERT INTO schema_migrations (version) VALUES (54);
