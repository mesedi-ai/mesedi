-- Migration 021: Hobby billing scheduler state columns ().
--
-- Adds two columns to projects so the new HobbyBillingScheduler can
-- track its retry cadence and auto-downgrade decision per project:
--
--   hobby_billing_last_attempt_at:
--     Timestamp of the most recent charge attempt against the
--     project's saved payment method, success or failure. The
--     scheduler reads this to enforce the every-other-day retry
--     cadence: a fresh attempt is allowed when last_attempt_at IS
--     NULL or > 48 hours ago. NULL on rows that have never been
--     attempted (the default for every existing Hobby project).
--
--   hobby_billing_consecutive_failures:
--     Counter that increments on each failed charge and resets to
--     zero on a successful charge. When this crosses 5 the scheduler
--     auto-detaches the saved card (clears stripe_customer_id),
--     hard-capping the project at the included free quota until the
--     customer attaches a new card. Default 0 for new and existing
--     rows; zero means "no failures recorded, scheduler may attempt
--     normally."
--
-- Both columns are Hobby-tier-specific by usage but the schema does
-- not enforce that; the scheduler's query filters by tier. Team /
-- Enterprise tiers never read or write these columns because their
-- billing flows through Stripe subscriptions, not the Hobby
-- per-period scheduler.

ALTER TABLE projects ADD COLUMN hobby_billing_last_attempt_at TIMESTAMP;
ALTER TABLE projects ADD COLUMN hobby_billing_consecutive_failures INTEGER NOT NULL DEFAULT 0;

INSERT INTO schema_migrations (version) VALUES (21);
