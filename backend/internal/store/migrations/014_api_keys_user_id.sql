-- Migration 014: api_keys.user_id for per-member API keys (RBAC).
--
-- Until this migration, every API key just identified a PROJECT, with
-- no link to which user / member the key was minted for. Combined with
-- the team-membership model (), that meant every key effectively
-- authenticated as the project's owner -- which is always admin --
-- regardless of which member generated or accepted the key. A READ
-- member could send invites, change roles, or remove members because
-- the role check was on the project owner, not the caller.
--
-- This migration adds api_keys.user_id and is the schema foundation
-- for proper per-member role enforcement. The auth middleware reads
-- the column on every request and stamps the caller's identity onto
-- the request context; downstream handlers (resolveAdminContext,
-- future role checks) consult the caller's role in the org.
--
-- Backfill semantics:
--   user_id IS NULL for every pre-014 row. This is the "legacy" path,
--   and the auth middleware treats it as "caller identity unknown,
--   fall back to project.owner_user_id or owner_email." That keeps
--   existing keys working without forcing a re-mint.
--
-- Going forward:
--   - signup mints the project's first key with user_id = owner_email
--     (same email-as-user-id convention the invite-accept flow uses).
--   - HandleCreateAPIKey on /app/api-keys page stamps user_id = the
--     caller's user_id from context (so an admin-generated key for
--     themselves is owned by the admin; an admin-generated key on
--     behalf of a member isn't supported in this UI yet, but the
--     schema allows it).
--   - HandleAcceptInvite mints the invitee's first key with user_id =
--     the invitee's email.

ALTER TABLE api_keys ADD COLUMN user_id TEXT;
CREATE INDEX idx_api_keys_user_id ON api_keys(user_id);

INSERT INTO schema_migrations (version) VALUES (14);
