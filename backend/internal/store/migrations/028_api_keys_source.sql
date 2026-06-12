-- Migration 028: api_keys.source for session-grade keys minted by
-- SSO login and magic-link sign-in (#196 commit 1 + 2).
--
-- Background. Before #196 the dashboard only had ONE way in: paste an
-- API key on /login. That key was minted at /signup (source='signup'
-- semantically; the column did not exist yet) or via the admin CLI
-- (source='manual'). Both kinds are "long-lived" credentials that the
-- customer is expected to store somewhere durable -- a password
-- manager, an .env file, their SDK config.
--
-- #196 adds two new dashboard sign-in paths: SSO via Google/GitHub and
-- magic-link via email. Both paths produce a NEW api_keys row under
-- the hood (we hash keys at storage, so we cannot return the
-- customer's existing key after the OAuth handshake). These rows are
-- SESSION-GRADE rather than long-lived: they exist purely to keep the
-- customer signed in to the dashboard for a short window, and the
-- customer must never see them in the /admin/api-keys UI.
--
-- This migration adds a `source` discriminator so we can:
--
--   1. Filter session-grade keys OUT of customer-facing listings (the
--      /admin/api-keys page and the GET /admin/api-keys endpoint) so
--      the user's key list stays clean and meaningful.
--   2. Set a short expires_at on session-grade rows (7 days from mint)
--      so a stale localStorage entry on an old browser does NOT remain
--      a valid bearer forever. The existing retention job already
--      deletes rows where expires_at < now() once they are revoked,
--      so this dovetails with the time-boxed credentials feature
--      added in migration 015.
--   3. Audit-log differently for session keys (the audit_events row
--      records "sso_login (google)" / "magic_link" instead of
--      "manual key mint by admin").
--
-- Backfill: every existing row backfills to source='manual' rather
-- than 'signup' because we cannot retroactively distinguish keys
-- minted at /signup from keys minted by the admin CLI; treating them
-- ALL as long-lived 'manual' keys keeps the filtering rule simple
-- (anything not session-grade is shown to the customer). Future
-- signup paths set source='signup' explicitly so the column reflects
-- reality going forward.
--
-- The DEFAULT 'manual' guards any forgotten insert path: a code
-- branch that creates a key without setting source still produces a
-- visible, long-lived key rather than a hidden, short-lived one.
-- That fails safe -- a too-visible key is a customer-noticeable bug,
-- a too-hidden key is a customer-can't-find-their-key bug.

ALTER TABLE api_keys ADD COLUMN source TEXT NOT NULL DEFAULT 'manual';

CREATE INDEX idx_api_keys_source ON api_keys(source);

INSERT INTO schema_migrations (version) VALUES (28);
