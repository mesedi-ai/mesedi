-- Migration 022: card_on_file boolean separates "has a Stripe customer
-- record" from "has a card on file for billing" ().
--
-- Before this migration the code conflated those two concepts using
-- stripe_customer_id == "" as the signal for "no card." That worked
-- for Hobby (where the only reason to have a Stripe customer is to
-- have a card) but breaks Team's customer-initiated card removal:
-- we want to keep the Stripe customer linkage so the active
-- subscription stays addressable, but we ALSO want the hard-cap to
-- trigger as if there's no card.
--
-- Semantics:
--   card_on_file = TRUE   → there's a card we can charge off-session
--                            (the default; existing behavior preserved
--                            for the vast majority of rows)
--   card_on_file = FALSE  → no card to charge; ingest path hard-caps
--                            at the included quota, AI analysis path
--                            refuses overage requests, scheduler skips
--                            charge attempts
--
-- Backfill: any project with a non-NULL non-empty stripe_customer_id
-- is assumed to have a card on file (the prior invariant). Projects
-- with no Stripe customer have no card and start FALSE.
--
-- The DEFAULT TRUE on the column itself is a forward-looking pick:
-- new projects created after this migration will start with card on
-- file = TRUE the moment they attach a card, the same moment they
-- get a stripe_customer_id. Until then the existing project-creation
-- path (which inserts no stripe_customer_id) leaves the column at
-- the DEFAULT, which is wrong for the no-card initial state — so the
-- project-creation path is updated to set card_on_file = FALSE
-- explicitly until the first card-attach lands.

ALTER TABLE projects ADD COLUMN card_on_file BOOLEAN NOT NULL DEFAULT 1;

UPDATE projects
SET card_on_file = 0
WHERE stripe_customer_id IS NULL OR stripe_customer_id = '';

INSERT INTO schema_migrations (version) VALUES (22);
