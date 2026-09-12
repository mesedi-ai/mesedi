// Event queries the tool, throttling, DLP and validator detectors read.
// Split out of sqlite.go on 2026-09-12; every declaration moved verbatim.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

// FindFirstFailedToolName returns the tool_name of the first (lowest
// sequence) tool_call event with payload.status = "failed" for the
// given execution. Returns "" with nil error if no failed tool calls
// exist.
//
// Uses SQLite's JSON1 extension (json_extract) so we don't have to
// scan-and-unmarshal Go-side. The events table's payload column is
// stored as BLOB but JSON1 reads it transparently as JSON text.
func (s *SQLiteStore) FindFirstFailedTool(
	ctx context.Context,
	executionID string,
) (toolName, exceptionType string, err error) {
	var name sql.NullString
	var exc sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT json_extract(payload, '$.tool_name'),
		       json_extract(payload, '$.exception_type')
		FROM events
		WHERE execution_id = ?
		  AND event_type = 'tool_call'
		  AND json_extract(payload, '$.status') = 'failed'
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

// FindFirstThrottlingSignal returns the cluster signature derived
// from the first (lowest-sequence) infrastructure_event row for the
// given execution. Returns "" with nil error when no such event
// exists or when the row's payload is missing required fields.
//
// Calls ThrottlingSignature internally to assemble the signature
// from the payload fields, so the handler only needs to pass the
// result to GroupInfrastructureThrottled. This keeps the signature
// assembly logic in one place rather than duplicating field-name
// knowledge in the handler.
func (s *SQLiteStore) FindFirstThrottlingSignal(
	ctx context.Context,
	executionID string,
) (string, error) {
	var reason, provider, dimension, circuitState sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT
			json_extract(payload, '$.event_type'),
			json_extract(payload, '$.provider'),
			json_extract(payload, '$.quota_dimension'),
			json_extract(payload, '$.circuit_state')
		FROM events
		WHERE execution_id = ?
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

// FindFirstDLPSignal returns the rule_id of the highest-priority
// dlp_scan_result hit recorded against this execution, or empty
// string if no DLP events fired. "Highest priority" means: prefer
// the first critical-severity hit by sequence; if none exist, fall
// back to the first high-severity hit. medium-severity hits never
// cluster and are filtered out here.
//
// The returned rule_id is the data_leakage cluster signature
// (e.g. "aws_access_key"), one failure_group per rule per project.
func (s *SQLiteStore) FindFirstDLPSignal(
	ctx context.Context,
	executionID string,
) (string, error) {
	// LEGACY: delegate to the new ForSeverities method with the
	// historical default. Preserves backward compat for any caller
	// or test fixture still hitting this signature directly.
	return s.FindFirstDLPSignalForSeverities(ctx, executionID,
		[]string{"critical", "high"})
}

// FindFirstDLPSignalForSeverities, SQLite implementation
// (data_leakage.G5 wave). Builds the IN clause dynamically from
// the customer's allowed-severity slice. The CASE ORDER BY is
// constant (critical < high < medium < other) so priority remains
// deterministic regardless of which severities are in scope.
func (s *SQLiteStore) FindFirstDLPSignalForSeverities(
	ctx context.Context,
	executionID string,
	allowed []string,
) (string, error) {
	if len(allowed) == 0 {
		return "", fmt.Errorf("FindFirstDLPSignalForSeverities: allowed severities slice required")
	}
	// Build ?-placeholder list for the IN clause. args slice holds
	// the execution_id first followed by every allowed severity in
	// the same order they appear in the IN clause.
	placeholders := make([]string, len(allowed))
	args := make([]any, 0, len(allowed)+1)
	args = append(args, executionID)
	for i, sev := range allowed {
		placeholders[i] = "?"
		args = append(args, sev)
	}
	query := `
		SELECT json_extract(payload, '$.hits[0].rule_id')
		FROM events
		WHERE execution_id = ?
		  AND event_type = 'dlp_scan_result'
		  AND json_extract(payload, '$.highest_severity') IN (` +
		strings.Join(placeholders, ",") + `)
		ORDER BY
			CASE json_extract(payload, '$.highest_severity')
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

// ListCheckpointPayloads returns the payloads of all checkpoint
// events on the given execution in sequence order. Returns an empty
// slice (not nil) when no checkpoints exist, so the semantic_loop
// detector can range over the result unconditionally.
func (s *SQLiteStore) ListCheckpointPayloads(
	ctx context.Context,
	executionID string,
) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = ?
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

// ListSuccessfulToolReturns returns recent return_value payloads
// from successful tool_call events for a (project, tool). Used by
// the schema-drift detector to build the historical baseline.
// Excludes the calling execution so we compare against PRIOR runs.
func (s *SQLiteStore) ListSuccessfulToolReturns(
	ctx context.Context,
	projectID, toolName, excludeExecutionID string,
	limit int,
) ([][]byte, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT json_extract(ev.payload, '$.return_value')
		FROM events ev
		JOIN executions ex ON ex.execution_id = ev.execution_id
		WHERE ex.project_id = ?
		  AND ev.event_type = 'tool_call'
		  AND json_extract(ev.payload, '$.tool_name') = ?
		  AND COALESCE(json_extract(ev.payload, '$.status'), 'ok') != 'failed'
		  AND ev.execution_id != ?
		ORDER BY ev.timestamp DESC
		LIMIT ?
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

// ListToolDescriptions returns recent tool_description values from
// tool_call events for a (project, tool). Twin of the Postgres method
// of the same name. See the interface doc in store.go for why this is
// separate from ListSuccessfulToolReturns.
func (s *SQLiteStore) ListToolDescriptions(
	ctx context.Context,
	projectID, toolName, excludeExecutionID string,
	limit int,
) ([]string, error) {
	if limit <= 0 {
		limit = 100
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT json_extract(ev.payload, '$.tool_description')
		FROM events ev
		JOIN executions ex ON ex.execution_id = ev.execution_id
		WHERE ex.project_id = ?
		  AND ev.event_type = 'tool_call'
		  AND json_extract(ev.payload, '$.tool_name') = ?
		  AND ev.execution_id != ?
		ORDER BY ev.timestamp DESC
		LIMIT ?
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
		// SDKs older than the version that added tool_description
		// send nothing here. Skipping rather than counting "" as a
		// value matters: an empty string would form its own majority
		// baseline and then every upgraded client would look like
		// drift on its first call.
		if !d.Valid || d.String == "" {
			continue
		}
		out = append(out, d.String)
	}
	return out, rows.Err()
}

// ListToolNamesInExecution returns the distinct tool_names invoked
// successfully in the execution. Used by the schema-drift detector
// to enumerate tools to query history for.
func (s *SQLiteStore) ListToolNamesInExecution(
	ctx context.Context,
	executionID string,
) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT json_extract(payload, '$.tool_name')
		FROM events
		WHERE execution_id = ?
		  AND event_type = 'tool_call'
		  AND COALESCE(json_extract(payload, '$.status'), 'ok') != 'failed'
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

// ListAllToolCallPayloads returns every tool_call event payload on
// the execution in sequence order, including failed calls. Used by
// the sandbox_escape detector.
func (s *SQLiteStore) ListAllToolCallPayloads(
	ctx context.Context,
	executionID string,
) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = ?
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

// FindFirstFailedValidator returns the validator name from the first
// (lowest-sequence) validator_result event with payload.passed = false
// for the given execution. JSON1 boolean comparison: SQLite stores
// JSON booleans as `true`/`false` text, so we compare against the
// JSON-equivalent value json('false').
func (s *SQLiteStore) FindFirstFailedValidator(
	ctx context.Context,
	executionID string,
) (validatorName, severityHint, category string, err error) {
	var name sql.NullString
	var sev sql.NullString
	var cat sql.NullString
	err = s.db.QueryRowContext(ctx, `
		SELECT json_extract(payload, '$.name'),
		       json_extract(payload, '$.severity'),
		       json_extract(payload, '$.category')
		FROM events
		WHERE execution_id = ?
		  AND event_type = 'validator_result'
		  AND json_extract(payload, '$.passed') = 0
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
