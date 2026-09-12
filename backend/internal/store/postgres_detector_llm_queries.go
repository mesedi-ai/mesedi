// Event queries the LLM, eval, HITL, topology and provider detectors read.
// Split out of postgres.go on 2026-09-12; every declaration moved verbatim.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

func (s *PostgresStore) CountEventsForExecution(ctx context.Context, executionID string) (int, error) {
	var n int
	err := s.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM events WHERE execution_id = $1`,
		executionID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count events: %w", err)
	}
	return n, nil
}

// ListLLMCallPayloads is the Postgres twin of the SQLite method of
// the same name.
func (s *PostgresStore) ListLLMCallPayloads(ctx context.Context, executionID string) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'llm_call'
		ORDER BY sequence ASC
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list llm_call payloads: %w", err)
	}
	defer rows.Close()
	out := [][]byte{}
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan llm_call payload: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListEvalScorePayloads is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) ListEvalScorePayloads(ctx context.Context, executionID string) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'eval_score'
		ORDER BY sequence ASC
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list eval_score payloads: %w", err)
	}
	defer rows.Close()
	out := [][]byte{}
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan eval_score payload: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListHandoffsWithChildStatus is the Postgres twin of the SQLite
// method of the same name. Uses ->> to pull typed fields out of
// the jsonb payload column. The LEFT JOIN keeps handoffs whose
// child_execution_id did not resolve (either because the SDK
// could not provide one at emit-time, or because the referenced
// execution is in a different project_id and tenant isolation
// dropped it from the join).
func (s *PostgresStore) ListHandoffsWithChildStatus(
	ctx context.Context,
	parentExecutionID, projectID string,
) ([]HandoffWithChildStatus, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			(e.payload::jsonb->>'from_agent')         AS from_agent,
			(e.payload::jsonb->>'to_agent')           AS to_agent,
			(e.payload::jsonb->>'handoff_kind')       AS handoff_kind,
			(e.payload::jsonb->>'child_execution_id') AS child_execution_id,
			e.timestamp                               AS handoff_emitted_at,
			c.execution_id                            AS child_id_found,
			c.status                                  AS child_status,
			c.ended_at                                AS child_ended_at
		FROM events e
		LEFT JOIN executions c
		  ON c.execution_id = (e.payload::jsonb->>'child_execution_id')
		 AND c.project_id   = $1
		WHERE e.execution_id = $2
		  AND e.event_type   = 'agent_handoff'
		ORDER BY e.sequence ASC
	`, projectID, parentExecutionID)
	if err != nil {
		return nil, fmt.Errorf("list handoffs with child status: %w", err)
	}
	defer rows.Close()
	out := []HandoffWithChildStatus{}
	for rows.Next() {
		var (
			fromAgent  sql.NullString
			toAgent    sql.NullString
			handoffKnd sql.NullString
			childID    sql.NullString
			emittedAt  time.Time
			foundID    sql.NullString
			childStat  sql.NullString
			childEnded sql.NullTime
		)
		if err := rows.Scan(
			&fromAgent, &toAgent, &handoffKnd, &childID,
			&emittedAt, &foundID, &childStat, &childEnded,
		); err != nil {
			return nil, fmt.Errorf("scan handoff row: %w", err)
		}
		row := HandoffWithChildStatus{
			FromAgent:        fromAgent.String,
			ToAgent:          toAgent.String,
			HandoffKind:      handoffKnd.String,
			ChildExecutionID: childID.String,
			HandoffEmittedAt: emittedAt,
		}
		if foundID.Valid && foundID.String != "" {
			row.ChildExists = true
			row.ChildStatus = childStat.String
			if childEnded.Valid {
				t := childEnded.Time
				row.ChildEndedAt = &t
			}
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// ListHandoffEdgesInTopology is the Postgres twin of the SQLite
// method of the same name. Uses standard SQL:1999 recursive CTE
// syntax (no Postgres-specific quirks vs SQLite for this shape).
func (s *PostgresStore) ListHandoffEdgesInTopology(
	ctx context.Context,
	rootExecutionID, projectID string,
	maxDepth int,
) ([]HandoffEdge, error) {
	if maxDepth <= 0 {
		maxDepth = 8
	}
	rows, err := s.db.QueryContext(ctx, `
		WITH RECURSIVE subtree(execution_id, depth) AS (
			SELECT execution_id, 0
			FROM executions
			WHERE execution_id = $1 AND project_id = $2
			UNION ALL
			SELECT e.execution_id, s.depth + 1
			FROM executions e
			JOIN subtree s ON e.parent_execution_id = s.execution_id
			WHERE e.project_id = $3 AND s.depth < $4
		)
		SELECT
			e.execution_id,
			(e.payload::jsonb->>'from_agent') AS from_agent,
			(e.payload::jsonb->>'to_agent')   AS to_agent,
			e.timestamp                       AS emitted_at
		FROM events e
		JOIN subtree s ON s.execution_id = e.execution_id
		WHERE e.event_type = 'agent_handoff'
		ORDER BY e.timestamp ASC
	`, rootExecutionID, projectID, projectID, maxDepth)
	if err != nil {
		return nil, fmt.Errorf("list handoff edges in topology: %w", err)
	}
	defer rows.Close()
	out := []HandoffEdge{}
	for rows.Next() {
		var (
			edge HandoffEdge
			from sql.NullString
			to   sql.NullString
		)
		if err := rows.Scan(&edge.EmittingExecutionID, &from, &to, &edge.EmittedAt); err != nil {
			return nil, fmt.Errorf("scan handoff edge: %w", err)
		}
		edge.FromAgent = from.String
		edge.ToAgent = to.String
		if edge.FromAgent == "" || edge.ToAgent == "" {
			continue
		}
		out = append(out, edge)
	}
	return out, rows.Err()
}

// CountDistinctTenantsWithProviderError is the Postgres twin of the
// SQLite method of the same name.
func (s *PostgresStore) CountDistinctTenantsWithProviderError(
	ctx context.Context,
	projectID, provider, errorClass string,
	since time.Time,
) (int, error) {
	var n int
	err := s.db.QueryRowContext(ctx, `
		SELECT COUNT(DISTINCT COALESCE(x.tenant_id, ''))
		FROM (
			SELECT DISTINCT x.execution_id, x.tenant_id
			FROM executions x
			JOIN events ev ON ev.execution_id = x.execution_id
			WHERE x.project_id = $1
			  AND ev.event_type = 'llm_call'
			  AND (ev.payload::jsonb->>'provider')    = $2
			  AND (ev.payload::jsonb->>'error_class') = $3
			  AND x.started_at >= $4
		) AS x
	`, projectID, provider, errorClass, since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count distinct tenants with provider error: %w", err)
	}
	return n, nil
}

// ListHumanInterventionPayloads is the Postgres twin of the
// SQLite method of the same name.
func (s *PostgresStore) ListHumanInterventionPayloads(ctx context.Context, executionID string) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = $1
		  AND event_type   = 'human_intervention'
		ORDER BY sequence ASC
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list human_intervention payloads: %w", err)
	}
	defer rows.Close()
	out := [][]byte{}
	for rows.Next() {
		var p []byte
		if err := rows.Scan(&p); err != nil {
			return nil, fmt.Errorf("scan human_intervention payload: %w", err)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountHITLOutcomesInWindow is the Postgres twin of the SQLite
// method of the same name.
func (s *PostgresStore) CountHITLOutcomesInWindow(
	ctx context.Context,
	projectID string,
	since time.Time,
) (HITLOutcomeCounts, error) {
	var counts HITLOutcomeCounts
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(DISTINCT x.execution_id),
			COUNT(DISTINCT CASE
				WHEN (ev.payload::jsonb->>'response_kind') = 'rejected'
				THEN x.execution_id
			END),
			COUNT(DISTINCT CASE
				WHEN (ev.payload::jsonb->>'response_kind') = 'edited'
				THEN x.execution_id
			END)
		FROM executions x
		JOIN events ev ON ev.execution_id = x.execution_id
		WHERE x.project_id   = $1
		  AND ev.event_type  = 'human_intervention'
		  AND x.started_at  >= $2
	`, projectID, since).Scan(
		&counts.TotalExecutionsWithHITL,
		&counts.ExecutionsWithRejection,
		&counts.ExecutionsWithEdit,
	)
	if err != nil {
		return counts, fmt.Errorf("count hitl outcomes in window: %w", err)
	}
	return counts, nil
}

// GetExecutionTopology is the Postgres twin of the SQLite method of
// the same name. Uses standard SQL:1999 recursive CTE syntax which
// Postgres supports natively (no quirks vs SQLite for the two-CTE
// shape we're using here).
func (s *PostgresStore) GetExecutionTopology(
	ctx context.Context,
	projectID, executionID string,
	maxDepth int,
) ([]TopologyNode, error) {
	if maxDepth <= 0 {
		maxDepth = 8
	}
	query := `
		WITH RECURSIVE
		ancestors(execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, depth) AS (
			SELECT execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, 0
			FROM executions
			WHERE execution_id = $1 AND project_id = $2
			UNION ALL
			SELECT e.execution_id, e.parent_execution_id, e.status, e.started_at, e.ended_at, e.duration_ms, e.sdk_language, e.failure_group_id, a.depth - 1
			FROM executions e
			JOIN ancestors a ON e.execution_id = a.parent_execution_id
			WHERE e.project_id = $3 AND a.depth > $4
		),
		descendants(execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, depth) AS (
			SELECT execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, 0
			FROM executions
			WHERE execution_id = $5 AND project_id = $6
			UNION ALL
			SELECT e.execution_id, e.parent_execution_id, e.status, e.started_at, e.ended_at, e.duration_ms, e.sdk_language, e.failure_group_id, d.depth + 1
			FROM executions e
			JOIN descendants d ON e.parent_execution_id = d.execution_id
			WHERE e.project_id = $7 AND d.depth < $8
		)
		SELECT execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, depth FROM ancestors
		UNION
		SELECT execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, depth FROM descendants
		ORDER BY depth ASC, started_at ASC
	`
	// $4 is the depth floor for the ancestors CTE (negative); $8 is
	// the depth ceiling for the descendants CTE (positive). We
	// precompute the negation in Go so Postgres can resolve the
	// parameter type without needing a unary-minus operator (which
	// triggers "operator is not unique: - unknown" against an
	// unparameterized integer).
	rows, err := s.db.QueryContext(ctx, query,
		executionID, projectID,
		projectID, -maxDepth,
		executionID, projectID,
		projectID, maxDepth,
	)
	if err != nil {
		return nil, fmt.Errorf("get execution topology: %w", err)
	}
	defer rows.Close()
	out := []TopologyNode{}
	for rows.Next() {
		var (
			n          TopologyNode
			parent     sql.NullString
			endedAt    sql.NullTime
			durationMs sql.NullInt64
			sdkLang    sql.NullString
			fgID       sql.NullString
		)
		if err := rows.Scan(
			&n.ExecutionID, &parent, &n.Status, &n.StartedAt,
			&endedAt, &durationMs, &sdkLang, &fgID, &n.Depth,
		); err != nil {
			return nil, fmt.Errorf("scan topology row: %w", err)
		}
		if parent.Valid {
			v := parent.String
			n.ParentExecutionID = &v
		}
		if endedAt.Valid {
			t := endedAt.Time
			n.EndedAt = &t
		}
		if durationMs.Valid {
			n.DurationMs = durationMs.Int64
		}
		if sdkLang.Valid {
			n.SDKLanguage = sdkLang.String
		}
		if fgID.Valid {
			v := fgID.String
			n.FailureGroupID = &v
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *PostgresStore) ListModelsForExecution(ctx context.Context, executionID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT (payload::jsonb->>'model') AS model
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'llm_call'
		  AND (payload::jsonb->>'model') IS NOT NULL
		  AND (payload::jsonb->>'model') != ''
		ORDER BY model ASC
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list models for execution: %w", err)
	}
	defer rows.Close()

	models := make([]string, 0, 4)
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}
		if m != "" {
			models = append(models, m)
		}
	}
	return models, rows.Err()
}

func (s *PostgresStore) ListModelsForProjectSince(ctx context.Context, projectID string, cutoff time.Time, excludeExecutionID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT (e.payload::jsonb->>'model') AS model
		FROM events e
		JOIN executions x ON x.execution_id = e.execution_id
		WHERE x.project_id = $1
		  AND e.event_type = 'llm_call'
		  AND e.timestamp >= $2
		  AND e.execution_id != $3
		  AND (e.payload::jsonb->>'model') IS NOT NULL
		  AND (e.payload::jsonb->>'model') != ''
		ORDER BY model ASC
	`, projectID, cutoff.UTC(), excludeExecutionID)
	if err != nil {
		return nil, fmt.Errorf("list models for project since: %w", err)
	}
	defer rows.Close()

	models := make([]string, 0, 8)
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("scan model: %w", err)
		}
		if m != "" {
			models = append(models, m)
		}
	}
	return models, rows.Err()
}

func (s *PostgresStore) ListLLMUserMessagesForExecution(ctx context.Context, executionID string) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT (payload::jsonb->>'user_message') AS user_message
		FROM events
		WHERE execution_id = $1
		  AND event_type = 'llm_call'
		  AND (payload::jsonb->>'user_message') IS NOT NULL
		  AND (payload::jsonb->>'user_message') != ''
		ORDER BY sequence ASC
	`, executionID)
	if err != nil {
		return nil, fmt.Errorf("list user messages for execution: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0, 4)
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("scan user message: %w", err)
		}
		if m != "" {
			out = append(out, m)
		}
	}
	return out, rows.Err()
}

func (s *PostgresStore) ListLLMUserMessagesForProjectSince(ctx context.Context, projectID string, cutoff time.Time, excludeExecutionID string, limit int) ([]string, error) {
	query := `
		SELECT (e.payload::jsonb->>'user_message') AS user_message
		FROM events e
		JOIN executions x ON x.execution_id = e.execution_id
		WHERE x.project_id = $1
		  AND e.event_type = 'llm_call'
		  AND e.timestamp >= $2
		  AND e.execution_id != $3
		  AND (e.payload::jsonb->>'user_message') IS NOT NULL
		  AND (e.payload::jsonb->>'user_message') != ''
		ORDER BY e.timestamp DESC
	`
	args := []any{projectID, cutoff.UTC(), excludeExecutionID}
	if limit > 0 {
		query += " LIMIT $4"
		args = append(args, limit)
	}

	rows, err := s.db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("list user messages for project since: %w", err)
	}
	defer rows.Close()

	out := make([]string, 0, 64)
	for rows.Next() {
		var m string
		if err := rows.Scan(&m); err != nil {
			return nil, fmt.Errorf("scan user message: %w", err)
		}
		if m != "" {
			out = append(out, m)
		}
	}
	return out, rows.Err()
}
