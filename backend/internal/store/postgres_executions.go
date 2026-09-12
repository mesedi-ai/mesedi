// Execution write path: create, pause, resume, update, events, cost.
// Split out of postgres.go on 2026-09-12; every declaration moved verbatim.
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

func (s *PostgresStore) CreateExecution(ctx context.Context, e *events.Execution) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO executions (
			execution_id, project_id, parent_execution_id, status,
			started_at, ended_at, duration_ms,
			total_tokens_in, total_tokens_out, estimated_cost_usd,
			input_summary, output_summary, crash_signature,
			sdk_version, sdk_language, tenant_id, api_key_id
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
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

// PauseExecution is the Postgres twin of the SQLite method of the
// same name. .
func (s *PostgresStore) PauseExecution(ctx context.Context, executionID, projectID string, pausedAt time.Time) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE executions SET
			status      = 'awaiting_human',
			paused_at   = $1,
			pause_count = pause_count + 1
		WHERE execution_id = $2
		  AND project_id   = $3
		  AND status       = 'started'
	`, pausedAt, executionID, projectID)
	if err != nil {
		return fmt.Errorf("pause execution: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		var status sql.NullString
		qErr := s.db.QueryRowContext(ctx, `
			SELECT status FROM executions
			WHERE execution_id = $1 AND project_id = $2
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

// ResumeExecution is the Postgres twin of the SQLite method of the
// same name. .
func (s *PostgresStore) ResumeExecution(ctx context.Context, executionID, projectID string, resumedAt time.Time) error {
	var pausedAt sql.NullTime
	var status sql.NullString
	err := s.db.QueryRowContext(ctx, `
		SELECT paused_at, status FROM executions
		WHERE execution_id = $1 AND project_id = $2
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
		deltaMs = 0
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE executions SET
			status          = 'started',
			paused_at       = NULL,
			total_paused_ms = total_paused_ms + $1
		WHERE execution_id = $2
		  AND project_id   = $3
		  AND status       = 'awaiting_human'
	`, deltaMs, executionID, projectID)
	if err != nil {
		return fmt.Errorf("resume execution: %w", err)
	}
	rows, _ := res.RowsAffected()
	if rows == 0 {
		return ErrInvalidLifecycleTransition
	}
	return nil
}

func (s *PostgresStore) UpdateExecution(ctx context.Context, e *events.Execution) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE executions SET
			status              = COALESCE(NULLIF($1, ''), status),
			ended_at            = COALESCE($2, ended_at),
			duration_ms         = COALESCE($3, duration_ms),
			total_tokens_in     = COALESCE($4, total_tokens_in),
			total_tokens_out    = COALESCE($5, total_tokens_out),
			estimated_cost_usd  = COALESCE($6, estimated_cost_usd),
			output_summary      = COALESCE(NULLIF($7, ''), output_summary),
			crash_signature     = COALESCE(NULLIF($8, ''), crash_signature)
		WHERE execution_id = $9
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

func (s *PostgresStore) GetExecution(ctx context.Context, executionID string) (*events.Execution, error) {
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
		FROM executions WHERE execution_id = $1
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

func (s *PostgresStore) SaveEvents(ctx context.Context, batch []events.Event) error {
	if len(batch) == 0 {
		return nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin tx: %w", err)
	}
	defer tx.Rollback()

	stmt, err := tx.PrepareContext(ctx, `
		INSERT INTO events (event_id, execution_id, event_type, sequence, timestamp, duration_ms, payload)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
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

func (s *PostgresStore) SetExecutionCost(ctx context.Context, executionID string, cost float64) error {
	if cost <= 0 {
		return nil
	}
	_, err := s.db.ExecContext(
		ctx,
		`UPDATE executions SET estimated_cost_usd = $1 WHERE execution_id = $2`,
		cost,
		executionID,
	)
	if err != nil {
		return fmt.Errorf("set execution cost: %w", err)
	}
	return nil
}
