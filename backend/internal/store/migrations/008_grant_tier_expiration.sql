-- Migration 008: add expiration timestamps for admin tier flips and credit grants.
--
-- Both columns are nullable; NULL means "never expires" — the
-- backwards-compatible default for projects created before this
-- migration and for grants/tier-flips made without an expiration.
--
-- Lazy enforcement: nothing in the database checks these columns.
-- billing.go's HandleGetBilling compares them to time.Now() on each
-- read and treats expired tiers/grants as Hobby/zero in the response.
-- The underlying column values stay intact so the admin can audit
-- "this customer had a 100K bonus that expired on Jun 23".
--
-- Single-expiration-per-project model: re-granting overwrites
-- granted_executions_expires_at. Multiple stacked grants with
-- independent expirations are deliberately out of scope for v1
-- (would need a separate credit_grants table).
ALTER TABLE projects ADD COLUMN granted_executions_expires_at INTEGER;
ALTER TABLE projects ADD COLUMN tier_expires_at INTEGER;
