package store

import (
	"context"
	"database/sql"
	"fmt"

	"mesedi/backend/internal/attest"
)

// Postgres twin of sqlite_checkpoint_range.go. See that file for why
// these range reads exist and why the cap refuses rather than truncates.
//
// The dialect differences are the usual two and both matter here. Numbered
// placeholders instead of ?, and timestamps scanned straight into
// time.Time instead of through a string parse — TIMESTAMPTZ round-trips as
// a time, whereas SQLite stores text. The second difference is exactly
// where the false-tampering bug of 2026-09-04 lived, so it is worth
// naming: Postgres truncates to microseconds, which is why canonicalTime
// truncates before hashing.

func (s *PostgresStore) ListCheckpointRange(
	ctx context.Context, fromSeq, toSeq uint64,
) ([]AnchoredCheckpoint, error) {
	if err := ValidateCheckpointRange(fromSeq, toSeq); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, format, prev_checkpoint_hash, prev_log_entry_id,
		       interval_start, interval_end, tenant_leaf_count, merkle_root,
		       cumulative_count, created_at_unattested, hash
		  FROM checkpoints
		 WHERE seq >= $1 AND seq <= $2
		 ORDER BY seq
	`, int64(fromSeq), int64(toSeq))
	if err != nil {
		return nil, fmt.Errorf("list checkpoints [%d, %d]: %w", fromSeq, toSeq, err)
	}
	defer rows.Close()

	var out []AnchoredCheckpoint
	for rows.Next() {
		var (
			cp              attest.Checkpoint
			seq, cumulative int64
		)
		if err := rows.Scan(
			&seq, &cp.Format, &cp.PrevCheckpointHash, &cp.PrevLogEntryID,
			&cp.IntervalStart, &cp.IntervalEnd, &cp.TenantLeafCount, &cp.MerkleRoot,
			&cumulative, &cp.CreatedAtUnattested, &cp.Hash,
		); err != nil {
			return nil, fmt.Errorf("scan checkpoint in [%d, %d]: %w", fromSeq, toSeq, err)
		}
		cp.Seq = uint64(seq)
		cp.CumulativeCount = uint64(cumulative)
		cp.IntervalStart = cp.IntervalStart.UTC()
		cp.IntervalEnd = cp.IntervalEnd.UTC()
		cp.CreatedAtUnattested = cp.CreatedAtUnattested.UTC()

		// Recomputed on every read, exactly as the single-row path does.
		// A range read that skipped this would be a quiet way to bypass
		// the one check that catches an edited row.
		if got := attest.CheckpointHash(cp); got != cp.Hash {
			return nil, fmt.Errorf(
				"checkpoint %d was stored with hash %s but its columns now hash to %s; "+
					"the row has been altered since it was written",
				cp.Seq, cp.Hash, got)
		}
		out = append(out, AnchoredCheckpoint{Checkpoint: cp})
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

func (s *PostgresStore) listCheckpointAnchorRange(
	ctx context.Context, fromSeq, toSeq uint64,
) (map[uint64]CheckpointAnchor, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT seq, anchored_at, log_entry_id, ledger_backend
		  FROM checkpoints
		 WHERE seq >= $1 AND seq <= $2
	`, int64(fromSeq), int64(toSeq))
	if err != nil {
		return nil, fmt.Errorf("list checkpoint anchors [%d, %d]: %w", fromSeq, toSeq, err)
	}
	defer rows.Close()

	out := make(map[uint64]CheckpointAnchor)
	for rows.Next() {
		var (
			seq                       int64
			anchoredAt                sql.NullTime
			logEntryID, ledgerBackend string
		)
		if err := rows.Scan(&seq, &anchoredAt, &logEntryID, &ledgerBackend); err != nil {
			return nil, fmt.Errorf("scan checkpoint anchor: %w", err)
		}
		a := CheckpointAnchor{LogEntryID: logEntryID, LedgerBackend: ledgerBackend}
		if anchoredAt.Valid {
			a.AnchoredAt = anchoredAt.Time.UTC()
		}
		a.Anchored = a.LogEntryID != ""
		out[uint64(seq)] = a
	}
	return out, rows.Err()
}

func (s *PostgresStore) ListCheckpointLeavesRange(
	ctx context.Context, fromSeq, toSeq uint64,
) (map[uint64][]attest.TenantLeaf, error) {
	if err := ValidateCheckpointRange(fromSeq, toSeq); err != nil {
		return nil, err
	}

	rows, err := s.db.QueryContext(ctx, `
		SELECT checkpoint_seq, project_id, interval_root, execution_count,
		       cumulative_count, prev_leaf_hash, leaf_hash
		  FROM checkpoint_tenant_leaves
		 WHERE checkpoint_seq >= $1 AND checkpoint_seq <= $2
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
