package store

// Checkpoint chain persistence, Postgres implementation (migration
// 058). Twin of sqlite_checkpoints.go, read that file for the
// reasoning behind each decision.
//
// ONE REAL DIALECT DIFFERENCE, NOT JUST PLACEHOLDERS
//
// SQLite stores these timestamps as TEXT and hands them back as
// strings, so that twin parses them. pgx returns TIMESTAMPTZ as a
// genuine time.Time, so this file scans directly into time.Time and
// needs no parsing step. Copying the SQLite string-parsing here would
// fail at runtime, and copying this file's direct scan into the SQLite
// twin would fail there, which is exactly why these are two files and
// not one with a dialect flag.
//
// Both stores MUST carry this. Shipping one and not the other is what
// caused the migration-056 production outage.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"mesedi/backend/internal/attest"
)

// InsertCheckpoint writes a checkpoint and its tenant leaves in one
// transaction. Leaves without their checkpoint, or the reverse, reads
// as corruption to a verifier rather than as a crash.
func (s *PostgresStore) InsertCheckpoint(
	ctx context.Context,
	cp attest.Checkpoint,
	leaves []attest.TenantLeaf,
) error {
	if cp.Seq == 0 {
		return errors.New("insert checkpoint: seq must be >= 1")
	}
	if len(leaves) != cp.TenantLeafCount {
		return fmt.Errorf(
			"insert checkpoint %d: %d leaves supplied but the checkpoint commits to %d; "+
				"storing a different number than the anchored root covers would make "+
				"the tree unreconstructable",
			cp.Seq, len(leaves), cp.TenantLeafCount)
	}
	if got := attest.CheckpointHash(cp); got != cp.Hash {
		return fmt.Errorf("insert checkpoint %d: supplied hash %s does not match "+
			"its contents, which hash to %s", cp.Seq, cp.Hash, got)
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("insert checkpoint: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO checkpoints (
			seq, format, prev_checkpoint_hash, prev_log_entry_id,
			interval_start, interval_end, tenant_leaf_count, merkle_root,
			cumulative_count, created_at_unattested, hash
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
	`,
		int64(cp.Seq), cp.Format, cp.PrevCheckpointHash, cp.PrevLogEntryID,
		cp.IntervalStart.UTC(), cp.IntervalEnd.UTC(), cp.TenantLeafCount,
		cp.MerkleRoot, int64(cp.CumulativeCount), cp.CreatedAtUnattested.UTC(),
		cp.Hash,
	); err != nil {
		return fmt.Errorf("insert checkpoint %d: %w", cp.Seq, err)
	}

	for i, l := range leaves {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO checkpoint_tenant_leaves (
				checkpoint_seq, project_id, position, interval_root,
				execution_count, cumulative_count, prev_leaf_hash, leaf_hash
			) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		`,
			int64(cp.Seq), l.ProjectID, i, l.IntervalRoot,
			l.ExecutionCount, int64(l.CumulativeCount), l.PrevLeafHash,
			attest.TenantLeafHash(l),
		); err != nil {
			return fmt.Errorf("insert checkpoint %d leaf %d (%s): %w",
				cp.Seq, i, l.ProjectID, err)
		}
	}
	return tx.Commit()
}

// LatestCheckpoint returns the highest-sequence checkpoint, or
// (nil, nil) before genesis. The stored hash is recomputed and compared
// so an edited row is detected here, distinctly from the chain simply
// failing to verify later.
func (s *PostgresStore) LatestCheckpoint(ctx context.Context) (*attest.Checkpoint, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT seq, format, prev_checkpoint_hash, prev_log_entry_id,
		       interval_start, interval_end, tenant_leaf_count, merkle_root,
		       cumulative_count, created_at_unattested, hash
		  FROM checkpoints
		 ORDER BY seq DESC
		 LIMIT 1
	`)
	cp, err := scanPGCheckpointRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return cp, err
}

// GetCheckpoint returns one checkpoint by sequence, or (nil, nil).
func (s *PostgresStore) GetCheckpoint(ctx context.Context, seq uint64) (*attest.Checkpoint, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT seq, format, prev_checkpoint_hash, prev_log_entry_id,
		       interval_start, interval_end, tenant_leaf_count, merkle_root,
		       cumulative_count, created_at_unattested, hash
		  FROM checkpoints WHERE seq = $1
	`, int64(seq))
	cp, err := scanPGCheckpointRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	return cp, err
}

// scanPGCheckpointRow scans directly into time.Time. See the file
// header: pgx returns TIMESTAMPTZ as time.Time, unlike the SQLite
// driver which returns a string.
func scanPGCheckpointRow(row *sql.Row) (*attest.Checkpoint, error) {
	var (
		cp              attest.Checkpoint
		seq, cumulative int64
	)
	if err := row.Scan(
		&seq, &cp.Format, &cp.PrevCheckpointHash, &cp.PrevLogEntryID,
		&cp.IntervalStart, &cp.IntervalEnd, &cp.TenantLeafCount, &cp.MerkleRoot,
		&cumulative, &cp.CreatedAtUnattested, &cp.Hash,
	); err != nil {
		return nil, err
	}
	cp.Seq = uint64(seq)
	cp.CumulativeCount = uint64(cumulative)
	// Normalise to UTC before hashing. The hash preimage formats times
	// as UTC, so a row returned in another zone would still hash the
	// same, but normalising here keeps the loaded value identical to
	// what was stored rather than merely hash-equivalent.
	cp.IntervalStart = cp.IntervalStart.UTC()
	cp.IntervalEnd = cp.IntervalEnd.UTC()
	cp.CreatedAtUnattested = cp.CreatedAtUnattested.UTC()

	if got := attest.CheckpointHash(cp); got != cp.Hash {
		return nil, fmt.Errorf(
			"checkpoint %d was stored with hash %s but its columns now hash to %s; "+
				"the row has been altered since it was written",
			cp.Seq, cp.Hash, got)
	}
	return &cp, nil
}

// GetCheckpointLeaves returns one interval's leaves in the order they
// were hashed. ORDER BY position: the root depends on leaf order and
// SELECT row order is not a contract.
//
// CROSS-TENANT BY DESIGN, INTERNAL USE ONLY. Returns every project's
// leaf for the interval, because that is the set the root was computed
// over and a tree cannot be rebuilt from a subset. Never hand the
// result to a tenant-facing response: on a DoD deployment the volume of
// a programme's agent activity can be sensitive regardless of what the
// agents were doing.
//
// Each leaf is verified against its stored leaf_hash before return. A
// stored hash nobody recomputes is decoration, and the layer that reads
// the row is the layer that should check it rather than hoping a caller
// remembers to.
func (s *PostgresStore) GetCheckpointLeaves(
	ctx context.Context, seq uint64,
) ([]attest.TenantLeaf, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT project_id, interval_root, execution_count,
		       cumulative_count, prev_leaf_hash, leaf_hash
		  FROM checkpoint_tenant_leaves
		 WHERE checkpoint_seq = $1
		 ORDER BY position
	`, int64(seq))
	if err != nil {
		return nil, fmt.Errorf("get checkpoint %d leaves: %w", seq, err)
	}
	defer rows.Close()

	var out []attest.TenantLeaf
	for rows.Next() {
		var (
			l          attest.TenantLeaf
			cumulative int64
			storedHash string
		)
		if err := rows.Scan(&l.ProjectID, &l.IntervalRoot, &l.ExecutionCount,
			&cumulative, &l.PrevLeafHash, &storedHash); err != nil {
			return nil, fmt.Errorf("scan checkpoint %d leaf: %w", seq, err)
		}
		l.CumulativeCount = uint64(cumulative)
		if got := attest.TenantLeafHash(l); got != storedHash {
			return nil, fmt.Errorf(
				"checkpoint %d leaf for project %q was stored with hash %s but its "+
					"columns now hash to %s; the row has been altered since it was "+
					"written", seq, l.ProjectID, storedHash, got)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// LatestTenantLeaf returns a project's most recent leaf, or (nil, nil)
// if it has never appeared. Nil means "first leaf", whose predecessor
// is attest.ZeroHash.
func (s *PostgresStore) LatestTenantLeaf(
	ctx context.Context, projectID string,
) (*attest.TenantLeaf, error) {
	if projectID == "" {
		return nil, errors.New("latest tenant leaf: empty project_id")
	}
	var (
		l          attest.TenantLeaf
		cumulative int64
		storedHash string
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT project_id, interval_root, execution_count,
		       cumulative_count, prev_leaf_hash, leaf_hash
		  FROM checkpoint_tenant_leaves
		 WHERE project_id = $1
		 ORDER BY checkpoint_seq DESC
		 LIMIT 1
	`, projectID).Scan(&l.ProjectID, &l.IntervalRoot, &l.ExecutionCount,
		&cumulative, &l.PrevLeafHash, &storedHash)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("latest tenant leaf %s: %w", projectID, err)
	}
	l.CumulativeCount = uint64(cumulative)
	// Verified because this value feeds the NEXT leaf's PrevLeafHash: an
	// altered row here silently corrupts every leaf that follows, and
	// the damage surfaces much later as an unexplained sub-chain break.
	if got := attest.TenantLeafHash(l); got != storedHash {
		return nil, fmt.Errorf(
			"latest leaf for project %q was stored with hash %s but now hashes to "+
				"%s; the row has been altered, and building the next leaf on it "+
				"would corrupt every leaf that follows",
			projectID, storedHash, got)
	}
	return &l, nil
}

// GetCheckpointAnchor reports where a checkpoint reached the log.
// Anchored=false for a built-but-unsubmitted checkpoint, which is a
// resumable state rather than an error; ErrNotFound if the checkpoint
// does not exist, because that is a different question.
//
// Scans anchored_at into sql.NullTime rather than a string: pgx returns
// TIMESTAMPTZ as a real time.Time. The SQLite twin cannot do this. See
// that file's header.
func (s *PostgresStore) GetCheckpointAnchor(
	ctx context.Context, seq uint64,
) (CheckpointAnchor, error) {
	var (
		a          CheckpointAnchor
		anchoredAt sql.NullTime
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT anchored_at, log_entry_id, ledger_backend, leaf_preimage,
		       anchor_proof_json
		  FROM checkpoints WHERE seq = $1
	`, int64(seq)).Scan(&anchoredAt, &a.LogEntryID, &a.LedgerBackend, &a.LeafPreimage,
		&a.AnchorProofJSON)
	if errors.Is(err, sql.ErrNoRows) {
		return CheckpointAnchor{}, ErrNotFound
	}
	if err != nil {
		return CheckpointAnchor{}, fmt.Errorf("get checkpoint %d anchor: %w", seq, err)
	}
	if anchoredAt.Valid {
		a.AnchoredAt = anchoredAt.Time.UTC()
	}
	// Driven by the log entry id, not the timestamp: an anchor with no
	// entry to point at cannot be verified, so it is not an anchor.
	a.Anchored = a.LogEntryID != ""
	return a, nil
}

// MarkCheckpointAnchored records where a checkpoint reached the
// transparency log. Refuses to overwrite an existing anchor: two log
// entries for one checkpoint is evidence, and keeping only the second
// would discard it.
func (s *PostgresStore) MarkCheckpointAnchored(
	ctx context.Context,
	seq uint64,
	a CheckpointAnchor,
	anchoredAt time.Time,
) error {
	if a.LogEntryID == "" {
		return fmt.Errorf("mark checkpoint %d anchored: empty log entry id; "+
			"an anchor with no entry to point at cannot be verified", seq)
	}
	// Not required here; see the SQLite twin for why the caller rather
	// than the store is the right place to insist on a preimage.
	res, err := s.db.ExecContext(ctx, `
		UPDATE checkpoints
		   SET anchored_at = $1, log_entry_id = $2, ledger_backend = $3,
		       leaf_preimage = $4, anchor_proof_json = $5
		 WHERE seq = $6 AND anchored_at IS NULL
	`, anchoredAt.UTC(), a.LogEntryID, a.LedgerBackend, a.LeafPreimage,
		a.AnchorProofJSON, int64(seq))
	if err != nil {
		return fmt.Errorf("mark checkpoint %d anchored: %w", seq, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("mark checkpoint %d anchored rows: %w", seq, err)
	}
	if n == 0 {
		return fmt.Errorf("mark checkpoint %d anchored: no unanchored checkpoint "+
			"with that sequence (already anchored, or does not exist)", seq)
	}
	return nil
}
