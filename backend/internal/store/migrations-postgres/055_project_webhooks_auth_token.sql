-- Migration 055: add auth_token column to project_webhooks.
--
-- Postgres twin of migrations/055_project_webhooks_auth_token.sql.
-- See that file for full context. Semantics are identical; postgres
-- accepts the same ALTER TABLE ADD COLUMN syntax.
--
-- Background: PagerDuty's Events API v2 authenticates via a
-- routing_key inside the request body, not via HTTP headers. The
-- existing `secret` column holds Mesedi's server-generated HMAC key;
-- PagerDuty's routing_key is a customer-generated 32-char
-- integration ID. Two unrelated concepts, two columns.
--
-- Nullable so non-PagerDuty webhooks (Slack/Discord/generic) don't
-- need to populate it. HandleCreateWebhook enforces at the API
-- layer that PagerDuty URLs must supply a non-empty auth_token.

ALTER TABLE project_webhooks ADD COLUMN auth_token TEXT;

INSERT INTO schema_migrations (version) VALUES (55);
