-- Migration 022: card_on_file boolean separates "has a Stripe customer
-- record" from "has a card on file for billing" (#209).
--
-- See migrations/022_card_on_file.sql for the design rationale. This
-- file is the Postgres mirror.

ALTER TABLE projects ADD COLUMN card_on_file BOOLEAN NOT NULL DEFAULT TRUE;

UPDATE projects
SET card_on_file = FALSE
WHERE stripe_customer_id IS NULL OR stripe_customer_id = '';

INSERT INTO schema_migrations (version) VALUES (22);
