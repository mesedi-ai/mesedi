-- Migration 061: checkpoints are permanent evidence.
--
-- Postgres twin of migrations/061_checkpoint_permanence.sql. Read that
-- file for the full reasoning; the semantics are identical: BEFORE
-- DELETE on both checkpoint tables refuses every delete, because the
-- chain commits each row to its predecessor and to a public
-- transparency-log entry, and removing any row destroys the ability to
-- verify everything after it.
--
-- Dialect differences: a Postgres row trigger needs a named plpgsql
-- function, and Postgres has no IF NOT EXISTS on CREATE TRIGGER, so
-- idempotency comes from DROP TRIGGER IF EXISTS first (safe: the drop
-- and recreate land in the same migration apply). FOR EACH ROW, not
-- FOR EACH STATEMENT, to match SQLite exactly: a DELETE that matches
-- zero rows succeeds as a no-op on both engines.
--
-- Reversal: DROP TRIGGER checkpoints_are_permanent ON checkpoints;
--           DROP TRIGGER checkpoint_leaves_are_permanent
--               ON checkpoint_tenant_leaves;
--           DROP FUNCTION refuse_checkpoint_delete();

CREATE OR REPLACE FUNCTION refuse_checkpoint_delete() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION
        '% rows are permanent evidence, deletion is refused (migration 061)',
        TG_TABLE_NAME;
END;
$$ LANGUAGE plpgsql;

DROP TRIGGER IF EXISTS checkpoints_are_permanent ON checkpoints;
CREATE TRIGGER checkpoints_are_permanent
    BEFORE DELETE ON checkpoints
    FOR EACH ROW EXECUTE FUNCTION refuse_checkpoint_delete();

DROP TRIGGER IF EXISTS checkpoint_leaves_are_permanent ON checkpoint_tenant_leaves;
CREATE TRIGGER checkpoint_leaves_are_permanent
    BEFORE DELETE ON checkpoint_tenant_leaves
    FOR EACH ROW EXECUTE FUNCTION refuse_checkpoint_delete();
