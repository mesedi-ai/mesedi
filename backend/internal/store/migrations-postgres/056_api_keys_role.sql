-- Migration 056: add role column to api_keys.
--
-- Postgres twin of migrations/056_api_keys_role.sql. See that file
-- for full context. Semantics are identical; postgres accepts the
-- same ALTER TABLE ADD COLUMN syntax.
--
-- Prior to this migration, every API key inherited its effective
-- role from the org membership of the user who minted it. Adding
-- an explicit per-key role column lets an admin mint scoped
-- credentials (write, read) without inviting a new user just to
-- hold the lower role. NULL preserves the legacy user-role
-- inheriting behavior; the resolver returns the column value
-- verbatim when set.

ALTER TABLE api_keys ADD COLUMN role TEXT;

INSERT INTO schema_migrations (version) VALUES (56);
