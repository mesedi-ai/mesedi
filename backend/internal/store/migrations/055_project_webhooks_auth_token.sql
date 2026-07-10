-- Migration 055: add auth_token column to project_webhooks.
--
-- Background: PagerDuty's Events API v2 authenticates every inbound
-- event via a routing_key inside the request body, not via HTTP
-- headers. Slack + Discord have no auth (they trust the URL secret).
-- The existing `secret` column on project_webhooks holds a server-
-- generated 256-bit HMAC key used to sign the X-Mesedi-Signature
-- header; it is NOT the same shape as a PagerDuty routing_key (their
-- keys are 32-char alphanumeric integration IDs the customer
-- generates on the PagerDuty side).
--
-- Adding `auth_token` as a separate column lets the customer paste
-- their PagerDuty routing_key on webhook creation without collapsing
-- two unrelated concepts into one field. The column is:
--
--   - Nullable: Slack/Discord/generic webhooks never populate it.
--   - Never returned in list responses (json:"-" on the Go struct).
--   - Never logged. Same posture as the existing `secret` column.
--
-- Enforcement lives in the API layer, not the DB: HandleCreateWebhook
-- refuses to create a PagerDuty webhook (URL prefix events.pagerduty
-- .com/v2/enqueue) when auth_token is empty. Making it NOT NULL at
-- the schema level would break every non-PagerDuty webhook already in
-- the customer's table.
--
-- Reversal: DROP COLUMN auth_token. Safe because non-PagerDuty
-- webhooks never populate it, and PagerDuty webhooks would just start
-- failing delivery instead of leaking data.

ALTER TABLE project_webhooks ADD COLUMN auth_token TEXT;

INSERT INTO schema_migrations (version) VALUES (55);
