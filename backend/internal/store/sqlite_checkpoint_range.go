package store

import (
	"context"
	"database/sql"
	"fmt"

	"mesedi/backend/internal/attest"
)

// Range reads over the checkpoint chain, for assembling an auditor's
// export.
//
// These exist instead of looping GetCheckpoint and GetCheckpointLeaves
// per sequence. A month of hourly checkpoints is ~744 rows; fetched one
// at a time that is ~1,500 round trips against Neon for a single export,
// which is the N+1 pattern the audit's B25 check exists to catch.

// MaxCheckpointRange caps a single range read.
//
// 744 is 31 days at one checkpoint an hour, which is the longest span an
// auditor is likely to want in one document and comfortably more than a
// typical reporting period.
//
// The cap REFUSES rather than truncating, on the same reasoning as the
// sealing caps: a silently shortened export is indistinguishable from an
// export of a chain with a hole in it, and this whole system exists to
// tell those two apart. A caller who wants more asks for it in ranges and
// knows they did.
const MaxCheckpointRange = 744

// AnchoredCheckpoint pairs a checkpoint with where it was published.
//
// Returned together because every consumer needs both and fetching the
// anchor separately would reintroduce the per-row round trip this file
// exists to avoid. They stay separate TYPES because the anchor is not
// hash-committed — see the note on CheckpointAnchor.
type AnchoredCheckpoint struct {
	Checkpoint attest.Checkpoint
	Anchor     CheckpointAnchor
}

// ValidateCheckpointRange is exported so the API layer can reject a bad
// range at the boundary, before a store round trip, without growing a
// second opinion about what "bad" means. One implementation, two callers.
func ValidateCheckpointRange(fromSeq, toSeq uint64) error {
	if fromSeq == 0 || toSeq == 0 {
		return fmt.Errorf("checkpoint range: sequences start at 1, got [%d, %d]",
			fromSeq, toSeq)
	}
	if toSeq < fromSeq {
		return fmt.Errorf("checkpoint range: to (%d) is before from (%d)", toSeq, fromSeq)
	}
	if n := toSeq - fromSeq + 1; n > MaxCheckpointRange {
		return fmt.Errorf(
			"checkpoint range [%d, %d] covers %d checkpoints, more than the %d "+
				"this reads at once. Request it in smaller ranges: a truncated "+
				"export cannot be told apart from a chain with a hole in it",
			fromSeq, toSeq, n, MaxCheckpointRange)
	}
	return nil
}

// ListCheckpointRange returns checkpoints fromSeq..toSeq inclusive, in
// sequence order, each with its anchor.
//
// Every row's stored hash is recomputed on the way out by
// scanCheckpointRow, so a range read is as strict as a single read. Gaps
// are not filled in or flagged here: this reports what is stored, and
// deciding whether a gap is a defect belongs to attest.VerifyChain, which
// is the one place that judgement should live.
func (s *SQLiteStore) ListCheckpointRange(
	ctx context.Context, fromSeq, toSeq uint64,
) ([]AnchoredCheckpoint, error) {
	if err := ValidateCheckpointRange(fromSeq, toSeq); err != nil {
		return nil, err
	}

	// Two queries rather than one wide one, so scanCheckpointRow stays the
	// single place a checkpoint row is decoded and its stored hash
	// recomputed. Selecting the anchor columns alongside would mean a
	// fourteen-column Scan that scanCheckpointRow cannot serve, and the
	// only way to write that is to copy its body — including the hash
	// check, which is precisely the line that must never exist twice.
	// Two round trips for a whole range is still O(1), which was the point.
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, format, prev_checkpoint_hash, prev_log_entry_id,
		       interval_start, interval_end, tenant_leaf_count, merkle_root,
		       cumulative_count, created_at_unattested, hash
		  FROM checkpoints
		 WHERE seq >= ? AND seq <= ?
		 ORDER BY seq
	`, int64(fromSeq), int64(toSeq))
	if err != nil {
		return nil, fmt.Errorf("list checkpoints [%d, %d]: %w", fromSeq, toSeq, err)
	}
	defer rows.Close()

	var out []AnchoredCheckpoint
	for rows.Next() {
		cp, err := scanCheckpointRow(rows)
		if err != nil {
			return nil, fmt.Errorf("checkpoint in [%d, %d]: %w", fromSeq, toSeq, err)
		}
		out = append(out, AnchoredCheckpoint{Checkpoint: *cp})
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return out, nil
	}

	anchors, err := s.listCheckpointAnchorRange(ctx, fromSeq, toSeq)
	if err != nil {
		return nil, err
	}
	for i := range out {
		out[i].Anchor = anchors[out[i].Checkpoint.Seq]
	}
	return out, nil
}

func (s *SQLiteStore) listCheckpointAnchorRange(
	ctx context.Context, fromSeq, toSeq uint64,
) (map[uint64]CheckpointAnchor, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, anchored_at, log_entry_id, ledger_backend, leaf_preimage
		  FROM checkpoints
		 WHERE seq >= ? AND seq <= ?
	`, int64(fromSeq), int64(toSeq))
	if err != nil {
		return nil, fmt.Errorf("list checkpoint anchors [%d, %d]: %w", fromSeq, toSeq, err)
	}
	defer rows.Close()

	out := make(map[uint64]CheckpointAnchor)
	for rows.Next() {
		var (
			seq                                   int64
			anchoredAt, logEntryID, ledgerBackend sql.NullString
			leafPreimage                          sql.NullString
		)
		if err := rows.Scan(&seq, &anchoredAt, &logEntryID, &ledgerBackend,
			&leafPreimage); err != nil {
			return nil, fmt.Errorf("scan checkpoint anchor: %w", err)
		}
		a := CheckpointAnchor{
			LogEntryID:    logEntryID.String,
			LedgerBackend: ledgerBackend.String,
			LeafPreimage:  leafPreimage.String,
		}
		if anchoredAt.Valid && anchoredAt.String != "" {
			// parseFlexTime, not mustParseStoredTime: anchored_at is not
			// hash-committed, so a malformed value degrades the display
			// rather than the chain. Anchored is driven by the log entry
			// id, which is what actually matters.
			a.AnchoredAt = parseFlexTime(anchoredAt.String)
		}
		a.Anchored = a.LogEntryID != ""
		out[uint64(seq)] = a
	}
	return out, rows.Err()
}

// ListCheckpointLeavesRange returns every tenant leaf for the range,
// keyed by checkpoint sequence and in committed (position) order.
//
// Returns ALL tenants' leaves, not just one project's, because building
// an inclusion proof requires the whole level. That is safe here and only
// here: this is a server-side read, and what leaves the building is the
// proof — about log2(n) opaque sibling hashes — never this map.
func (s *SQLiteStore) ListCheckpointLeavesRange(
	ctx context.Context, fromSeq, toSeq uint64,
) (map[uint64][]attest.TenantLeaf, error) {
	if err := ValidateCheckpointRange(fromSeq, toSeq); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT checkpoint_seq, project_id, interval_root, execution_count,
		       cumulative_count, prev_leaf_hash, leaf_hash
		  FROM checkpoint_tenant_leaves
		 WHERE checkpoint_seq >= ? AND checkpoint_seq <= ?
		 ORDER BY checkpoint_seq, position
	`, int64(fromSeq), int64(toSeq))
	if err != nil {
		return nil, fmt.Errorf("list checkpoint leaves [%d, %d]: %w", fromSeq, toSeq, err)
	}
	defer rows.Close()

	out := make(map[uint64][]attest.TenantLeaf)
	for rows.Next() {
		var (
			seq, cumulative int64
			l               attest.TenantLeaf
			storedHash      string
		)
		if err := rows.Scan(&seq, &l.ProjectID, &l.IntervalRoot, &l.ExecutionCount,
			&cumulative, &l.PrevLeafHash, &storedHash); err != nil {
			return nil, fmt.Errorf("scan leaf in [%d, %d]: %w", fromSeq, toSeq, err)
		}
		l.CumulativeCount = uint64(cumulative)

		// Recomputed here for the same reason the single-checkpoint read
		// does it: a stored hash that is never checked is decoration, and
		// a leaf read back unverified could carry an edited count into a
		// proof that then verifies against it.
		if got := attest.TenantLeafHash(l); got != storedHash {
			return nil, fmt.Errorf(
				"checkpoint %d leaf for project %q was stored with hash %s but its "+
					"columns now hash to %s; the row has been altered since it was written",
				uint64(seq), l.ProjectID, storedHash, got)
		}
		out[uint64(seq)] = append(out[uint64(seq)], l)
	}
	return out, rows.Err()
}
