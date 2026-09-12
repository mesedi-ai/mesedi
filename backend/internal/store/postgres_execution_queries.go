// Read-side execution and event queries for the dashboard.
// Split out of postgres.go on 2026-09-12; every declaration moved verbatim.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

func (s *PostgresStore) ListExecutions(ctx context.Context, projectID string, q string, limit, offset int) ([]*events.Execution, error) {
	// Search filter (list-search-paginate wave): when q is non-empty,
	// restrict to rows whose execution_id OR crash_signature ILIKE q.
	// Postgres' ILIKE is case-insensitive by definition (vs SQLite's
	// LOWER(col) LIKE LOWER(?)), twin behavior, different SQL.
	args := []any{projectID}
	whereClause := "project_id = $1"
	if q != "" {
		whereClause += " AND (execution_id ILIKE '%' || $2 || '%'" +
			" OR crash_signature ILIKE '%' || $2 || '%')"
		args = append(args, q)
	}
	args = append(args, limit, offset)
	limitPlaceholder := fmt.Sprintf("$%d", len(args)-1)
	offsetPlaceholder := fmt.Sprintf("$%d", len(args))

	// G202: whereClause is built above from allowlisted fragments
	// (project_id / status / crash_signature ILIKE with parameter
	// placeholders). limitPlaceholder / offsetPlaceholder are
	// server-controlled $N tokens, never user input. All user-supplied
	// values flow through args... as parameterized placeholders.
	//nolint:gosec // G202: false positive, see comment above.
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			execution_id, project_id, status,
			started_at, ended_at,
			duration_ms, total_tokens_in, total_tokens_out,
			estimated_cost_usd, sdk_language, sdk_version, crash_signature
		FROM executions
		WHERE `+whereClause+`
		ORDER BY started_at DESC
		LIMIT `+limitPlaceholder+` OFFSET `+offsetPlaceholder+`
	`, args...)
	if err != nil {
		return nil, fmt.Errorf("query executions: %w", err)
	}
	defer rows.Close()
	return scanExecutionRowsPg(rows)
}

// ListActiveExecutionsByProject is the Postgres counterpart to
// SQLiteStore.ListActiveExecutionsByProject. See that method's doc
// comment for contract.
func (s *PostgresStore) ListActiveExecutionsByProject(ctx context.Context, projectID string) ([]*events.Execution, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			execution_id, project_id, status,
			started_at, ended_at,
			duration_ms, total_tokens_in, total_tokens_out,
			estimated_cost_usd, sdk_language, sdk_version, crash_signature
		FROM executions
		WHERE project_id = $1 AND status = $2
		ORDER BY started_at DESC
	`, projectID, string(events.StatusStarted))
	if err != nil {
		return nil, fmt.Errorf("query active executions: %w", err)
	}
	defer rows.Close()
	return scanExecutionRowsPg(rows)
}

func (s *PostgresStore) ListExecutionsByFailureGroup(ctx context.Context, groupID string, limit, offset int) ([]*events.Execution, error) {
	// JOIN through execution_failure_groups so we surface executions
	// whose PRIMARY classification went to a different group but
	// where this group was a SECONDARY classification. See sqlite.go
	// counterpart and migration 039 for the rationale.
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			e.execution_id, e.project_id, e.status,
			e.started_at, e.ended_at,
			e.duration_ms, e.total_tokens_in, e.total_tokens_out,
			e.estimated_cost_usd, e.sdk_language, e.sdk_version, e.crash_signature
		FROM executions e
		INNER JOIN execution_failure_groups efg
			ON efg.execution_id = e.execution_id
		WHERE efg.group_id = $1
		ORDER BY e.started_at DESC
		LIMIT $2 OFFSET $3
	`, groupID, limit, offset)
	if err != nil {
		return nil, fmt.Errorf("query executions by failure_group: %w", err)
	}
	defer rows.Close()
	return scanExecutionRowsPg(rows)
}

// scanExecutionRowsPg is the postgres counterpart to scanExecutionRows.
// The only difference: started_at / ended_at come back as time.Time
// directly (TIMESTAMPTZ columns), not as RFC3339 strings.
func scanExecutionRowsPg(rows *sql.Rows) ([]*events.Execution, error) {
	var out []*events.Execution
	for rows.Next() {
		var (
			e          events.Execution
			endedAt    sql.NullTime
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
			&e.StartedAt, &endedAt,
			&durationMs, &tokensIn, &tokensOut,
			&costUSD, &sdkLang, &sdkVer, &crashSig,
		); err != nil {
			return nil, err
		}
		if endedAt.Valid {
			t := endedAt.Time
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

func (s *PostgresStore) ListEventsForExecution(ctx context.Context, executionID string) ([]*events.Event, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			event_id, execution_id, event_type, sequence,
			timestamp, duration_ms, payload
		FROM events
		WHERE execution_id = $1
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
			durationMs   sql.NullInt64
			payloadBytes []byte
		)
		if err := rows.Scan(
			&e.EventID, &e.ExecutionID, &e.EventType, &e.Sequence,
			&e.Timestamp, &durationMs, &payloadBytes,
		); err != nil {
			return nil, err
		}
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

func (s *PostgresStore) CountExecutionsByStatusSince(ctx context.Context, projectID, status string, cutoff time.Time) (int, error) {
	query := "SELECT COUNT(*) FROM executions WHERE project_id = $1"
	args := []any{projectID}
	placeholderIdx := 2

	if status != "" {
		query += fmt.Sprintf(" AND status = $%d", placeholderIdx)
		args = append(args, status)
		placeholderIdx++
	}
	if !cutoff.IsZero() {
		query += fmt.Sprintf(" AND started_at >= $%d", placeholderIdx)
		args = append(args, cutoff.UTC())
	}

	var n int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&n); err != nil {
		return 0, fmt.Errorf("count executions: %w", err)
	}
	return n, nil
}

// SumExecutionCostByProjectSince is the Postgres counterpart to
// SQLiteStore.SumExecutionCostByProjectSince. See that method's doc
// comment for contract.
func (s *PostgresStore) SumExecutionCostByProjectSince(
	ctx context.Context,
	projectID string,
	since time.Time,
) (float64, int, error) {
	query := "SELECT COALESCE(SUM(estimated_cost_usd), 0), COUNT(*) FROM executions WHERE project_id = $1"
	args := []any{projectID}
	if !since.IsZero() {
		query += " AND started_at >= $2"
		args = append(args, since.UTC())
	}
	var cost float64
	var count int
	if err := s.db.QueryRowContext(ctx, query, args...).Scan(&cost, &count); err != nil {
		return 0, 0, fmt.Errorf("sum execution cost: %w", err)
	}
	return cost, count, nil
}

// GetCostByTenant is the Postgres counterpart to
// SQLiteStore.GetCostByTenant. See that method's doc comment for the
// contract. Uses $N placeholder syntax for parameters; the placeholder
// indices are assembled dynamically because the time-window bounds
// are optional.
func (s *PostgresStore) GetCostByTenant(
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
		WHERE project_id = $1
	`
	args := []any{projectID}
	next := 2
	if !since.IsZero() {
		query += fmt.Sprintf(" AND started_at >= $%d", next)
		args = append(args, since.UTC())
		next++
	}
	if !until.IsZero() {
		query += fmt.Sprintf(" AND started_at < $%d", next)
		args = append(args, until.UTC())
		next++
	}
	query += `
		GROUP BY COALESCE(tenant_id, '')
		ORDER BY total_cost_usd DESC, execution_count DESC
	`
	if limit > 0 {
		// G202 false positive: $%d expands to a Postgres parameter
		// PLACEHOLDER index (e.g. "$4"), not the actual value. The
		// real `limit` int is bound through args below, the standard
		// safe-parameterized-query pattern.
		query += fmt.Sprintf(" LIMIT $%d", next) //nolint:gosec // G202: $N is a placeholder index, value is parameterized
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
