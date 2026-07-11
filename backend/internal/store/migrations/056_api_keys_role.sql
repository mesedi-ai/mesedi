-- Migration 056: add role column to api_keys.
--
-- Background: prior to this migration, every API key inherited its
-- effective role from the org membership of the user who minted it
-- (via api_keys.user_id → organization_members.role). That meant an
-- admin could only mint admin-grade keys — there was no way to
-- create a scoped "write-only" or "read-only" credential for a CI
-- pipeline, a monitoring script, or a partner integration.
--
-- Adding `role` as an explicit per-key column decouples the key's
-- authorization from the caller's identity. When set (admin/write/
-- read), the resolver uses it verbatim. When NULL, the resolver
-- falls back to the existing user-role lookup — preserving behavior
-- for every key minted before this migration.
--
-- Validation lives in the API layer, not the DB: HandleCreateAPIKey
-- rejects requests with an unknown role string. Making it an ENUM
-- at the schema level would fragment sqlite / postgres syntax and
-- offers no meaningful defense-in-depth beyond the handler check.
--
-- Reversal: DROP COLUMN role. Safe because pre-migration rows had
-- NULL role and the resolver already handles NULL.

ALTER TABLE api_keys ADD COLUMN role TEXT;

INSERT INTO schema_migrations (version) VALUES (56);
