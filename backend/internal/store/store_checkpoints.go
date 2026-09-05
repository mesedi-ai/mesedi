package store

import (
	"context"
	"time"

	"mesedi/backend/internal/attest"
)

// The checkpoint chain: execution sealing, checkpoint persistence and
// anchor state. Read migrations 057 and 058 before changing anything
// here; the ordering rules are load-bearing.
//
// Split out of store.go on 2026-09-04. The Store interface had grown to
// 1,570 lines inside a 2,463-line file, which tripped the audit's
// 1000-line limit as a BLOCKING failure and made every store change
// carry it.
//
// This is a pure move. The declarations below are byte-identical to what
// they were in store.go and Store now embeds this interface, so every
// implementation and every caller is unchanged. Go does not care which
// file in a package a declaration lives in, which is what makes a split
// like this verifiable by compiling.

type CheckpointStore interface {
	// ── Checkpoint chain: execution sealing (migration 057) ──────────
	//
	// Sealing fixes, permanently, which hourly checkpoint an execution
	// belongs to. Neither started_at nor ended_at can do this job:
	// started_at would anchor digests over still-running executions, so
	// the root would change afterwards and read as tampering; ended_at
	// is mutable, so a later correction would move an already-anchored
	// execution between intervals. See migration 057 for the full
	// argument.

	// SealExecutions stamps sealed_at on executions that have ended and
	// settled, or that never ended and have timed out. Idempotent:
	// already-sealed rows are excluded, because interval membership must
	// be recorded once and never recomputed.
	SealExecutions(ctx context.Context, now time.Time, settle, timeout time.Duration) (int64, error)

	// CountSealedByProject returns per-project counts for [from, to).
	// Half-open, so an execution sealed exactly on an hour boundary
	// belongs to one interval rather than two.
	CountSealedByProject(ctx context.Context, from, to time.Time) (map[string]int, error)

	// ListSealedExecutionIDs returns one project's sealed execution ids
	// for [from, to) in the exact order their leaves must be hashed.
	// Ordering is part of the contract: the Merkle root depends on it.
	ListSealedExecutionIDs(ctx context.Context, projectID string, from, to time.Time) ([]string, error)

	// ── Checkpoint chain: persistence (migration 058) ────────────────
	//
	// These take and return attest.Checkpoint / attest.TenantLeaf
	// directly rather than store-local copies. Those structs are
	// HASH-COMMITTED: a parallel copy would drift, and a field that
	// exists on one and not the other silently changes what a stored
	// checkpoint recomputes to.

	// InsertCheckpoint writes a checkpoint and its leaves atomically.
	// Half of a checkpoint reads as corruption to a verifier rather
	// than as a crash. Refuses to overwrite an existing sequence.
	InsertCheckpoint(ctx context.Context, cp attest.Checkpoint, leaves []attest.TenantLeaf) error

	// LatestCheckpoint returns the chain head, or (nil, nil) before
	// genesis, an empty chain is a legitimate state, not an error.
	LatestCheckpoint(ctx context.Context) (*attest.Checkpoint, error)

	// GetCheckpoint returns one checkpoint by sequence, or (nil, nil).
	GetCheckpoint(ctx context.Context, seq uint64) (*attest.Checkpoint, error)

	// GetCheckpointLeaves returns an interval's leaves IN HASHED ORDER.
	// The root depends on that order; SELECT row order does not.
	GetCheckpointLeaves(ctx context.Context, seq uint64) ([]attest.TenantLeaf, error)

	// ListCheckpointRange returns checkpoints fromSeq..toSeq inclusive
	// with their anchors, in sequence order. Refuses a range wider than
	// MaxCheckpointRange rather than truncating: a shortened export
	// cannot be told apart from a chain with a hole in it.
	//
	// Exists so assembling an export is a fixed number of round trips
	// rather than two per checkpoint.
	ListCheckpointRange(ctx context.Context, fromSeq, toSeq uint64) ([]AnchoredCheckpoint, error)

	// ListCheckpointLeavesRange returns every tenant leaf in the range,
	// keyed by checkpoint sequence and IN HASHED ORDER within each.
	//
	// Returns all tenants' leaves, not one project's, because building an
	// inclusion proof needs the whole level. Safe only because this is a
	// server-side read: what leaves the building is the proof, which is
	// a handful of opaque sibling hashes, never this map.
	ListCheckpointLeavesRange(ctx context.Context, fromSeq, toSeq uint64) (map[uint64][]attest.TenantLeaf, error)

	// LatestTenantLeaf returns a project's most recent leaf, or
	// (nil, nil) if it has never appeared. Supplies the next leaf's
	// PrevLeafHash and running total.
	LatestTenantLeaf(ctx context.Context, projectID string) (*attest.TenantLeaf, error)

	// MarkCheckpointAnchored records where a checkpoint reached the
	// transparency log. That entry id becomes the NEXT checkpoint's
	// PrevLogEntryID, which is what ties the chain to a log Mesedi does
	// not control.
	//
	// Takes the whole CheckpointAnchor rather than a list of scalars.
	// The previous signature was (seq, logEntryID, ledgerBackend,
	// anchoredAt) and adding the preimage would have made four
	// same-typed strings in a row, which is a transposition waiting to
	// happen, and a transposed anchor is not a compile error, it is a
	// checkpoint that silently names the wrong log entry. Named fields
	// also mean the inclusion proof (task #25) can be added without
	// touching this signature again.
	//
	// Anchored and AnchoredAt on the argument are ignored: the store
	// sets the timestamp and derives Anchored on read.
	MarkCheckpointAnchored(ctx context.Context, seq uint64, a CheckpointAnchor, anchoredAt time.Time) error

	// GetCheckpointAnchor reports where a checkpoint reached the log,
	// or Anchored=false if it has not yet.
	//
	// Separate from the checkpoint itself because anchor state is NOT
	// hash-committed: it is learned after the checkpoint is built and
	// sealed. Putting these fields on attest.Checkpoint would change
	// what every checkpoint hashes to, and a checkpoint whose hash
	// depended on where it was anchored could not be built before it
	// was anchored, which is the wrong way round.
	//
	// The scheduler needs this because checkpoint N+1 names N's OWN log
	// entry, and without it the chain cannot be extended.
	GetCheckpointAnchor(ctx context.Context, seq uint64) (CheckpointAnchor, error)
}
