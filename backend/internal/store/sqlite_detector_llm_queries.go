// Event queries the LLM, eval, HITL, topology and provider detectors read.
// Split out of sqlite.go on 2026-09-12; every declaration moved verbatim.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// CountEventsForExecution returns the number of event rows recorded
// against a single execution_id. Used by the step-count detector and
// (eventually) the replay UI's header.
func (s *SQLiteStore) CountEventsForExecution(
	ctx context.Context,
	executionID string,
) (int, error) {
	var n int
	err := s.db.QueryRowContext(
		ctx,
		`SELECT COUNT(*) FROM events WHERE execution_id = ?`,
		executionID,
	).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count events: %w", err)
	}
	return n, nil
}

// ListLLMCallPayloads returns every llm_call payload on the
// execution in sequence order. Shared by context_overflow and
// token_waste; kept as one query so the handler only hits the
// DB once per execution-terminal update for both detectors.
func (s *SQLiteStore) ListLLMCallPayloads(
	ctx context.Context,
	executionID string,
) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = ?
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

// ListEvalScorePayloads returns every eval_score event payload on
// the execution in sequence order. Used by the grounding_failure
// detector.
func (s *SQLiteStore) ListEvalScorePayloads(ctx context.Context, executionID string) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = ?
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

// ListHandoffsWithChildStatus joins every agent_handoff event on the
// parent execution with the terminal status of the referenced child
// execution. Used by the cascading_failure detector.
//
// Implementation notes:
//
//   - The join uses LEFT JOIN so handoffs that did not populate
//     child_execution_id (e.g. the SDK could not resolve it at
//     emit-time) still appear in the result with ChildExists=false.
//
//   - The child execution must live in the same project_id as the
//     parent. Cross-project child ids would represent a tenant
//     leak and are silently dropped by the JOIN's WHERE clause.
//
//   - SQLite's JSON1 extension is used to pull the typed fields out
//     of the payload column. This matches the pattern used by
//     FindFirstFailedValidator.
func (s *SQLiteStore) ListHandoffsWithChildStatus(
	ctx context.Context,
	parentExecutionID, projectID string,
) ([]HandoffWithChildStatus, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT
			json_extract(e.payload, '$.from_agent')         AS from_agent,
			json_extract(e.payload, '$.to_agent')           AS to_agent,
			json_extract(e.payload, '$.handoff_kind')       AS handoff_kind,
			json_extract(e.payload, '$.child_execution_id') AS child_execution_id,
			e.timestamp                                     AS handoff_emitted_at,
			c.execution_id                                  AS child_id_found,
			c.status                                        AS child_status,
			c.ended_at                                      AS child_ended_at
		FROM events e
		LEFT JOIN executions c
		  ON c.execution_id = json_extract(e.payload, '$.child_execution_id')
		 AND c.project_id   = ?
		WHERE e.execution_id = ?
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

// ListHandoffEdgesInTopology returns every agent_handoff edge
// emitted by the rootExecutionID's topology subtree (root +
// descendants reachable via parent_execution_id). Implemented as
// a recursive CTE collecting the subtree, then joined with
// events filtered to agent_handoff to pull the from/to labels.
func (s *SQLiteStore) ListHandoffEdgesInTopology(
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
			WHERE execution_id = ? AND project_id = ?
			UNION ALL
			SELECT e.execution_id, s.depth + 1
			FROM executions e
			JOIN subtree s ON e.parent_execution_id = s.execution_id
			WHERE e.project_id = ? AND s.depth < ?
		)
		SELECT
			e.execution_id,
			json_extract(e.payload, '$.from_agent') AS from_agent,
			json_extract(e.payload, '$.to_agent')   AS to_agent,
			e.timestamp                             AS emitted_at
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
		// Drop edges with missing labels; they can't participate
		// in a meaningful cycle and would corrupt the signature.
		if edge.FromAgent == "" || edge.ToAgent == "" {
			continue
		}
		out = append(out, edge)
	}
	return out, rows.Err()
}

// CountDistinctTenantsWithProviderError counts distinct tenant_id
// values across executions that emitted at least one llm_call
// event with the given provider + error_class since the supplied
// time. NULL tenant_id collapses to a single bucket and counts as
// one tenant when present. Used by the provider_incident detector
// .
//
// SQLite-flavor implementation note: the query uses a sub-select
// to extract distinct (execution_id × tenant_id) pairs that match
// the predicate, then COUNT(DISTINCT tenant_id) over the outer
// query. COALESCE(tenant_id, ”) folds NULLs into the empty
// string so the outer DISTINCT collapses them together rather
// than treating each NULL as its own value.
func (s *SQLiteStore) CountDistinctTenantsWithProviderError(
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
			WHERE x.project_id = ?
			  AND ev.event_type = 'llm_call'
			  AND json_extract(ev.payload, '$.provider')    = ?
			  AND json_extract(ev.payload, '$.error_class') = ?
			  AND x.started_at >= ?
		) AS x
	`, projectID, provider, errorClass, since).Scan(&n)
	if err != nil {
		return 0, fmt.Errorf("count distinct tenants with provider error: %w", err)
	}
	return n, nil
}

// ListHumanInterventionPayloads returns every human_intervention
// event payload on the execution in sequence order. Used by the
// hitl_timeout detector and the hitl_rejection_spike
// detector.
func (s *SQLiteStore) ListHumanInterventionPayloads(ctx context.Context, executionID string) ([][]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT payload
		FROM events
		WHERE execution_id = ?
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

// CountHITLOutcomesInWindow aggregates human_intervention
// verdicts across the project's recent executions. Counts are
// over distinct executions: an execution with multiple human
// inputs still counts once per outcome category. Used by the
// hitl_rejection_spike detector.
func (s *SQLiteStore) CountHITLOutcomesInWindow(
	ctx context.Context,
	projectID string,
	since time.Time,
) (HITLOutcomeCounts, error) {
	var counts HITLOutcomeCounts
	err := s.db.QueryRowContext(ctx, `
		SELECT
			COUNT(DISTINCT x.execution_id),
			COUNT(DISTINCT CASE
				WHEN json_extract(ev.payload, '$.response_kind') = 'rejected'
				THEN x.execution_id
			END),
			COUNT(DISTINCT CASE
				WHEN json_extract(ev.payload, '$.response_kind') = 'edited'
				THEN x.execution_id
			END)
		FROM executions x
		JOIN events ev ON ev.execution_id = x.execution_id
		WHERE x.project_id   = ?
		  AND ev.event_type  = 'human_intervention'
		  AND x.started_at  >= ?
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

// GetExecutionTopology walks the parent_execution_id graph rooted at
// the seed execution and returns every reachable node within the
// project. The traversal goes UP (ancestors) and DOWN (descendants)
// in two SQLite recursive CTEs unioned into one result set.
//
// SQLite-flavor implementation note: the two CTEs share the same
// starting execution but differ in direction; ancestors join on
// parent_execution_id, descendants on children-of-current. The depth
// guard is implemented as a level counter incremented per recursive
// step; the LIMIT is enforced via WHERE level < ?.
func (s *SQLiteStore) GetExecutionTopology(
	ctx context.Context,
	projectID, executionID string,
	maxDepth int,
) ([]TopologyNode, error) {
	if maxDepth <= 0 {
		maxDepth = 8 // safe default for visualization-friendly trees
	}
	// One query that unions ancestor traversal + descendant
	// traversal, dedupes on execution_id, and sorts by depth then
	// started_at. Depth is computed relative to the seed: negative
	// values for ancestors, zero for the seed, positive for
	// descendants. The dashboard normalizes depths to the smallest
	// observed value so the root renders at depth 0 visually.
	query := `
		WITH RECURSIVE
		ancestors(execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, depth) AS (
			SELECT execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, 0
			FROM executions
			WHERE execution_id = ? AND project_id = ?
			UNION ALL
			SELECT e.execution_id, e.parent_execution_id, e.status, e.started_at, e.ended_at, e.duration_ms, e.sdk_language, e.failure_group_id, a.depth - 1
			FROM executions e
			JOIN ancestors a ON e.execution_id = a.parent_execution_id
			WHERE e.project_id = ? AND a.depth > ?
		),
		descendants(execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, depth) AS (
			SELECT execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, 0
			FROM executions
			WHERE execution_id = ? AND project_id = ?
			UNION ALL
			SELECT e.execution_id, e.parent_execution_id, e.status, e.started_at, e.ended_at, e.duration_ms, e.sdk_language, e.failure_group_id, d.depth + 1
			FROM executions e
			JOIN descendants d ON e.parent_execution_id = d.execution_id
			WHERE e.project_id = ? AND d.depth < ?
		)
		SELECT execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, depth FROM ancestors
		UNION
		SELECT execution_id, parent_execution_id, status, started_at, ended_at, duration_ms, sdk_language, failure_group_id, depth FROM descendants
		ORDER BY depth ASC, started_at ASC
	`
	// Parameter 4 is the depth FLOOR for the ancestors CTE (a
	// negative value); parameter 8 is the depth CEILING for the
	// descendants CTE (positive). Negation happens in Go so the
	// query stays portable between SQLite and Postgres (Postgres
	// can't infer the type of a unary-minus on a placeholder).
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

// ListModelsForExecution returns the distinct set of model names from
// this execution's llm_call events, sorted alphabetically. Uses SQLite
// JSON1's json_extract to read payload.model. Returns empty slice (not
// nil) if no llm_call events have a model field.
func (s *SQLiteStore) ListModelsForExecution(
	ctx context.Context,
	executionID string,
) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT json_extract(payload, '$.model') AS model
		FROM events
		WHERE execution_id = ?
		  AND event_type = 'llm_call'
		  AND json_extract(payload, '$.model') IS NOT NULL
		  AND json_extract(payload, '$.model') != ''
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

// ListModelsForProjectSince returns distinct models seen in this
// project's llm_call events since the cutoff, EXCLUDING the given
// execution. Used by the drift detector to compute the historical
// model-mix baseline. Joins events ↔ executions on execution_id to
// scope by project; an indexed query on a hot path, but llm_call
// volume is modest enough at MVP scale that the join cost is fine.
func (s *SQLiteStore) ListModelsForProjectSince(
	ctx context.Context,
	projectID string,
	cutoff time.Time,
	excludeExecutionID string,
) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT DISTINCT json_extract(e.payload, '$.model') AS model
		FROM events e
		JOIN executions x ON x.execution_id = e.execution_id
		WHERE x.project_id = ?
		  AND e.event_type = 'llm_call'
		  AND e.timestamp >= ?
		  AND e.execution_id != ?
		  AND json_extract(e.payload, '$.model') IS NOT NULL
		  AND json_extract(e.payload, '$.model') != ''
		ORDER BY model ASC
	`, projectID, cutoff.UTC(), excludeExecutionID) // time.Time bound, NEVER Format(...): formats can't compare
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

// ListLLMUserMessagesForExecution returns user_messages from this
// execution's llm_call events, in sequence order. Empty / NULL
// user_messages are filtered out, they don't contribute lexical
// signal.
func (s *SQLiteStore) ListLLMUserMessagesForExecution(
	ctx context.Context,
	executionID string,
) ([]string, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT json_extract(payload, '$.user_message') AS user_message
		FROM events
		WHERE execution_id = ?
		  AND event_type = 'llm_call'
		  AND json_extract(payload, '$.user_message') IS NOT NULL
		  AND json_extract(payload, '$.user_message') != ''
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

// ListLLMUserMessagesForProjectSince returns user_messages from every
// llm_call event in this project's history since cutoff, excluding
// the current execution. Sorted by timestamp DESC (most recent first)
// so when callers apply a limit, they get the freshest signal.
//
// Bounded by limit. Pass 0 for "no limit", typically callers should
// pass 500 or 1000 for v0.0.1; once we have a project-volume signal,
// the limit becomes adaptive.
func (s *SQLiteStore) ListLLMUserMessagesForProjectSince(
	ctx context.Context,
	projectID string,
	cutoff time.Time,
	excludeExecutionID string,
	limit int,
) ([]string, error) {
	query := `
		SELECT json_extract(e.payload, '$.user_message') AS user_message
		FROM events e
		JOIN executions x ON x.execution_id = e.execution_id
		WHERE x.project_id = ?
		  AND e.event_type = 'llm_call'
		  AND e.timestamp >= ?
		  AND e.execution_id != ?
		  AND json_extract(e.payload, '$.user_message') IS NOT NULL
		  AND json_extract(e.payload, '$.user_message') != ''
		ORDER BY e.timestamp DESC
	`
	args := []interface{}{projectID, cutoff.UTC(), excludeExecutionID} // time.Time bound, NEVER Format(...): formats can't compare
	if limit > 0 {
		query += " LIMIT ?"
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
