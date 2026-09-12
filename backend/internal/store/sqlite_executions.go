// Execution write path: create, pause, resume, update, events, cost.
// Split out of sqlite.go on 2026-09-12; every declaration moved verbatim.
package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

func (s *SQLiteStore) CreateExecution(ctx context.Context, e *events.Execution) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO executions (
			execution_id, project_id, parent_execution_id, status,
			started_at, ended_at, duration_ms,
			total_tokens_in, total_tokens_out, estimated_cost_usd,
			input_summary, output_summary, crash_signature,
			sdk_version, sdk_language, tenant_id, api_key_id
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`,
		e.ExecutionID, e.ProjectID, nullStringPtr(e.ParentExecutionID), e.Status,
		e.StartedAt, nullTime(e.EndedAt), nullInt64(e.DurationMs),
		nullInt(e.TotalTokensIn), nullInt(e.TotalTokensOut), nullFloat(e.EstimatedCostUSD),
		nullString(e.InputSummary), nullString(e.OutputSummary), nullString(e.CrashSignature),
		nullString(e.SDKVersion), nullString(e.SDKLanguage), nullStringPtr(e.TenantID),
		nullStringPtr(e.APIKeyID),
	)
	if err != nil {
		return fmt.Errorf("insert execution: %w", err)
	}
	return nil
}

// PauseExecution transitions a started execution into the
// awaiting_human state. Atomic: the WHERE clause
// guarantees the transition only succeeds from `started`; if the
// execution is in any other state, RowsAffected is 0 and we
// translate that into ErrInvalidLifecycleTransition (vs. plain
// ErrNotFound). pause_count is incremented unconditionally on the
// successful transition so the hitl_rejection_spike detector
// can read it as cumulative HITL cycle count without further
// computation.
func (s *SQLiteStore) PauseExecution(ctx context.Context, executionID, projectID string, pausedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE executions SET
			status      = 'awaiting_human',
			paused_at   = ?,
			pause_count = pause_count + 1
		WHERE execution_id = ?
		  AND project_id   = ?
		  AND status       = 'started'
	`, pausedAt, executionID, projectID)
	if err != nil {
		return fmt.Errorf("pause execution: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		// Distinguish "execution does not exist in this project"
		// from "execution exists but is not in `started` state".
		var status sql.NullString
		qErr := s.db.QueryRowContext(ctx, `
			SELECT status FROM executions
			WHERE execution_id = ? AND project_id = ?
		`, executionID, projectID).Scan(&status)
		if errors.Is(qErr, sql.ErrNoRows) {
			return ErrNotFound
		}
		if qErr != nil {
			return fmt.Errorf("pause execution status probe: %w", qErr)
		}
		return ErrInvalidLifecycleTransition
	}
	return nil
}

// ResumeExecution transitions an awaiting_human execution back to
// started. Computes (resumedAt - paused_at) in
// milliseconds and adds it to total_paused_ms, then clears
// paused_at. SQLite stores TIMESTAMP as ISO8601 text; the
// (resumedAt - paused_at) arithmetic is performed in Go after
// reading the prior paused_at, then written in the same statement
// to keep the operation a single round-trip. We read the prior
// paused_at via a returning sub-select pattern (SQLite supports
// this in 3.35+; the deployed binary is well past that).
func (s *SQLiteStore) ResumeExecution(ctx context.Context, executionID, projectID string, resumedAt time.Time) error {
	// Read prior paused_at to compute delta. Doing this in two
	// statements rather than one expression keeps SQLite's
	// julianday() arithmetic out of the write path (its precision
	// is documented as "to within milliseconds" but loses fidelity
	// at millisecond granularity for very short pauses).
	var pausedAt sql.NullTime
	var status sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT paused_at, status FROM executions
		WHERE execution_id = ? AND project_id = ?
	`, executionID, projectID).Scan(&pausedAt, &status)
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("resume execution status probe: %w", err)
	}
	if status.String != "awaiting_human" || !pausedAt.Valid {
		return ErrInvalidLifecycleTransition
	}
	deltaMs := resumedAt.Sub(pausedAt.Time).Milliseconds()
	if deltaMs < 0 {
		// Clock skew defensive: a backwards delta would corrupt
		// total_paused_ms. Treat as zero rather than abort the
		// resume; the agent should not stay paused due to wall
		// clock issues.
		deltaMs = 0
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE executions SET
			status          = 'started',
			paused_at       = NULL,
			total_paused_ms = total_paused_ms + ?
		WHERE execution_id = ?
		  AND project_id   = ?
		  AND status       = 'awaiting_human'
	`, deltaMs, executionID, projectID)
	if err != nil {
		return fmt.Errorf("resume execution: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		// Raced with another writer; the status changed between
		// our probe and our update.
		return ErrInvalidLifecycleTransition
	}
	return nil
}

func (s *SQLiteStore) UpdateExecution(ctx context.Context, e *events.Execution) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE executions SET
			status              = COALESCE(NULLIF(?, ''), status),
			ended_at            = COALESCE(?, ended_at),
			duration_ms         = COALESCE(?, duration_ms),
			total_tokens_in     = COALESCE(?, total_tokens_in),
			total_tokens_out    = COALESCE(?, total_tokens_out),
			estimated_cost_usd  = COALESCE(?, estimated_cost_usd),
			output_summary      = COALESCE(NULLIF(?, ''), output_summary),
			crash_signature     = COALESCE(NULLIF(?, ''), crash_signature)
		WHERE execution_id = ?
	`,
		string(e.Status), nullTime(e.EndedAt), nullInt64(e.DurationMs),
		nullInt(e.TotalTokensIn), nullInt(e.TotalTokensOut), nullFloat(e.EstimatedCostUSD),
		e.OutputSummary, e.CrashSignature,
		e.ExecutionID,
	)
	if err != nil {
		return fmt.Errorf("update execution: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *SQLiteStore) GetExecution(ctx context.Context, executionID string) (*events.Execution, error) {
	e := &events.Execution{}
	var parent, inputSum, outputSum, crashSig, sdkVer, sdkLang, failureGroupID, tenantID, apiKeyID sql.NullString
	var endedAt, pausedAt sql.NullTime
	var durationMs, tokensIn, tokensOut, totalPausedMs sql.NullInt64
	var pauseCount sql.NullInt64
	var costUSD sql.NullFloat64
	err := s.db.QueryRowContext(ctx, `
		SELECT
			execution_id, project_id, parent_execution_id, status,
			started_at, ended_at, duration_ms,
			total_tokens_in, total_tokens_out, estimated_cost_usd,
			input_summary, output_summary, crash_signature,
			sdk_version, sdk_language, failure_group_id, tenant_id,
			paused_at, total_paused_ms, pause_count, api_key_id
		FROM executions WHERE execution_id = ?
	`, executionID).Scan(
		&e.ExecutionID, &e.ProjectID, &parent, &e.Status,
		&e.StartedAt, &endedAt, &durationMs,
		&tokensIn, &tokensOut, &costUSD,
		&inputSum, &outputSum, &crashSig,
		&sdkVer, &sdkLang, &failureGroupID, &tenantID,
		&pausedAt, &totalPausedMs, &pauseCount, &apiKeyID,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if parent.Valid {
		v := parent.String
		e.ParentExecutionID = &v
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
	if inputSum.Valid {
		e.InputSummary = inputSum.String
	}
	if outputSum.Valid {
		e.OutputSummary = outputSum.String
	}
	if crashSig.Valid {
		e.CrashSignature = crashSig.String
	}
	if sdkVer.Valid {
		e.SDKVersion = sdkVer.String
	}
	if sdkLang.Valid {
		e.SDKLanguage = sdkLang.String
	}
	if failureGroupID.Valid {
		v := failureGroupID.String
		e.FailureGroupID = &v
	}
	if tenantID.Valid {
		v := tenantID.String
		e.TenantID = &v
	}
	if pausedAt.Valid {
		t := pausedAt.Time
		e.PausedAt = &t
	}
	if totalPausedMs.Valid {
		e.TotalPausedMs = totalPausedMs.Int64
	}
	if pauseCount.Valid {
		e.PauseCount = int(pauseCount.Int64)
	}
	if apiKeyID.Valid {
		v := apiKeyID.String
		e.APIKeyID = &v
	}
	return e, nil
}

func (s *SQLiteStore) SaveEvents(ctx context.Context, batch []events.Event) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback() // safe to call after Commit, becomes a no-op

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO events (event_id, execution_id, event_type, sequence, timestamp, duration_ms, payload)
		VALUES (?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		return fmt.Errorf("prepare event insert: %w", err)
	}
	defer stmt.Close()

	for i := range batch {
		evt := &batch[i]
		payload := []byte(evt.Payload)
		if len(payload) == 0 {
			payload = []byte("null")
		}
		if !json.Valid(payload) {
			return fmt.Errorf("event %s: invalid JSON payload", evt.EventID)
		}
		if _, err := stmt.ExecContext(ctx,
			evt.EventID, evt.ExecutionID, evt.EventType, evt.Sequence,
			evt.Timestamp, nullInt64(evt.DurationMs), string(payload),
		); err != nil {
			return fmt.Errorf("insert event %s: %w", evt.EventID, err)
		}
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit events: %w", err)
	}
	return nil
}

// SetExecutionCost writes a computed estimated_cost_usd onto an
// execution row. No-op if cost is non-positive (we don't want to
// overwrite an existing positive cost with 0 from a model whose
// pricing isn't in the table). Used by the post-PATCH cost aggregator
// in HandleUpdateExecution.
func (s *SQLiteStore) SetExecutionCost(
	ctx context.Context,
	executionID string,
	cost float64,
) error {
	if cost <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE executions SET estimated_cost_usd = ? WHERE execution_id = ?`,
		cost,
		executionID,
	)
	if err != nil {
		return fmt.Errorf("set execution cost: %w", err)
	}
	return nil
}
