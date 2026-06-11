-- Migration 023: tighten the card_on_file backfill (#209 hotfix).
--
-- Migration 022 used DEFAULT 1 + a backfill that only flipped to 0
-- for rows with NULL stripe_customer_id. That left projects in a
-- broken state if their Stripe customer was created during partial
-- signup or earlier testing but the SetupIntent never landed a
-- verified payment method: card_on_file=TRUE but no card to charge.
-- The AI root-cause analyze handler's no-card gate then failed to
-- fire and let analyses proceed against missing cards.
--
-- This migration takes the conservative position:
--
--   * Team with an active Stripe subscription -> card_on_file=TRUE
--     (Stripe is actively billing $99/mo, which can only succeed if a
--     payment method is attached, so this is a verifiable signal)
--   * Everyone else -> card_on_file=FALSE
--
-- The false negative for Hobby projects that DO have a card attached
-- but no subscription is acceptable: those customers re-confirm via
-- the Setup Intent flow on /app/billing, which fires the
-- handleSetupIntentSucceeded webhook and sets card_on_file=TRUE
-- through the proper code path. Pre-launch this affects only test
-- accounts.
--
-- The new project creation path (separate code change) also sets
-- card_on_file=FALSE explicitly on INSERT instead of relying on the
-- column default, so this issue cannot recur for newly created
-- projects after this migration.

UPDATE projects
SET card_on_file = CASE
    WHEN tier = 'team'
         AND stripe_subscription_id IS NOT NULL
         AND stripe_subscription_id != ''
    THEN 1
    ELSE 0
END;

INSERT INTO schema_migrations (version) VALUES (23);
