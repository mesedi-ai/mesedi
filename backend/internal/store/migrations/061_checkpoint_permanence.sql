-- Migration 061: checkpoints are permanent evidence.
--
-- THE DECISION THIS MIGRATION RECORDS
--
-- The checkpoint tables get no retention path, ever. The alternative
-- on the ledger was a delete path with anchoring-aware retention; the
-- decision is the opposite, for three reasons that go to what the
-- product is:
--
--   1. Each checkpoint commits to its predecessor and to a public
--      transparency-log entry. Deleting any prefix of the chain
--      destroys the data needed to verify everything after it from
--      genesis, and the anchors in the public log would then name
--      hashes nothing local can reproduce.
--   2. The rows are tiny. One checkpoint per hour plus one leaf per
--      active tenant per hour is thousands of rows per year, not
--      millions. There is no storage pressure to trade evidence away
--      for, and if there ever is, the answer is an export-and-verify
--      archival format designed on purpose, not a DELETE.
--   3. checkpoint_tenant_leaves carries project_id, but its rows are
--      interval roots, counts and chain hashes: sealed pseudonymous
--      aggregates, not personal data. Project deletion deliberately
--      leaves them in place today (the same posture audit_events
--      takes), because removing one tenant's leaves breaks chain
--      verification for every other tenant in those intervals.
--
-- THE MECHANISM
--
-- BEFORE DELETE triggers on both tables refuse every delete with a
-- message naming this decision. Triggers rather than a foreign-key
-- change because they guard DIRECT deletes on both tables without
-- rebuilding an evidence table's bytes, and because the original
-- schema's ON DELETE CASCADE from leaves to checkpoints meant an
-- accidental checkpoint delete would silently erase leaves; with the
-- parent delete refused outright, that cascade is unreachable.
--
-- If a future need to remove rows ever appears real, the path is to
-- delete these triggers in a migration that records why, not to work
-- around them.
--
-- Reversal: DROP TRIGGER checkpoints_are_permanent;
--           DROP TRIGGER checkpoint_leaves_are_permanent;

CREATE TRIGGER IF NOT EXISTS checkpoints_are_permanent
BEFORE DELETE ON checkpoints
BEGIN
    SELECT RAISE(ABORT,
        'checkpoints are permanent evidence, deletion is refused (migration 061)');
END;

CREATE TRIGGER IF NOT EXISTS checkpoint_leaves_are_permanent
BEFORE DELETE ON checkpoint_tenant_leaves
BEGIN
    SELECT RAISE(ABORT,
        'checkpoint tenant leaves are permanent evidence, deletion is refused (migration 061)');
END;
