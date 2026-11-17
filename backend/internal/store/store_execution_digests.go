package store

// Stored execution digests (migration 063, 2026-11-16).
//
// Until then every execution's digest was computed while building its
// hour's tenant leaf and thrown away; exports recomputed it from the
// events that existed at export time. One late event (outside review
// the outside review of 2026-11-15) or a retention purge (E4) then made an honest export report
// tampering. Now the anchored value is stored with its checkpoint, the
// export reads it, and retention can delete the records while the chain
// still vouches for them (Robert's decisions, 2026-11-16).

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest"
	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

// ExecutionDigest is one execution's anchored digest and its place in the
// project's interval fold.
type ExecutionDigest struct {
	ExecutionID   string
	ProjectID     string
	CheckpointSeq uint64
	// Position is the execution's index in the fold that produced the
	// leaf's IntervalRoot. Stored so the export never depends on query
	// order or database collation (the outside review of 2026-11-15).
	Position  int
	Root      string
	LeafCount int
	Algorithm string
}

// LeafMissingDigests is a stored tenant leaf whose executions have no
// stored digests and no recorded history gap: one sealed before
// migration 063, waiting for BackfillDigestHistory.
type LeafMissingDigests struct {
	Seq            uint64
	ProjectID      string
	IntervalStart  time.Time
	IntervalEnd    time.Time
	IntervalRoot   string
	ExecutionCount int
}

// HistoryGapRecordsChanged is the reason recorded when an hour sealed
// before digests were stored no longer folds to its anchored value.
const HistoryGapRecordsChanged = "records changed before digests were stored"

// ExecutionDigestStore is the persistence for stored digests. Part of
// Store; split out so the contract reads in one place.
type ExecutionDigestStore interface {
	// InsertCheckpointWithDigests is InsertCheckpoint plus every leaf's
	// execution digests, in one transaction. Refuses digests that do not
	// fold exactly to their leaf, so a stored digest can never disagree
	// with what was anchored.
	InsertCheckpointWithDigests(ctx context.Context, cp attest.Checkpoint, leaves []attest.TenantLeaf, digests map[string][]ExecutionDigest) error

	// ListExecutionDigests returns one project's digests for one
	// checkpoint, in fold order.
	ListExecutionDigests(ctx context.Context, projectID string, seq uint64) ([]ExecutionDigest, error)

	// ListLeavesMissingDigests returns up to limit leaves with neither
	// stored digests nor a recorded history gap, oldest first.
	ListLeavesMissingDigests(ctx context.Context, limit int) ([]LeafMissingDigests, error)

	// StoreHistoricalDigests stores digests recomputed for a leaf sealed
	// before migration 063. Refuses them unless they fold exactly to the
	// stored leaf: history is preserved, never re-anchored.
	StoreHistoricalDigests(ctx context.Context, seq uint64, projectID string, digests []ExecutionDigest) error

	// RecordDigestHistoryGap labels a leaf whose history can no longer
	// be reconstructed. Idempotent.
	RecordDigestHistoryGap(ctx context.Context, seq uint64, projectID, reason string, at time.Time) error

	// GetDigestHistoryGap returns the recorded reason, if any.
	GetDigestHistoryGap(ctx context.Context, seq uint64, projectID string) (reason string, found bool, err error)

	// ExistingExecutionIDs reports which of ids still have an execution
	// row (the rest were deleted under retention).
	ExistingExecutionIDs(ctx context.Context, ids []string) (map[string]bool, error)

	// ListDigestEventsForExecution returns the events a digest covers:
	// every event except those that arrived after the execution's
	// digest was stored.
	ListDigestEventsForExecution(ctx context.Context, executionID string) ([]*events.Event, error)

	// CountEventsAfterDigest returns, for each of the given executions
	// that has any, how many of its events arrived after its digest was
	// stored. Executions with none are absent from the map. One query per
	// 500 ids, so ingest asks once per batch rather than once per run.
	CountEventsAfterDigest(ctx context.Context, executionIDs []string) (map[string]int, error)

	// ListReportChainStatuses returns the recorded report-chain verdict
	// of each given run that has one, for the hourly export.
	ListReportChainStatuses(ctx context.Context, executionIDs []string) (map[string]string, error)
}

// validateDigestsFold checks that digests fold exactly to the leaf: same
// count, positions 0..n-1, every row naming the leaf's project and
// checkpoint, and RootOverExecutionDigests over the roots in position
// order equal to the leaf's IntervalRoot.
func validateDigestsFold(seq uint64, projectID, intervalRoot string, executionCount int, digests []ExecutionDigest) error {
	if len(digests) != executionCount {
		return fmt.Errorf("checkpoint %d project %s: %d digests for a leaf that counts %d executions",
			seq, projectID, len(digests), executionCount)
	}
	roots := make([]string, len(digests))
	for i, d := range digests {
		if d.Position != i {
			return fmt.Errorf("checkpoint %d project %s: digest %d has position %d", seq, projectID, i, d.Position)
		}
		if d.ProjectID != projectID || d.CheckpointSeq != seq {
			return fmt.Errorf("checkpoint %d project %s: digest for %s names project %s checkpoint %d",
				seq, projectID, d.ExecutionID, d.ProjectID, d.CheckpointSeq)
		}
		roots[i] = d.Root
	}
	got, err := attest.RootOverExecutionDigests(roots)
	if err != nil {
		return fmt.Errorf("checkpoint %d project %s: fold digests: %w", seq, projectID, err)
	}
	if got != intervalRoot {
		return fmt.Errorf("checkpoint %d project %s: digests fold to %s but the leaf commits to %s",
			seq, projectID, got, intervalRoot)
	}
	return nil
}

// ── Shared implementation ─────────────────────────────────────────────
//
// One body for both databases, so the two cannot drift (outside review
// the outside review of 2026-11-15 is what drift between them cost). The dialect differences
// are placeholders and the boolean column.

type digestRepo struct {
	db *sql.DB
	pg bool
}

// q rewrites ? placeholders as $1, $2 ... for Postgres.
func (r digestRepo) q(query string) string {
	if !r.pg {
		return query
	}
	var b strings.Builder
	n := 0
	for _, c := range query {
		if c == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
			continue
		}
		b.WriteRune(c)
	}
	return b.String()
}

func (r digestRepo) lateFalse() string {
	if r.pg {
		return "NOT after_digest"
	}
	return "after_digest = 0"
}

func (r digestRepo) lateTrue() string {
	if r.pg {
		return "after_digest"
	}
	return "after_digest = 1"
}

// insertExecutionDigestsTx writes every leaf's digests inside the
// checkpoint's transaction. A nil map writes nothing.
func insertExecutionDigestsTx(ctx context.Context, tx *sql.Tx, insert string,
	leaves []attest.TenantLeaf, digests map[string][]ExecutionDigest, now time.Time) error {
	if digests == nil {
		return nil
	}
	for _, l := range leaves {
		for _, d := range digests[l.ProjectID] {
			if _, err := tx.ExecContext(ctx, insert,
				d.ExecutionID, d.ProjectID, int64(d.CheckpointSeq), d.Position,
				d.Root, d.LeafCount, d.Algorithm, now); err != nil {
				return fmt.Errorf("store digest of %s: %w", d.ExecutionID, err)
			}
		}
	}
	return nil
}

func (r digestRepo) listExecutionDigests(ctx context.Context, projectID string, seq uint64) ([]ExecutionDigest, error) {
	rows, err := r.db.QueryContext(ctx, r.q(`
		SELECT execution_id, project_id, checkpoint_seq, position, root, leaf_count, algorithm
		  FROM execution_digests
		 WHERE project_id = ? AND checkpoint_seq = ?
		 ORDER BY position`), projectID, int64(seq))
	if err != nil {
		return nil, fmt.Errorf("list execution digests: %w", err)
	}
	defer rows.Close()
	var out []ExecutionDigest
	for rows.Next() {
		var d ExecutionDigest
		var s int64
		if err := rows.Scan(&d.ExecutionID, &d.ProjectID, &s, &d.Position, &d.Root, &d.LeafCount, &d.Algorithm); err != nil {
			return nil, fmt.Errorf("scan execution digest: %w", err)
		}
		d.CheckpointSeq = uint64(s)
		out = append(out, d)
	}
	return out, rows.Err()
}

// scanStoredTime accepts the two shapes the drivers return: time.Time
// (Postgres) or text (SQLite).
func scanStoredTime(field string, v any) (time.Time, error) { // any: time.Time on Postgres, text on SQLite
	switch t := v.(type) {
	case time.Time:
		return t.UTC(), nil
	case string:
		return mustParseStoredTime(field, t)
	case []byte:
		return mustParseStoredTime(field, string(t))
	default:
		return time.Time{}, fmt.Errorf("%s: unexpected stored type %T", field, v)
	}
}

func (r digestRepo) listLeavesMissingDigests(ctx context.Context, limit int) ([]LeafMissingDigests, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := r.db.QueryContext(ctx, r.q(`
		SELECT l.checkpoint_seq, l.project_id, c.interval_start, c.interval_end,
		       l.interval_root, l.execution_count
		  FROM checkpoint_tenant_leaves l
		  JOIN checkpoints c ON c.seq = l.checkpoint_seq
		 WHERE NOT EXISTS (SELECT 1 FROM execution_digests d
		                    WHERE d.checkpoint_seq = l.checkpoint_seq AND d.project_id = l.project_id)
		   AND NOT EXISTS (SELECT 1 FROM digest_history_gaps g
		                    WHERE g.checkpoint_seq = l.checkpoint_seq AND g.project_id = l.project_id)
		 ORDER BY l.checkpoint_seq, l.position
		 LIMIT ?`), limit)
	if err != nil {
		return nil, fmt.Errorf("list leaves missing digests: %w", err)
	}
	defer rows.Close()
	var out []LeafMissingDigests
	for rows.Next() {
		var (
			l          LeafMissingDigests
			seq        int64
			start, end any // any: see scanStoredTime
		)
		if err := rows.Scan(&seq, &l.ProjectID, &start, &end, &l.IntervalRoot, &l.ExecutionCount); err != nil {
			return nil, fmt.Errorf("scan leaf missing digests: %w", err)
		}
		l.Seq = uint64(seq)
		if l.IntervalStart, err = scanStoredTime("interval_start", start); err != nil {
			return nil, err
		}
		if l.IntervalEnd, err = scanStoredTime("interval_end", end); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (r digestRepo) storeHistoricalDigests(ctx context.Context, seq uint64, projectID string, digests []ExecutionDigest) error {
	var root string
	var count int
	err := r.db.QueryRowContext(ctx, r.q(`
		SELECT interval_root, execution_count FROM checkpoint_tenant_leaves
		 WHERE checkpoint_seq = ? AND project_id = ?`), int64(seq), projectID).Scan(&root, &count)
	if errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("store historical digests: checkpoint %d has no leaf for %s", seq, projectID)
	}
	if err != nil {
		return fmt.Errorf("store historical digests: read leaf: %w", err)
	}
	if err := validateDigestsFold(seq, projectID, root, count, digests); err != nil {
		return fmt.Errorf("store historical digests: %w", err)
	}
	tx, err := r.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("store historical digests: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	leaf := []attest.TenantLeaf{{ProjectID: projectID}}
	if err := insertExecutionDigestsTx(ctx, tx, r.q(`INSERT INTO execution_digests
		(execution_id, project_id, checkpoint_seq, position, root, leaf_count, algorithm, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`), leaf, map[string][]ExecutionDigest{projectID: digests}, time.Now().UTC()); err != nil {
		return fmt.Errorf("store historical digests: %w", err)
	}
	return tx.Commit()
}

func (r digestRepo) recordDigestHistoryGap(ctx context.Context, seq uint64, projectID, reason string, at time.Time) error {
	conflict := "ON CONFLICT (checkpoint_seq, project_id) DO NOTHING"
	if _, err := r.db.ExecContext(ctx, r.q(`INSERT INTO digest_history_gaps
		(checkpoint_seq, project_id, reason, detected_at) VALUES (?, ?, ?, ?) `+conflict),
		int64(seq), projectID, reason, at.UTC()); err != nil {
		return fmt.Errorf("record digest history gap: %w", err)
	}
	return nil
}

func (r digestRepo) getDigestHistoryGap(ctx context.Context, seq uint64, projectID string) (string, bool, error) {
	var reason string
	err := r.db.QueryRowContext(ctx, r.q(`SELECT reason FROM digest_history_gaps
		WHERE checkpoint_seq = ? AND project_id = ?`), int64(seq), projectID).Scan(&reason)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, fmt.Errorf("get digest history gap: %w", err)
	}
	return reason, true, nil
}

// idsParam returns an IN (...) subquery over one JSON array parameter
// and that parameter. One bound value whatever the number of ids, so a
// long list never reaches either database's parameter limit and needs no
// chunked loop of queries.
func (r digestRepo) idsParam(ids []string) (string, string, error) {
	if ids == nil {
		ids = []string{}
	}
	raw, err := json.Marshal(ids)
	if err != nil {
		return "", "", fmt.Errorf("encode ids: %w", err)
	}
	if r.pg {
		return "SELECT json_array_elements_text(?::json)", string(raw), nil
	}
	return "SELECT value FROM json_each(?)", string(raw), nil
}

func (r digestRepo) existingExecutionIDs(ctx context.Context, ids []string) (map[string]bool, error) {
	sub, param, err := r.idsParam(ids)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, r.q(`SELECT execution_id FROM executions
		WHERE execution_id IN (`+sub+`)`), param)
	if err != nil {
		return nil, fmt.Errorf("existing execution ids: %w", err)
	}
	defer rows.Close()
	out := make(map[string]bool, len(ids)) // max: one entry per id asked about
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("existing execution ids scan: %w", err)
		}
		out[id] = true
	}
	return out, rows.Err()
}

func (r digestRepo) countEventsAfterDigest(ctx context.Context, ids []string) (map[string]int, error) {
	sub, param, err := r.idsParam(ids)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, r.q(`SELECT execution_id, COUNT(*) FROM events
		WHERE execution_id IN (`+sub+`) AND `+r.lateTrue()+`
		GROUP BY execution_id`), param)
	if err != nil {
		return nil, fmt.Errorf("count events after digest: %w", err)
	}
	defer rows.Close()
	out := make(map[string]int) // max: one entry per id asked about
	for rows.Next() {
		var id string
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, fmt.Errorf("count events after digest scan: %w", err)
		}
		out[id] = n
	}
	return out, rows.Err()
}

// listReportChainStatuses returns each given run's recorded chain
// verdict (migration 067). Runs with none recorded are absent.
func (r digestRepo) listReportChainStatuses(ctx context.Context, ids []string) (map[string]string, error) {
	sub, param, err := r.idsParam(ids)
	if err != nil {
		return nil, err
	}
	rows, err := r.db.QueryContext(ctx, r.q(`SELECT execution_id, status FROM report_chain_status
		WHERE execution_id IN (`+sub+`)`), param)
	if err != nil {
		return nil, fmt.Errorf("list report chain statuses: %w", err)
	}
	defer rows.Close()
	out := make(map[string]string) // max: one entry per id asked about
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			return nil, fmt.Errorf("report chain status scan: %w", err)
		}
		out[id] = status
	}
	return out, rows.Err()
}
