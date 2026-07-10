-- Migration 054: align existing Hobby project retention to 7 days.
--
-- Context: HobbyDefaultRetentionDays was cut from 15 to 7 in an
-- earlier slice (see backend/internal/api/billing.go comment) to
-- match the PostHog free tier and create a real storage-cost delta
-- between Cloud Hobby (7 days) and Cloud Team (90 days). Migration
-- 020 previously clamped Hobby projects to 15, so the rows written
-- before this migration reflect the older 15-day cap; from the
-- customer's perspective a Hobby project that was on 15-day
-- retention has been silently over-retaining data ever since.
--
-- We backfill in two cases:
--
--   1. NULL retention_days on a Hobby project. NULL means "indefinite"
--      in the schema; the write path forbids this on Hobby, but a
--      legacy row created before that guard could still hold NULL.
--      Set to 7 so the nightly retention scheduler starts pruning to
--      the published Hobby cap.
--
--   2. retention_days > 7 on a Hobby project. Customer chose 15 (the
--      old cap) via the settings page while the old cap allowed it,
--      or was clamped there by migration 020. Either way, tighten
--      to 7 so the retention posture matches the current pricing card.
--
-- Team and Enterprise projects are not touched: their tier caps
-- did not change in this slice.
--
-- Forward-going enforcement lives in
-- backend/internal/api/tier_change_cascade.go — every future tier
-- flip (admin manual override, Stripe subscription updated, Stripe
-- subscription deleted) runs applyTierChangeCascade which re-clamps
-- retention to the destination tier's cap. This migration is the
-- one-shot fix for projects that downgraded before that cascade was
-- wired in.
--
-- Reversal: not needed. Migration 020 was a similar one-way clamp;
-- customers wanting longer retention upgrade to Team or Enterprise
-- to raise their cap.

UPDATE projects
SET retention_days = 7
WHERE tier = 'hobby'
  AND (retention_days IS NULL OR retention_days > 7);

INSERT INTO schema_migrations (version) VALUES (54);
