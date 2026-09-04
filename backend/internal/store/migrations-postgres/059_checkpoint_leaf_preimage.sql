-- Postgres twin of migrations/059_checkpoint_leaf_preimage.sql. See that
-- file for why this column exists and why its default is the empty
-- string rather than a behaviour-preserving value.
ALTER TABLE checkpoints ADD COLUMN IF NOT EXISTS leaf_preimage TEXT NOT NULL DEFAULT '';
