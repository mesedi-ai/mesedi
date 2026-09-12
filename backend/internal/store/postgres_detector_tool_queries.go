// Event queries the tool, throttling, DLP and validator detectors read.
// Split out of postgres.go on 2026-09-12; every declaration moved verbatim.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// FindFirstFailedTool uses Postgres jsonb operators. See the SQLite
// twin's docstring for the granular-sig contract (also returns
// exception_type).
func (s *PostgresStore) FindFirstFailedTool(ctx context.Context, executionID string) (toolName, exceptionType string, err error) {
	var name sql.NullString
	var exc sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT (payload::jsonb->>'tool_name'),
		       (payload::jsonb->>'exception_type')
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'tool_call'
		  AND (payload::jsonb->>'status') = 'failed'
		ORDER BY sequence ASC
		LIMIT 1
	`, executionID).Scan(&name, &exc)
	if err == sql.ErrNoRows {
		return "", "", nil
	}
	if err != nil {
		return "", "", fmt.Errorf("find first failed tool: %w", err)
	}
	if !name.Valid {
		return "", "", nil
	}
	if exc.Valid {
		exceptionType = exc.String
	}
	return name.String, exceptionType, nil
}

// FindFirstThrottlingSignal is the Postgres twin of the SQLite method
// of the same name. Pulls the four signature pieces from the first
// infrastructure_event row's payload and hands them to
// ThrottlingSignature for assembly. Returns "" with nil error when no
// such event exists.
func (s *PostgresStore) FindFirstThrottlingSignal(ctx context.Context, executionID string) (string, error) {
	var reason, provider, dimension, circuitState sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT
			(payload::jsonb->>'event_type'),
			(payload::jsonb->>'provider'),
			(payload::jsonb->>'quota_dimension'),
			(payload::jsonb->>'circuit_state')
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'infrastructure_event'
		ORDER BY sequence ASC
		LIMIT 1
	`, executionID).Scan(&reason, &provider, &dimension, &circuitState)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find first throttling signal: %w", err)
	}
	if !reason.Valid || reason.String == "" {
		return "", nil
	}
	return ThrottlingSignature(reason.String, provider.String, dimension.String, circuitState.String), nil
}

// FindFirstDLPSignal is the Postgres twin of the SQLite method of
// the same name. LEGACY, delegates to FindFirstDLPSignalForSeverities
// with the historical default.
func (s *PostgresStore) FindFirstDLPSignal(ctx context.Context, executionID string) (string, error) {
	return s.FindFirstDLPSignalForSeverities(ctx, executionID,
		[]string{"critical", "high"})
}

// FindFirstDLPSignalForSeverities, Postgres twin (data_leakage.G5
// wave). Same shape as the sqlite version: builds the IN clause
// dynamically from the customer's allowed-severity slice. Postgres
// numbered placeholders ($1, $2, ...) drive the IN clause; the
// CASE ORDER BY is constant so priority remains deterministic.
func (s *PostgresStore) FindFirstDLPSignalForSeverities(
	ctx context.Context,
	executionID string,
	allowed []string,
) (string, error) {
	if len(allowed) == 0 {
		return "", fmt.Errorf("FindFirstDLPSignalForSeverities: allowed severities slice required")
	}
	// Postgres uses numbered placeholders; execution_id is $1, then
	// each severity is $2, $3, ... Build the IN clause to match.
	placeholders := make([]string, len(allowed))
	args := make([]any, 0, len(allowed)+1)
	args = append(args, executionID)
	for i, sev := range allowed {
		placeholders[i] = fmt.Sprintf("$%d", i+2)
		args = append(args, sev)
	}
	query := `
		SELECT (payload::jsonb #>> '{hits,0,rule_id}')
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'dlp_scan_result'
		  AND (payload::jsonb->>'highest_severity') IN (` +
		strings.Join(placeholders, ",") + `)
		ORDER BY
			CASE (payload::jsonb->>'highest_severity')
				WHEN 'critical' THEN 0
				WHEN 'high'     THEN 1
				WHEN 'medium'   THEN 2
				ELSE 3
			END ASC,
			sequence ASC
		LIMIT 1
	`
	var ruleID sql.NullString
	err := s.db.QueryRowContext(ctx, query, args...).Scan(&ruleID)
	if errors.Is(err, sql.ErrNoRows) {
		return "", nil
	}
	if err != nil {
		return "", fmt.Errorf("find first dlp signal for severities: %w", err)
	}
	if !ruleID.Valid {
		return "", nil
	}
	return ruleID.String, nil
}

// ListCheckpointPayloads is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) ListCheckpointPayloads(ctx context.Context, executionID string) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'checkpoint'
		ORDER BY sequence ASC
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list checkpoint payloads: %w", err)
	}
	defer rows.Close()
	out := [][]byte{}
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan checkpoint payload: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate checkpoint payloads: %w", err)
	}
	return out, nil
}

// ListSuccessfulToolReturns is the Postgres twin of the SQLite
// method of the same name.
func (s *PostgresStore) ListSuccessfulToolReturns(
	ctx context.Context,
	projectID, toolName, excludeExecutionID string,
	limit int,
) ([][]byte, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT (ev.payload::jsonb->>'return_value')
		FROM events ev
		JOIN executions ex ON ex.execution_id = ev.execution_id
		WHERE ex.project_id = $1
		  AND ev.event_type = 'tool_call'
		  AND (ev.payload::jsonb->>'tool_name') = $2
		  AND COALESCE(ev.payload::jsonb->>'status', 'ok') != 'failed'
		  AND ev.execution_id != $3
		ORDER BY ev.timestamp DESC
		LIMIT $4
	`, projectID, toolName, excludeExecutionID, limit)
	if err != nil {
		return nil, fmt.Errorf("list successful tool returns: %w", err)
	}
	defer rows.Close()
	out := [][]byte{}
	for rows.Next() {
		var p sql.NullString
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan tool return: %w", err)
		}
		if !p.Valid || p.String == "" {
			continue
		}
		out = append(out, []byte(p.String))
	}
	return out, rows.Err()
}

// ListToolDescriptions is the Postgres twin of the SQLite method of
// the same name.
//
// PRODUCTION PATH. This store and the SQLite one carry separate
// hand-written SQL, and a change applied to one and not the other is
// invisible to a test that only exercises the other. That is exactly
// the shape of the migration-056 outage. Both were written together
// here and both are covered by tests.
func (s *PostgresStore) ListToolDescriptions(
	ctx context.Context,
	projectID, toolName, excludeExecutionID string,
	limit int,
) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT (ev.payload::jsonb->>'tool_description')
		FROM events ev
		JOIN executions ex ON ex.execution_id = ev.execution_id
		WHERE ex.project_id = $1
		  AND ev.event_type = 'tool_call'
		  AND (ev.payload::jsonb->>'tool_name') = $2
		  AND ev.execution_id != $3
		ORDER BY ev.timestamp DESC
		LIMIT $4
	`, projectID, toolName, excludeExecutionID, limit)
	if err != nil {
		return nil, fmt.Errorf("list tool descriptions: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var d sql.NullString
		if err := rows.Scan(&d); err != nil {
			return nil, fmt.Errorf("scan tool description: %w", err)
		}
		// See the SQLite twin: pre-upgrade SDKs send no description,
		// and counting "" as a value would let it form a majority
		// baseline that every upgraded client then "drifts" from.
		if !d.Valid || d.String == "" {
			continue
		}
		out = append(out, d.String)
	}
	return out, rows.Err()
}

// ListToolNamesInExecution is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) ListToolNamesInExecution(ctx context.Context, executionID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT (payload::jsonb->>'tool_name')
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'tool_call'
		  AND COALESCE(payload::jsonb->>'status', 'ok') != 'failed'
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list tool names: %w", err)
	}
	defer rows.Close()
	out := []string{}
	for rows.Next() {
		var n sql.NullString
		if err := rows.Scan(&n); err != nil {
			return nil, fmt.Errorf("scan tool name: %w", err)
		}
		if !n.Valid || n.String == "" {
			continue
		}
		out = append(out, n.String)
	}
	return out, rows.Err()
}

// ListAllToolCallPayloads is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) ListAllToolCallPayloads(ctx context.Context, executionID string) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'tool_call'
		ORDER BY sequence ASC
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list all tool_call payloads: %w", err)
	}
	defer rows.Close()
	out := [][]byte{}
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan tool_call payload: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FindFirstFailedValidator compares the jsonb-extracted 'passed' field
// to the literal text 'false'. In SQLite the same comparison was
// against integer 0 because SQLite's JSON1 returns 0 for false; in
// Postgres jsonb the text form is 'false'.
func (s *PostgresStore) FindFirstFailedValidator(
	ctx context.Context,
	executionID string,
) (validatorName, severityHint, category string, err error) {
	var name sql.NullString
	var sev sql.NullString
	var cat sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT (payload::jsonb->>'name'),
		       (payload::jsonb->>'severity'),
		       (payload::jsonb->>'category')
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'validator_result'
		  AND (payload::jsonb->>'passed') = 'false'
		ORDER BY sequence ASC
		LIMIT 1
	`, executionID).Scan(&name, &sev, &cat)
	if err == sql.ErrNoRows {
		return "", "", "", nil
	}
	if err != nil {
		return "", "", "", fmt.Errorf("find first failed validator: %w", err)
	}
	if !name.Valid {
		return "", "", "", nil
	}
	if sev.Valid {
		severityHint = sev.String
	}
	if cat.Valid {
		category = cat.String
	}
	return name.String, severityHint, category, nil
}
