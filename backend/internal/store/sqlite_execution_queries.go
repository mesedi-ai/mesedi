// Read-side execution and event queries for the dashboard.
// Split out of sqlite.go on 2026-09-12; every declaration moved verbatim.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

// ListExecutions returns the project's executions sorted by
// started_at DESC (most recent first), paginated.
func (s *SQLiteStore) ListExecutions(
	ctx context.Context,
	projectID string,
	q string,
	limit, offset int,
) ([]*events.Execution, error) {
	// Search filter (list-search-paginate wave): when q is non-empty,
	// restrict to rows whose execution_id OR crash_signature contains
	// q, case-insensitively. Parameterized query, safe against
	// injection. Empty q skips the predicate entirely so existing
	// internal callers (admin export, savings report) keep their
	// fast unfiltered path.
	args := []any{projectID}
	whereClause := "project_id = ?"
	if q != "" {
		whereClause += " AND (LOWER(execution_id) LIKE '%' || LOWER(?) || '%'" +
			" OR LOWER(crash_signature) LIKE '%' || LOWER(?) || '%')"
		args = append(args, q, q)
	}
	args = append(args, limit, offset)

	rows, err := s.db.QueryContext(ctx, `
		SELECT
			execution_id, project_id, status,
			started_at, ended_at,
			duration_ms, total_tokens_in, total_tokens_out,
			estimated_cost_usd, sdk_language, sdk_version, crash_signature
		FROM executions
		WHERE `+whereClause+`
		ORDER BY started_at DESC
		LIMIT ? OFFSET ?
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("query executions: %w", err)
	}
	defer rows.Close()
	return scanExecutionRows(rows)
}

// ListActiveExecutionsByProject returns executions that are still
// running (status = "started"). Used by the budget-ceiling halt
// fan-out to enumerate halt targets when a tenant breaches.
func (s *SQLiteStore) ListActiveExecutionsByProject(
	ctx context.Context,
	projectID string,
) ([]*events.Execution, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			execution_id, project_id, status,
			started_at, ended_at,
			duration_ms, total_tokens_in, total_tokens_out,
			estimated_cost_usd, sdk_language, sdk_version, crash_signature
		FROM executions
		WHERE project_id = ? AND status = ?
		ORDER BY started_at DESC
	`, projectID, string(events.StatusStarted))
	if err != nil {
		return nil, fmt.Errorf("query active executions: %w", err)
	}
	defer rows.Close()
	return scanExecutionRows(rows)
}

// ListExecutionsByFailureGroup returns executions classified into
// groupID, sorted by started_at DESC. Joins through the
// execution_failure_groups link table so executions whose PRIMARY
// classification was a different group still surface here when this
// group was a SECONDARY classification on them. Before migration 039
// this query was a direct equality scan on executions.failure_group_id,
// which silently dropped every execution whose primary detector ran
// first and claimed the slot, that's the bug that motivated 039.
//
// Caller is expected to have already verified the group belongs to
// the auth context's project; this method does not enforce project
// scoping (the failure_groups foreign key on the link table provides
// it transitively).
func (s *SQLiteStore) ListExecutionsByFailureGroup(
	ctx context.Context,
	groupID string,
	limit, offset int,
) ([]*events.Execution, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			e.execution_id, e.project_id, e.status,
			e.started_at, e.ended_at,
			e.duration_ms, e.total_tokens_in, e.total_tokens_out,
			e.estimated_cost_usd, e.sdk_language, e.sdk_version, e.crash_signature
		FROM executions e
		INNER JOIN execution_failure_groups efg
			ON efg.execution_id = e.execution_id
		WHERE efg.group_id = ?
		ORDER BY e.started_at DESC
		LIMIT ? OFFSET ?
	`, groupID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query executions by failure_group: %w", err)
	}
	defer rows.Close()
	return scanExecutionRows(rows)
}

// scanExecutionRows is the shared row-iteration helper for both the
// project-scoped and failure-group-scoped execution list queries. Both
// queries return identical column ordering, so the scanning logic is
// truly shared (not just copy-paste).
func scanExecutionRows(rows *sql.Rows) ([]*events.Execution, error) {
	var out []*events.Execution
	for rows.Next() {
		var (
			e          events.Execution
			startedAt  string
			endedAt    sql.NullString
			durationMs sql.NullInt64
			tokensIn   sql.NullInt64
			tokensOut  sql.NullInt64
			costUSD    sql.NullFloat64
			sdkLang    sql.NullString
			sdkVer     sql.NullString
			crashSig   sql.NullString
		)
		if err := rows.Scan(
			&e.ExecutionID, &e.ProjectID, &e.Status,
			&startedAt, &endedAt,
			&durationMs, &tokensIn, &tokensOut,
			&costUSD, &sdkLang, &sdkVer, &crashSig,
		); err != nil {
			return nil, err
		}
		e.StartedAt, _ = time.Parse(time.RFC3339, startedAt)
		if endedAt.Valid {
			t, _ := time.Parse(time.RFC3339, endedAt.String)
			e.EndedAt = &t
		}
		if durationMs.Valid {
			e.DurationMs = durationMs.Int64
		}
		if tokensIn.Valid {
			e.TotalTokensIn = int(tokensIn.Int64)
		}
		if tokensOut.Valid {
			e.TotalTokensOut = int(tokensOut.Int64)
		}
		if costUSD.Valid {
			e.EstimatedCostUSD = costUSD.Float64
		}
		if sdkLang.Valid {
			e.SDKLanguage = sdkLang.String
		}
		if sdkVer.Valid {
			e.SDKVersion = sdkVer.String
		}
		if crashSig.Valid {
			e.CrashSignature = crashSig.String
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// ListEventsForExecution returns the events recorded against a single
// execution, sorted by sequence ASC (oldest first, matching the order
// they were emitted by the agent).
func (s *SQLiteStore) ListEventsForExecution(
	ctx context.Context,
	executionID string,
) ([]*events.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			event_id, execution_id, event_type, sequence,
			timestamp, duration_ms, payload
		FROM events
		WHERE execution_id = ?
		ORDER BY sequence ASC
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("query events: %w", err)
	}
	defer rows.Close()

	var out []*events.Event
	for rows.Next() {
		var (
			e            events.Event
			ts           string
			durationMs   sql.NullInt64
			payloadBytes []byte
		)
		if err := rows.Scan(
			&e.EventID, &e.ExecutionID, &e.EventType, &e.Sequence,
			&ts, &durationMs, &payloadBytes,
		); err != nil {
			return nil, err
		}
		e.Timestamp, _ = time.Parse(time.RFC3339, ts)
		if durationMs.Valid {
			e.DurationMs = durationMs.Int64
		}
		if len(payloadBytes) > 0 {
			e.Payload = json.RawMessage(payloadBytes)
		}
		out = append(out, &e)
	}
	return out, rows.Err()
}

// CountExecutionsByStatusSince returns the number of executions for the
// given project, optionally filtered by status and/or cutoff. An empty
// status string means "any status." A zero cutoff means "all time."
// All four combinations are supported.
func (s *SQLiteStore) CountExecutionsByStatusSince(
	ctx context.Context,
	projectID, status string,
	cutoff time.Time,
) (int, error) {
	query := "SELECT COUNT(*) FROM executions WHERE project_id = ?"
	args := []any{projectID}

	if status != "" {
		query += " AND status = ?"
		args = append(args, status)
	}
	if !cutoff.IsZero() {
		query += " AND started_at >= ?"
		args = append(args, cutoff.UTC()) // time.Time bound, NEVER Format(...): formats can't compare
	}

	var n int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count executions: %w", err)
	}
	return n, nil
}

// SumExecutionCostByProjectSince aggregates SUM(estimated_cost_usd) and
// COUNT(*) across executions of projectID. Used by the org-rollup
// endpoint for per-project burn. Reads the persisted
// estimated_cost_usd column directly, which matches what the existing
// project-scoped dashboard surfaces show.
func (s *SQLiteStore) SumExecutionCostByProjectSince(
	ctx context.Context,
	projectID string,
	since time.Time,
) (float64, int, error) {
	query := "SELECT COALESCE(SUM(estimated_cost_usd), 0), COUNT(*) FROM executions WHERE project_id = ?"
	args := []any{projectID}
	if !since.IsZero() {
		query += " AND started_at >= ?"
		args = append(args, since.UTC()) // NEVER Format(...): see sqlite_sum_execution_cost_test.go's header
	}
	var cost float64
	var count int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&cost, &count); err != nil {
		return 0, 0, fmt.Errorf("sum execution cost: %w", err)
	}
	return cost, count, nil
}

// GetCostByTenant aggregates per-tenant cost across the project's
// executions within the requested time window. NULL tenant_id rows
// collapse into a single TenantID="" bucket so the dashboard can
// render unattributed cost as a distinct row instead of dropping it
// silently. since/until are inclusive lower / exclusive upper bounds
// matched against executions.started_at; zero values disable the
// respective bound. limit caps row count (0 = unlimited).
func (s *SQLiteStore) GetCostByTenant(
	ctx context.Context,
	projectID string,
	since time.Time,
	until time.Time,
	limit int,
) ([]TenantCostRow, error) {
	query := `
		SELECT
			COALESCE(tenant_id, '') AS tenant_id,
			COALESCE(SUM(estimated_cost_usd), 0) AS total_cost_usd,
			COUNT(*) AS execution_count,
			COALESCE(SUM(total_tokens_in), 0) AS total_tokens_in,
			COALESCE(SUM(total_tokens_out), 0) AS total_tokens_out
		FROM executions
		WHERE project_id = ?
	`
	args := []any{projectID}
	if !since.IsZero() {
		query += " AND started_at >= ?"
		args = append(args, since.UTC()) // time.Time bound, NEVER Format(...): formats can't compare
	}
	if !until.IsZero() {
		query += " AND started_at < ?"
		args = append(args, until.UTC()) // time.Time bound, NEVER Format(...): formats can't compare
	}
	query += `
		GROUP BY COALESCE(tenant_id, '')
		ORDER BY total_cost_usd DESC, execution_count DESC
	`
	if limit > 0 {
		query += " LIMIT ?"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("get cost by tenant: %w", err)
	}
	defer rows.Close()

	out := []TenantCostRow{}
	for rows.Next() {
		var r TenantCostRow
		if err := rows.Scan(
			&r.TenantID,
			&r.TotalCostUSD,
			&r.ExecutionCount,
			&r.TotalTokensIn,
			&r.TotalTokensOut,
		); err != nil {
			return nil, fmt.Errorf("scan tenant cost row: %w", err)
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate tenant cost rows: %w", err)
	}
	return out, nil
}
