// Failure grouping: signatures, group ids, and the Group* writers detectors call.
// Split out of postgres.go on 2026-09-12; every declaration moved verbatim.
package store

import (
	"context"
	"database/sql"
	"fmt"
	"time"
)

// groupExecutionInternalPg is the postgres counterpart to
// SQLiteStore.groupExecutionInternal. Same idempotency contract, same
// isNew semantics, but uses $N placeholders.
func (s *PostgresStore) groupExecutionInternalPg(
	ctx context.Context,
	executionID, projectID, failureClass, signature string,
) (isNew bool, err error) {
	// Postgres twin of sqlite.go groupExecutionInternal. The order
	// of operations is CRITICAL: failure_groups must be upserted
	// BEFORE the link insert because the link table has a foreign
	// key on group_id, and Postgres enforces FKs by default. The
	// original migration-039 commit had the order reversed; SQLite
	// tolerated it (FKs default off), Postgres surfaced it in
	// production as "violates foreign key constraint" warnings on
	// every brand-new group. See lessons-learned for the trace.
	if executionID == "" || projectID == "" || failureClass == "" || signature == "" {
		return false, fmt.Errorf("executionID, projectID, failureClass, signature all required")
	}

	var primaryExisting sql.NullString
	err = s.db.QueryRowContext(
		ctx,
		`SELECT failure_group_id FROM executions WHERE execution_id = $1`,
		executionID,
	).Scan(&primaryExisting)
	if err == sql.ErrNoRows {
		return false, ErrNotFound
	}
	if err != nil {
		return false, fmt.Errorf("read execution failure_group_id: %w", err)
	}
	isPrimary := !primaryExisting.Valid || primaryExisting.String == ""

	groupID := deriveGroupID(projectID, failureClass, signature)
	now := time.Now().UTC().Format(time.RFC3339)

	// Newness probe BEFORE the upsert for webhook escalation
	// semantics. Racy but acceptable at current volume.
	var existedBefore int
	err = s.db.QueryRowContext(
		ctx,
		`SELECT 1 FROM failure_groups WHERE group_id = $1 LIMIT 1`,
		groupID,
	).Scan(&existedBefore)
	if err != nil && err != sql.ErrNoRows {
		return false, fmt.Errorf("probe failure_group existence: %w", err)
	}
	isNew = err == sql.ErrNoRows

	// Step 1: upsert failure_groups so the FK target exists. Counters
	// get +1 speculatively on the assumption that the link insert
	// below will succeed (the common case). If the link turns out to
	// already exist, the decrement at the bottom of this function
	// compensates.
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO failure_groups (
			group_id, project_id, failure_class, signature,
			first_seen, last_seen,
			event_count, affected_executions,
			sample_execution_id
		)
		VALUES ($1, $2, $3, $4, $5, $6, 1, 1, $7)
		ON CONFLICT(group_id) DO UPDATE SET
			event_count = failure_groups.event_count + 1,
			affected_executions = failure_groups.affected_executions + 1,
			last_seen = excluded.last_seen,
			-- Auto-reopen on recurrence. A resolved group that fires
			-- again is, by definition, not resolved.
			--
			-- Before this, resolved_at was never cleared: the counters
			-- kept climbing behind a row the customer could no longer
			-- see (the list filters resolved_at IS NULL) and no webhook
			-- fired because the class was not new. A customer could
			-- ship a fix that did not work, click Resolve, and receive
			-- no signal that the failure was still happening, the
			-- product having told them it was handled.
			--
			-- Matches Sentry, which reopens an issue on regression.
			-- That is what "Resolve" means to an engineer, and Mesedi
			-- is positioned as Sentry for AI agents.
			--
			-- resolved_by is cleared too, so the audit trail does not
			-- credit the reopened state to whoever closed it last.
			resolved_at = NULL,
			resolved_by = NULL
	`, groupID, projectID, failureClass, signature, now, now, executionID)
	if err != nil {
		return false, fmt.Errorf("upsert failure_group: %w", err)
	}

	// Step 2: insert the link. FK on group_id is now satisfied.
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO execution_failure_groups (
			execution_id, group_id, is_primary, classified_at
		)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT(execution_id, group_id) DO NOTHING
	`, executionID, groupID, isPrimary, now)
	if err != nil {
		return false, fmt.Errorf("link execution to failure_group: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected on membership insert: %w", err)
	}
	if inserted == 0 {
		// Link already existed; undo the speculative counter
		// increment so affected_executions stays accurate.
		_, derr := s.db.ExecContext(ctx, `
			UPDATE failure_groups
			SET event_count = failure_groups.event_count - 1,
			    affected_executions = failure_groups.affected_executions - 1
			WHERE group_id = $1
		`, groupID)
		if derr != nil {
			s.logger.Warn("failed to undo speculative counter increment after link conflict",
				"execution_id", executionID,
				"failure_group_id", groupID,
				"error", derr.Error(),
			)
		}
		return false, nil
	}

	if isPrimary {
		_, err = s.db.ExecContext(
			ctx,
			`UPDATE executions SET failure_group_id = $1 WHERE execution_id = $2`,
			groupID,
			executionID,
		)
		if err != nil {
			return false, fmt.Errorf("set primary failure_group on execution: %w", err)
		}
	}

	s.logger.Info("execution grouped",
		"execution_id", executionID,
		"failure_group_id", groupID,
		"failure_class", failureClass,
		"signature", signature,
		"is_new_group", isNew,
		"is_primary", isPrimary,
	)
	return isNew, nil
}

func (s *PostgresStore) GroupCrashedExecution(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassCrashes, signature)
}

func (s *PostgresStore) GroupTimeBudgetExceedance(ctx context.Context, executionID, projectID string, durationMs int64) (isNew bool, err error) {
	signature := TimeBudgetSignature(durationMs)
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassLoops, signature)
}

func (s *PostgresStore) GroupStepCountExceedance(ctx context.Context, executionID, projectID string, eventCount int) (isNew bool, err error) {
	signature := StepCountSignature(eventCount)
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassLoops, signature)
}

func (s *PostgresStore) GroupToolFailure(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassToolFailures, signature)
}

// GroupInfrastructureThrottled is the Postgres twin of the SQLite
// method of the same name.
func (s *PostgresStore) GroupInfrastructureThrottled(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassInfraThrottled, signature)
}

// GroupDataLeakage is the Postgres twin of the SQLite method of the
// same name.
func (s *PostgresStore) GroupDataLeakage(ctx context.Context, executionID, projectID, ruleID string) (isNew bool, err error) {
	if ruleID == "" {
		return false, fmt.Errorf("ruleID required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassDataLeakage, ruleID)
}

// GroupSemanticLoop is the Postgres twin of the SQLite method of the
// same name.
func (s *PostgresStore) GroupSemanticLoop(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassSemanticLoop, signature)
}

// GroupToolSchemaDrift is the Postgres twin of the SQLite method of
// the same name.
func (s *PostgresStore) GroupToolSchemaDrift(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassToolSchemaDrift, signature)
}

// GroupContextOverflow is the Postgres twin of the SQLite method of
// the same name.
func (s *PostgresStore) GroupContextOverflow(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassContextOverflow, signature)
}

// GroupTokenWaste is the Postgres twin of the SQLite method of the
// same name.
func (s *PostgresStore) GroupTokenWaste(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassTokenWaste, signature)
}

// GroupSandboxEscape is the Postgres twin of the SQLite method of
// the same name.
func (s *PostgresStore) GroupSandboxEscape(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassSandboxEscape, signature)
}

// GroupGroundingFailure is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) GroupGroundingFailure(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassGroundingFailure, signature)
}

// GroupCascadingFailure is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) GroupCascadingFailure(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassCascadingFailure, signature)
}

// GroupCoordinationDeadlock is the Postgres twin of the SQLite
// method of the same name.
func (s *PostgresStore) GroupCoordinationDeadlock(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassCoordinationDeadlock, signature)
}

// GroupProviderIncident is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) GroupProviderIncident(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassProviderIncident, signature)
}

// GroupHITLTimeout is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) GroupHITLTimeout(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassHITLTimeout, signature)
}

// GroupRecordIntegrity is the Postgres twin of the SQLite method of
// the same name.
func (s *PostgresStore) GroupRecordIntegrity(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassRecordIntegrity, signature)
}

// GroupHITLRejectionSpike is the Postgres twin of the SQLite method
// of the same name.
func (s *PostgresStore) GroupHITLRejectionSpike(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassHITLRejectionSpike, signature)
}

func (s *PostgresStore) GroupValidatorFailure(ctx context.Context, executionID, projectID, validatorName string) (isNew bool, err error) {
	if validatorName == "" {
		return false, fmt.Errorf("validatorName required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassValidator, validatorName)
}

func (s *PostgresStore) GroupPromptInjection(ctx context.Context, executionID, projectID, patternName string) (isNew bool, err error) {
	if patternName == "" {
		return false, fmt.Errorf("patternName required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassInjection, patternName)
}

func (s *PostgresStore) GroupIdenticalCallLoop(ctx context.Context, executionID, projectID, callHash string) (isNew bool, err error) {
	if callHash == "" {
		return false, fmt.Errorf("callHash required")
	}
	signature := "identical_call_" + callHash
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassLoops, signature)
}

func (s *PostgresStore) GroupSimilarCallLoop(ctx context.Context, executionID, projectID, callHash string) (isNew bool, err error) {
	if callHash == "" {
		return false, fmt.Errorf("callHash required")
	}
	signature := "similar_call_" + callHash
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassLoops, signature)
}

func (s *PostgresStore) GroupDriftSignal(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("drift signature required")
	}
	return s.groupExecutionInternalPg(ctx, executionID, projectID, FailureClassDrift, signature)
}
