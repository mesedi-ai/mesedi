-- Migration 015: api_keys.scope + api_keys.expires_at for admin-scope
-- keys + time-boxed credentials.
--
-- Until now Mesedi had two completely separate auth surfaces:
--   - Customer auth: bearer mesedi_sk_<random> looked up in api_keys.
--   - Admin auth:    bearer === MESEDI_ADMIN_TOKEN (a static Fly
--                    secret) compared in constant time.
--
-- The static admin token has obvious limitations: no per-operator
-- attribution, no revocation without redeploy, no expiration. This
-- migration lays the schema foundation for admin-scope keys that live
-- in the regular api_keys table alongside customer keys, so we can
-- mint / revoke / expire them like any other credential.
--
-- Backfill semantics:
--   - scope: every existing row backfills to 'customer'. The default
--     ensures any forgotten insert path stays safe; an old code path
--     that doesn't know about scope can't accidentally mint an admin
--     key.
--   - expires_at: empty string = "never expires" (mirrors the existing
--     last_used_at convention of using empty strings for "unset"). New
--     admin-scope keys minted from the dashboard accept an optional
--     RFC3339Nano UTC timestamp or a YYYY-MM-DD date the handler
--     parses as end-of-day UTC.
--
-- The auth middleware reads both columns on every request:
--   - scope='admin' + project_id='_admin' identifies an admin key.
--   - expires_at != '' && now > expires_at rejects identically to a
--     revoked / missing key.

ALTER TABLE api_keys ADD COLUMN scope      TEXT NOT NULL DEFAULT 'customer';
ALTER TABLE api_keys ADD COLUMN expires_at TEXT NOT NULL DEFAULT '';

CREATE INDEX idx_api_keys_scope ON api_keys(scope);

INSERT INTO schema_migrations (version) VALUES (15);
