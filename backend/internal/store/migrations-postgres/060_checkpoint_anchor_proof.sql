-- Postgres twin of migrations/060_checkpoint_anchor_proof.sql. See that
-- file for what the JSON envelope holds, why it is one opaque column
-- rather than six typed ones, and why the default is the empty string.
--
-- TEXT and not JSONB, deliberately. The value is evidence produced by
-- another party; storing it as JSONB would let Postgres reorder keys
-- and normalise whitespace on the way in, which changes the bytes. Any
-- later decision to hash or sign this envelope would then be operating
-- on something other than what arrived.
ALTER TABLE checkpoints ADD COLUMN IF NOT EXISTS anchor_proof_json TEXT NOT NULL DEFAULT '';
