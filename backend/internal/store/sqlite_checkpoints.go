package store

// Checkpoint chain persistence, SQLite implementation (migration 058).
// Twin: postgres_checkpoints.go. Both stores MUST carry this; shipping
// one and not the other is what caused the migration-056 outage.
//
// TIME HANDLING IS THE SUBTLE PART OF THIS FILE
//
// This store binds time.Time for writes and the driver stores it as
// RFC3339Nano TEXT; reads come back as a STRING, and scanning a
// timestamp column into sql.NullTime fails outright ("unsupported Scan,
// storing driver.Value type string into type *time.Time"). So reads go
// through a string.
//
// They do NOT go through parseFlexTime, which the rest of the package
// uses. parseFlexTime returns the zero time on a parse failure, without
// an error. For a display field that is a reasonable trade. For a field
// that feeds a HASH it is not: a silently-zeroed interval_start would
// change what the checkpoint recomputes to, and the chain verifier
// would report tampering on a row nobody touched, pointing an auditor
// at a crime that did not happen. Hash-committed timestamps are parsed
// strictly and a failure is returned as an error.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"mesedi/backend/internal/attest"
)

// mustParseStoredTime parses a timestamp that participates in a hash.
// Strict on purpose — see the file header.
func mustParseStoredTime(field, s string) (time.Time, error) {
	if s == "" {
		return time.Time{}, fmt.Errorf(
			"checkpoint %s is empty; a hash-committed timestamp cannot be defaulted", field)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err == nil {
		return t.UTC(), nil
	}
	// The driver has been seen to write without the T separator.
	if t, err2 := time.Parse("2006-01-02 15:04:05.999999999-07:00", s); err2 == nil {
		return t.UTC(), nil
	}
	return time.Time{}, fmt.Errorf("checkpoint %s %q is not a parseable timestamp: %w",
		field, s, err)
}

// InsertCheckpoint writes a checkpoint and its tenant leaves atomically.
//
// One transaction, and that is the whole point. Leaves without their
// checkpoint, or a checkpoint without its leaves, is a chain that reads
// as corrupt to a verifier rather than as a crash — and the difference
// between "this system crashed" and "this record was altered" is the
// only thing this mechanism sells.
//
// Re-inserting an existing seq FAILS rather than overwriting. A
// checkpoint is an anchored fact; silently replacing one would rewrite
// history, which is the attack, not a convenience.
func (s *SQLiteStore) InsertCheckpoint(
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
	// Recompute rather than trust the caller's Hash field. A stored hash
	// that does not describe its own row is worse than no hash: every
	// later check compares against it.
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
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
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
			) VALUES (?, ?, ?, ?, ?, ?, ?, ?)
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
// (nil, nil) when the chain has not started.
//
// Nil rather than an error for an empty table: before genesis, having
// no checkpoint is the correct state, not a failure. Treating it as an
// error would make the scheduler's first run look broken.
//
// The stored hash is RECOMPUTED and compared before returning. Nothing
// downstream trusts the stored value, so the only thing storing it buys
// is exactly this: the ability to notice that a row was edited
// underneath us, as a distinct event from the chain failing to verify
// later.
func (s *SQLiteStore) LatestCheckpoint(ctx context.Context) (*attest.Checkpoint, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT seq, format, prev_checkpoint_hash, prev_log_entry_id,
		       interval_start, interval_end, tenant_leaf_count, merkle_root,
		       cumulative_count, created_at_unattested, hash
		  FROM checkpoints
		 ORDER BY seq DESC
		 LIMIT 1
	`)
	cp, err := scanCheckpointRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cp, nil
}

// GetCheckpoint returns one checkpoint by sequence, or (nil, nil).
func (s *SQLiteStore) GetCheckpoint(ctx context.Context, seq uint64) (*attest.Checkpoint, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT seq, format, prev_checkpoint_hash, prev_log_entry_id,
		       interval_start, interval_end, tenant_leaf_count, merkle_root,
		       cumulative_count, created_at_unattested, hash
		  FROM checkpoints WHERE seq = ?
	`, int64(seq))
	cp, err := scanCheckpointRow(row)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return cp, nil
}

func scanCheckpointRow(row *sql.Row) (*attest.Checkpoint, error) {
	var (
		cp                           attest.Checkpoint
		seq, cumulative              int64
		startStr, endStr, createdStr string
	)
	if err := row.Scan(
		&seq, &cp.Format, &cp.PrevCheckpointHash, &cp.PrevLogEntryID,
		&startStr, &endStr, &cp.TenantLeafCount, &cp.MerkleRoot,
		&cumulative, &createdStr, &cp.Hash,
	); err != nil {
		return nil, err
	}
	cp.Seq = uint64(seq)
	cp.CumulativeCount = uint64(cumulative)

	var err error
	if cp.IntervalStart, err = mustParseStoredTime("interval_start", startStr); err != nil {
		return nil, err
	}
	if cp.IntervalEnd, err = mustParseStoredTime("interval_end", endStr); err != nil {
		return nil, err
	}
	if cp.CreatedAtUnattested, err = mustParseStoredTime("created_at_unattested", createdStr); err != nil {
		return nil, err
	}

	if got := attest.CheckpointHash(cp); got != cp.Hash {
		return nil, fmt.Errorf(
			"checkpoint %d was stored with hash %s but its columns now hash to %s; "+
				"the row has been altered since it was written",
			cp.Seq, cp.Hash, got)
	}
	return &cp, nil
}

// GetCheckpointLeaves returns one interval's leaves IN THE ORDER THEY
// WERE HASHED.
//
// ORDER BY position, not by project_id and not unordered. The interval
// root depends on leaf order, and row order from a SELECT is not a
// contract — it can differ between SQLite and Postgres, and between two
// runs on the same engine. Reconstructing the tree in a different order
// computes a different root than the one anchored, and a verifier
// reports tampering on data nobody touched.
//
// CROSS-TENANT BY DESIGN — INTERNAL USE ONLY.
//
// This returns EVERY project's leaf for the interval, including their
// execution counts and running totals. It has to: that is the set the
// interval root was computed over, and a tree cannot be reconstructed
// from a subset. But it means the result must never be handed to a
// tenant-facing response. One customer learning another's activity
// volume is a disclosure, and on a DoD deployment the volume of a
// programme's agent activity can itself be sensitive regardless of what
// the agents were doing. Anything customer-facing must go through a
// project-scoped path.
//
// Every leaf is verified against its stored leaf_hash before being
// returned, for the same reason the checkpoint's own hash is
// recomputed on load: a stored hash nobody recomputes is decoration.
// Without this, a row edited directly in the database would be returned
// clean and would only be caught later, if the caller happened to run
// VerifyIntervalLeaves. Defence in depth means the layer that reads the
// row is the layer that checks it.
func (s *SQLiteStore) GetCheckpointLeaves(
	ctx context.Context, seq uint64,
) ([]attest.TenantLeaf, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT project_id, interval_root, execution_count,
		       cumulative_count, prev_leaf_hash, leaf_hash
		  FROM checkpoint_tenant_leaves
		 WHERE checkpoint_seq = ?
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
// if it has never appeared in a checkpoint.
//
// This is what the next interval's leaf needs for its PrevLeafHash and
// its running total. Nil means "this project's first leaf", whose
// predecessor is attest.ZeroHash.
//
// Scoped to one project, and the scope is in the WHERE clause rather
// than applied after the read — the same discipline as the attestation
// reads in verdifax-orchestrator. Verified against its stored hash for
// the same reason as GetCheckpointLeaves: this value feeds the NEXT
// leaf's PrevLeafHash, so an altered row here would silently corrupt
// every leaf that follows it, and the corruption would surface much
// later as an unexplained break in the project's sub-chain.
func (s *SQLiteStore) LatestTenantLeaf(
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
		 WHERE project_id = ?
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
//
// Anchored=false for a checkpoint that exists but has not been
// submitted yet, which is a resumable state rather than an error. A
// checkpoint that does not exist at all returns ErrNotFound, because
// asking about the anchor of something that was never built is a
// different question with a different answer.
func (s *SQLiteStore) GetCheckpointAnchor(
	ctx context.Context, seq uint64,
) (CheckpointAnchor, error) {
	var (
		a          CheckpointAnchor
		anchoredAt sql.NullString
	)
	err := s.db.QueryRowContext(ctx, `
		SELECT anchored_at, log_entry_id, ledger_backend
		  FROM checkpoints WHERE seq = ?
	`, int64(seq)).Scan(&anchoredAt, &a.LogEntryID, &a.LedgerBackend)
	if errors.Is(err, sql.ErrNoRows) {
		return CheckpointAnchor{}, ErrNotFound
	}
	if err != nil {
		return CheckpointAnchor{}, fmt.Errorf("get checkpoint %d anchor: %w", seq, err)
	}
	if anchoredAt.Valid && anchoredAt.String != "" {
		// Not parsed strictly: anchored_at is operational metadata, not
		// part of any hash, so a malformed value here cannot change what
		// a checkpoint verifies to. Anchored is driven by the log entry
		// id being present rather than by the timestamp parsing, so a
		// bad timestamp degrades the display and not the chain.
		a.AnchoredAt = parseFlexTime(anchoredAt.String)
	}
	a.Anchored = a.LogEntryID != ""
	return a, nil
}

// MarkCheckpointAnchored records where a checkpoint reached the
// transparency log.
//
// The log entry id is what the NEXT checkpoint names as its
// PrevLogEntryID, so this is the step that stitches the chain to a log
// Mesedi does not control. Refuses to overwrite an existing anchor:
// a checkpoint that reached the log twice has two entries, and quietly
// keeping only the second would discard evidence.
func (s *SQLiteStore) MarkCheckpointAnchored(
	ctx context.Context,
	seq uint64,
	logEntryID, ledgerBackend string,
	anchoredAt time.Time,
) error {
	if logEntryID == "" {
		return fmt.Errorf("mark checkpoint %d anchored: empty log entry id; "+
			"an anchor with no entry to point at cannot be verified", seq)
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE checkpoints
		   SET anchored_at = ?, log_entry_id = ?, ledger_backend = ?
		 WHERE seq = ? AND anchored_at IS NULL
	`, anchoredAt.UTC(), logEntryID, ledgerBackend, int64(seq))
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
