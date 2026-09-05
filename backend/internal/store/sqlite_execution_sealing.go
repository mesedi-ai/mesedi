package store

// Execution sealing for the checkpoint chain, SQLite implementation
// (migration 057). Sidecar matching the layout used for retention /
// provider_incident / time_budget. Twin: postgres_execution_sealing.go.
//
// Sealing decides, once and permanently, which hourly checkpoint an
// execution belongs to. See migrations/057_executions_sealed_at.sql for
// why neither started_at nor ended_at can serve that purpose.
//
// BOTH STORES MUST CARRY THIS. Changing one and not the other is the
// exact failure mode that caused the migration-056 production outage.

import (
	"context"
	"fmt"
	"time"
)

// Memory bounds on the interval queries. Both are MEMORY LIMITS, not
// product limits, and both are enforced by REFUSING rather than by
// truncating.
//
// That distinction is the whole point. Truncating a result set here
// would silently drop executions or whole tenants out of the interval's
// Merkle tree, which is precisely the omission attack the checkpoint
// chain exists to make detectable. A cap that quietly returns fewer
// rows would build a perfectly valid-looking checkpoint over an
// incomplete set, and no verifier could ever tell. Failing the interval
// loudly is recoverable; silently anchoring a lie is not.
//
// If either ceiling is ever reached in practice the answer is a shorter
// interval or a batched tree build, decided deliberately, not a bigger
// number pasted in here.
// Declared as var rather than const so tests can lower them and
// actually exercise the refusal path. A cap that cannot be tested is a
// cap nobody knows works, and this one only matters on the day it
// fires. Production code must never assign to these.
var (
	// MaxSealedExecutionsPerProjectPerInterval bounds the id slice one
	// project can produce for one interval.
	MaxSealedExecutionsPerProjectPerInterval = 100_000

	// MaxProjectsPerInterval bounds the per-project count map. One
	// entry per project with activity in the interval.
	MaxProjectsPerInterval = 10_000
)

// SealExecutions marks eligible executions with sealed_at = now.
//
// Two kinds become eligible:
//
//   - SETTLED: ended_at is set and is older than settle. The grace
//     period exists because events can still arrive after an execution
//     reports it ended; sealing immediately would anchor a digest that
//     later changes, and a changed root reads as tampering rather than
//     as a race.
//
//   - TIMED OUT: no ended_at, but started_at is older than timeout.
//     Sealed as-is, carrying whatever status it has. An execution that
//     never ends would otherwise stay outside the chain forever, and an
//     omission nobody can see is the precise attack this mechanism
//     exists to expose. Better to anchor an incomplete record that
//     openly says it is incomplete.
//
// Idempotent by construction: the WHERE clause excludes rows that
// already carry sealed_at, so a second pass cannot move an execution
// between intervals. That is not an optimisation, interval membership
// must be recorded once and never recomputed, or the chain anchors a
// fact that can change underneath it.
//
// Returns the number of rows sealed.
func (s *SQLiteStore) SealExecutions(
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
		   SET sealed_at = ?
		 WHERE sealed_at IS NULL
		   AND (
		         (ended_at IS NOT NULL AND ended_at <= ?)
		      OR (ended_at IS NULL     AND started_at <= ?)
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

// CountSealedByProject returns, per project, how many executions were
// sealed in [from, to).
//
// Half-open on purpose. Adjacent checkpoints share a boundary instant,
// and a closed upper bound would count an execution sealed exactly on
// the hour in BOTH intervals, inflating one cumulative count and
// making the chain's arithmetic fail to reconcile against itself.
func (s *SQLiteStore) CountSealedByProject(
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
		 WHERE sealed_at >= ? AND sealed_at < ?
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
// [from, to), in the order their leaves must be hashed.
//
// ORDER BY sealed_at, execution_id, and the tiebreak is not
// decoration. The project's Merkle root depends on leaf order, so two
// runs that ordered ties differently would produce two different roots
// for identical data, and the second one would look like tampering.
// sealed_at ties are routine because a single seal pass stamps every
// row it touches with the same instant.
func (s *SQLiteStore) ListSealedExecutionIDs(
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
		 WHERE project_id = ?
		   AND sealed_at >= ? AND sealed_at < ?
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
