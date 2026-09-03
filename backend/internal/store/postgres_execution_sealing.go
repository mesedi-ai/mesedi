package store

// Execution sealing for the checkpoint chain, Postgres implementation
// (migration 057). Twin of sqlite_execution_sealing.go — read that file
// for the full reasoning behind each decision. Same semantics, $N
// placeholders instead of ?.
//
// This file existing is not optional. readiness.SchemaStatus counts
// embedded migrations per dialect, and implementing a store feature on
// one backend only is what caused the migration-056 production outage.
// A self-hoster on SQLite and a customer on Postgres must get the same
// chain, or the chain means something different depending on where it
// runs.

import (
	"context"
	"fmt"
	"time"
)

// SealExecutions marks eligible executions with sealed_at = now.
// Settled (ended and past the grace period) or timed out (never ended,
// started long enough ago). Idempotent: rows already sealed are
// excluded, so interval membership is recorded once and never
// recomputed.
func (s *PostgresStore) SealExecutions(
	ctx context.Context,
	now time.Time,
	settle time.Duration,
	timeout time.Duration,
) (int64, error) {
	if settle < 0 || timeout <= 0 {
		return 0, fmt.Errorf(
			"seal executions: settle must be >= 0 and timeout > 0, got settle=%s timeout=%s",
			settle, timeout)
	}
	now = now.UTC()
	settledBefore := now.Add(-settle)
	startedBefore := now.Add(-timeout)

	res, err := s.db.ExecContext(ctx, `
		UPDATE executions
		   SET sealed_at = $1
		 WHERE sealed_at IS NULL
		   AND (
		         (ended_at IS NOT NULL AND ended_at <= $2)
		      OR (ended_at IS NULL     AND started_at <= $3)
		       )
	`, now, settledBefore, startedBefore)
	if err != nil {
		return 0, fmt.Errorf("seal executions: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("seal executions rows affected: %w", err)
	}
	return n, nil
}

// CountSealedByProject returns per-project counts of executions sealed
// in [from, to). Half-open so an execution sealed exactly on an hour
// boundary is counted in one interval, not two.
func (s *PostgresStore) CountSealedByProject(
	ctx context.Context,
	from, to time.Time,
) (map[string]int, error) {
	if !to.After(from) {
		return nil, fmt.Errorf("count sealed: to (%s) must be after from (%s)",
			to.UTC(), from.UTC())
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT project_id, COUNT(*)
		  FROM executions
		 WHERE sealed_at >= $1 AND sealed_at < $2
		 GROUP BY project_id
	`, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("count sealed by project: %w", err)
	}
	defer rows.Close()

	out := make(map[string]int)
	for rows.Next() {
		var projectID string
		var n int
		if err := rows.Scan(&projectID, &n); err != nil {
			return nil, fmt.Errorf("count sealed scan: %w", err)
		}
		out[projectID] = n
		// This map is bounded by a cap, and the cap REFUSES rather than
		// truncating.
		//
		// The distinction is the reason the check exists at all. A cap
		// that silently returned fewer rows would hand the caller a
		// short project list, which would build a perfectly valid
		// looking checkpoint over a tree that is missing tenants. No
		// verifier could ever detect that, because every hash in it
		// would be correct. Losing an interval loudly is recoverable;
		// anchoring an incomplete tree is permanent, and it is the exact
		// omission the checkpoint chain exists to expose.
		//
		// The limit itself lives in sqlite_execution_sealing.go so both
		// backends share one number.
		if len(out) > MaxProjectsPerInterval {
			return nil, fmt.Errorf(
				"count sealed by project: more than %d projects active in [%s, %s); "+
					"refusing rather than truncating, because a short result would "+
					"drop tenants from the interval's tree and be indistinguishable "+
					"from the omission this chain exists to detect",
				MaxProjectsPerInterval, from.UTC(), to.UTC())
		}
	}
	return out, rows.Err()
}

// ListSealedExecutionIDs returns one project's execution ids sealed in
// [from, to), ordered by sealed_at then execution_id. The tiebreak is
// required, not cosmetic: the project's Merkle root depends on leaf
// order, and a single seal pass stamps many rows with the same instant.
func (s *PostgresStore) ListSealedExecutionIDs(
	ctx context.Context,
	projectID string,
	from, to time.Time,
) ([]string, error) {
	if projectID == "" {
		return nil, fmt.Errorf("list sealed execution ids: empty project_id")
	}
	if !to.After(from) {
		return nil, fmt.Errorf("list sealed execution ids: to (%s) must be after from (%s)",
			to.UTC(), from.UTC())
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT execution_id
		  FROM executions
		 WHERE project_id = $1
		   AND sealed_at >= $2 AND sealed_at < $3
		 ORDER BY sealed_at, execution_id
	`, projectID, from.UTC(), to.UTC())
	if err != nil {
		return nil, fmt.Errorf("list sealed execution ids: %w", err)
	}
	defer rows.Close()

	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("list sealed execution ids scan: %w", err)
		}
		out = append(out, id)
		if len(out) > MaxSealedExecutionsPerProjectPerInterval {
			return nil, fmt.Errorf(
				"list sealed execution ids: project %q has more than %d executions "+
					"sealed in [%s, %s); refusing rather than truncating, because a "+
					"short list would silently exclude executions from the project's "+
					"Merkle root and be indistinguishable from the omission this "+
					"chain exists to detect",
				projectID, MaxSealedExecutionsPerProjectPerInterval, from.UTC(), to.UTC())
		}
	}
	return out, rows.Err()
}
