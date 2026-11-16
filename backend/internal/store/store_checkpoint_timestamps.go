package store

// OpenTimestamps receipts for checkpoints (migration 064).
//
// A second public anchor, independent of Sigstore (owner decision D, 21
// October 2026). Each checkpoint hash is submitted to OpenTimestamps
// calendar servers once it is anchored. A calendar answers at once with
// a pending receipt and, a few hours later, commits it to a Bitcoin
// block; the upgrade worker then replaces the pending receipt with the
// complete one. Receipts are opaque bytes here: the OpenTimestamps
// serialisation is parsed by the verifier, not by the store.

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CheckpointTimestamp is one calendar's receipt for one checkpoint.
type CheckpointTimestamp struct {
	CheckpointSeq uint64
	CalendarURL   string
	Receipt       []byte
	Complete      bool
	SubmittedAt   time.Time
	UpgradedAt    time.Time
}

// UntimestampedCheckpoint is an anchored checkpoint with no receipt yet.
type UntimestampedCheckpoint struct {
	Seq  uint64
	Hash string
}

// CheckpointTimestampStore holds OpenTimestamps receipts.
type CheckpointTimestampStore interface {
	// RecordCheckpointTimestamp stores a calendar's first (pending)
	// receipt. A second call for the same checkpoint and calendar does
	// nothing, so a retried submission never replaces a receipt.
	RecordCheckpointTimestamp(ctx context.Context, t CheckpointTimestamp) error

	// CompleteCheckpointTimestamp replaces a pending receipt with the
	// calendar's complete one. Refuses to touch a receipt already
	// complete: a Bitcoin-attested receipt is never overwritten.
	CompleteCheckpointTimestamp(ctx context.Context, seq uint64, calendarURL string,
		receipt []byte, at time.Time) error

	// ListPendingTimestamps returns pending receipts, oldest checkpoint
	// first, at most limit of them.
	ListPendingTimestamps(ctx context.Context, limit int) ([]CheckpointTimestamp, error)

	// ListUntimestampedCheckpoints returns anchored checkpoints that no
	// calendar has a receipt for yet, oldest first, at most limit.
	ListUntimestampedCheckpoints(ctx context.Context, limit int) ([]UntimestampedCheckpoint, error)

	// ListCheckpointTimestamps returns every receipt for checkpoints in
	// [fromSeq, toSeq], keyed by checkpoint, for exports.
	ListCheckpointTimestamps(ctx context.Context, fromSeq, toSeq uint64) (map[uint64][]CheckpointTimestamp, error)
}

func (r digestRepo) recordCheckpointTimestamp(ctx context.Context, t CheckpointTimestamp) error {
	if len(t.Receipt) == 0 || t.CalendarURL == "" {
		return fmt.Errorf("record checkpoint %d timestamp: empty receipt or calendar", t.CheckpointSeq)
	}
	s, err := checkedSeq(t.CheckpointSeq)
	if err != nil {
		return err
	}
	if _, err := r.db.ExecContext(ctx, r.q(`INSERT INTO checkpoint_timestamps
		(checkpoint_seq, calendar_url, receipt, complete, submitted_at)
		VALUES (?, ?, ?, `+r.falseValue()+`, ?)
		ON CONFLICT (checkpoint_seq, calendar_url) DO NOTHING`),
		s, t.CalendarURL, t.Receipt, t.SubmittedAt.UTC()); err != nil {
		return fmt.Errorf("record checkpoint %d timestamp: %w", t.CheckpointSeq, err)
	}
	return nil
}

func (r digestRepo) completeCheckpointTimestamp(ctx context.Context, seq uint64, calendarURL string,
	receipt []byte, at time.Time) error {
	if len(receipt) == 0 {
		return fmt.Errorf("complete checkpoint %d timestamp: empty receipt", seq)
	}
	s, err := checkedSeq(seq)
	if err != nil {
		return err
	}
	res, err := r.db.ExecContext(ctx, r.q(`UPDATE checkpoint_timestamps
		SET receipt = ?, complete = `+r.trueValue()+`, upgraded_at = ?
		WHERE checkpoint_seq = ? AND calendar_url = ? AND complete = `+r.falseValue()),
		receipt, at.UTC(), s, calendarURL)
	if err != nil {
		return fmt.Errorf("complete checkpoint %d timestamp: %w", seq, err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("complete checkpoint %d timestamp rows: %w", seq, err)
	}
	if n == 0 {
		return fmt.Errorf("complete checkpoint %d timestamp from %s: no pending receipt", seq, calendarURL)
	}
	return nil
}

func (r digestRepo) listPendingTimestamps(ctx context.Context, limit int) ([]CheckpointTimestamp, error) {
	rows, err := r.db.QueryContext(ctx, r.q(`SELECT checkpoint_seq, calendar_url, receipt,
		complete, submitted_at, upgraded_at FROM checkpoint_timestamps
		WHERE complete = `+r.falseValue()+` ORDER BY checkpoint_seq, calendar_url LIMIT ?`), limit)
	if err != nil {
		return nil, fmt.Errorf("list pending timestamps: %w", err)
	}
	defer rows.Close()
	var out []CheckpointTimestamp
	for rows.Next() {
		t, err := r.scanTimestamp(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (r digestRepo) listUntimestampedCheckpoints(ctx context.Context, limit int) ([]UntimestampedCheckpoint, error) {
	rows, err := r.db.QueryContext(ctx, r.q(`SELECT c.seq, c.hash FROM checkpoints c
		WHERE c.log_entry_id <> '' AND NOT EXISTS
		(SELECT 1 FROM checkpoint_timestamps t WHERE t.checkpoint_seq = c.seq)
		ORDER BY c.seq LIMIT ?`), limit)
	if err != nil {
		return nil, fmt.Errorf("list untimestamped checkpoints: %w", err)
	}
	defer rows.Close()
	var out []UntimestampedCheckpoint
	for rows.Next() {
		var s int64
		var u UntimestampedCheckpoint
		if err := rows.Scan(&s, &u.Hash); err != nil {
			return nil, fmt.Errorf("scan untimestamped checkpoint: %w", err)
		}
		if s < 0 {
			return nil, fmt.Errorf("scan untimestamped checkpoint: negative seq %d", s)
		}
		u.Seq = uint64(s)
		out = append(out, u)
	}
	return out, rows.Err()
}

func (r digestRepo) listCheckpointTimestamps(ctx context.Context, fromSeq, toSeq uint64) (map[uint64][]CheckpointTimestamp, error) {
	from, to, err := seqRangeArgs(fromSeq, toSeq)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, r.q(`SELECT checkpoint_seq, calendar_url, receipt,
		complete, submitted_at, upgraded_at FROM checkpoint_timestamps
		WHERE checkpoint_seq >= ? AND checkpoint_seq <= ? ORDER BY checkpoint_seq, calendar_url`), from, to)
	if err != nil {
		return nil, fmt.Errorf("list checkpoint timestamps: %w", err)
	}
	defer rows.Close()
	out := make(map[uint64][]CheckpointTimestamp) // max: one entry per checkpoint in the range
	for rows.Next() {
		t, err := r.scanTimestamp(rows)
		if err != nil {
			return nil, err
		}
		out[t.CheckpointSeq] = append(out[t.CheckpointSeq], t)
	}
	return out, rows.Err()
}

func (r digestRepo) scanTimestamp(rows *sql.Rows) (CheckpointTimestamp, error) {
	var (
		t         CheckpointTimestamp
		s         int64
		complete  bool
		submitted any // any: time.Time on Postgres, text on SQLite
		upgraded  any // any: see scanStoredTime
	)
	if err := rows.Scan(&s, &t.CalendarURL, &t.Receipt, &complete, &submitted, &upgraded); err != nil {
		return t, fmt.Errorf("scan checkpoint timestamp: %w", err)
	}
	if s < 0 {
		return t, fmt.Errorf("scan checkpoint timestamp: negative seq %d", s)
	}
	t.CheckpointSeq, t.Complete = uint64(s), complete
	var err error
	if t.SubmittedAt, err = scanStoredTime("submitted_at", submitted); err != nil {
		return t, err
	}
	if upgraded != nil {
		if t.UpgradedAt, err = scanStoredTime("upgraded_at", upgraded); err != nil {
			return t, err
		}
	}
	return t, nil
}

func (r digestRepo) trueValue() string {
	if r.pg {
		return "TRUE"
	}
	return "1"
}

func (r digestRepo) falseValue() string {
	if r.pg {
		return "FALSE"
	}
	return "0"
}
