-- Migration 023: tighten the card_on_file backfill (#209 hotfix).
--
-- See migrations/023_card_on_file_strict_backfill.sql for the design
-- rationale. This file is the Postgres mirror.

UPDATE projects
SET card_on_file = CASE
    WHEN tier = 'team'
         AND stripe_subscription_id IS NOT NULL
         AND stripe_subscription_id != ''
    THEN TRUE
    ELSE FALSE
END;

INSERT INTO schema_migrations (version) VALUES (23);
