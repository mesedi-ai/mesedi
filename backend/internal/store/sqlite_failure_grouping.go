// Failure grouping: signatures, group ids, and the Group* writers detectors call.
// Split out of sqlite.go on 2026-09-12; every declaration moved verbatim.
package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"fmt"
	"time"
)

// deriveGroupID returns a deterministic group_id for a given
// (project_id, failure_class, signature) tuple. Same inputs always
// produce the same output, across runs and across restarts, so no
// coordination is needed to look up "the" group for a signature.
//
// 16 hex chars from SHA-256 = 64 bits of entropy, which is comfortably
// collision-resistant for any realistic per-project failure-group
// volume (billions of distinct signatures before birthday-paradox
// collisions become measurable).
func deriveGroupID(projectID, failureClass, signature string) string {
	return DeriveFailureGroupID(projectID, failureClass, signature)
}

// DeriveFailureGroupID is the exported form of deriveGroupID for
// callers (api handlers) that need to compute a group_id without
// hitting the DB, used by the validator_failures.G1 post-step
// that updates severity_hint on the just-created row.
func DeriveFailureGroupID(projectID, failureClass, signature string) string {
	h := sha256.Sum256([]byte(projectID + "|" + failureClass + "|" + signature))
	return "grp-" + hex.EncodeToString(h[:8])
}

// groupExecutionInternal is the shared upsert path for all detection
// classes. Both GroupCrashedExecution and GroupTimeBudgetExceedance
// are thin wrappers around this, they just supply the appropriate
// failure_class + signature.
//
// Idempotency: if the execution already has a failure_group_id set
// (because it was already linked to a different group, or a previous
// call already linked it to this group), the function returns nil
// without double-counting. This is also how "crash classification
// wins over time-budget overlap" is enforced, the crash grouping
// runs first in the handler, sets failure_group_id, then the
// subsequent time-budget call short-circuits here.
func (s *SQLiteStore) groupExecutionInternal(
	ctx context.Context,
	executionID, projectID, failureClass, signature string,
) (isNew bool, err error) {
	if executionID == "" || projectID == "" || failureClass == "" || signature == "" {
		return false, fmt.Errorf("executionID, projectID, failureClass, signature all required")
	}

	// Confirm the execution row exists; capture its current primary
	// failure_group_id so we know whether to claim the primary slot.
	var primaryExisting sql.NullString
	err = s.db.QueryRowContext(
		ctx,
		`SELECT failure_group_id FROM executions WHERE execution_id = ?`,
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

	// Newness probe BEFORE the upsert so we can report isNew=true to
	// the caller for webhook escalation. Racy under concurrent
	// writers, both observers could see "not found" and both report
	// isNew=true. At Mesedi's current volume the worst case is
	// duplicate webhook deliveries, not data corruption.
	var existedBefore int
	err = s.db.QueryRowContext(
		ctx,
		`SELECT 1 FROM failure_groups WHERE group_id = ? LIMIT 1`,
		groupID,
	).Scan(&existedBefore)
	if err != nil && err != sql.ErrNoRows {
		return false, fmt.Errorf("probe failure_group existence: %w", err)
	}
	isNew = err == sql.ErrNoRows

	// CRITICAL ORDERING: upsert the failure_groups row BEFORE
	// inserting into the link table. The link table has a foreign
	// key on group_id; doing the link insert first against a
	// not-yet-created group violates the FK and the whole detector
	// silently fails ("link execution to failure_group: ERROR:
	// violates foreign key constraint"). The original migration-039
	// commit got this order backwards; SQLite tolerated it because
	// FK enforcement defaults off, Postgres surfaced it in
	// production. See lessons-learned for the full trace.
	//
	// Counters get +1 here on the assumption that the link insert
	// below WILL succeed (the common case). If the link turns out to
	// already exist, a true idempotent retry or a concurrent
	// writer winning the race, the decrement at the bottom of this
	// function compensates.
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO failure_groups (
			group_id, project_id, failure_class, signature,
			first_seen, last_seen,
			event_count, affected_executions,
			sample_execution_id
		)
		VALUES (?, ?, ?, ?, ?, ?, 1, 1, ?)
		ON CONFLICT(group_id) DO UPDATE SET
			event_count = event_count + 1,
			affected_executions = affected_executions + 1,
			last_seen = excluded.last_seen,
			-- Auto-reopen on recurrence. Twin of the Postgres upsert in
			-- postgres.go: see the full rationale there. Both stores
			-- MUST carry this: changing only one is the exact failure
			-- mode that caused the migration-056 production outage,
			-- and it would mean self-hosters on SQLite silently keep
			-- the old never-reopen behaviour.
			resolved_at = NULL,
			resolved_by = NULL
	`, groupID, projectID, failureClass, signature, now, now, executionID)
	if err != nil {
		return false, fmt.Errorf("upsert failure_group: %w", err)
	}

	// Now insert the link. PK (execution_id, group_id) +
	// ON CONFLICT DO NOTHING gives true idempotency.
	isPrimaryInt := 0
	if isPrimary {
		isPrimaryInt = 1
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO execution_failure_groups (
			execution_id, group_id, is_primary, classified_at
		)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(execution_id, group_id) DO NOTHING
	`, executionID, groupID, isPrimaryInt, now)
	if err != nil {
		return false, fmt.Errorf("link execution to failure_group: %w", err)
	}
	inserted, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("rows affected on membership insert: %w", err)
	}
	if inserted == 0 {
		// Link already existed (true idempotent retry, or a
		// concurrent writer beat us). The counter +1 above was
		// speculative; undo it here so affected_executions
		// continues to mean "distinct executions classified into
		// this group."
		_, derr := s.db.ExecContext(ctx, `
			UPDATE failure_groups
			SET event_count = event_count - 1,
			    affected_executions = affected_executions - 1
			WHERE group_id = ?
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

	// Claim the primary slot if no detector has yet. Skipping when
	// already populated preserves the v002 first-detector-wins
	// behavior the dashboard's executions detail page still renders.
	if isPrimary {
		_, err = s.db.ExecContext(
			ctx,
			`UPDATE executions SET failure_group_id = ? WHERE execution_id = ?`,
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

// GroupCrashedExecution upserts a failure_group with failure_class=crashes
// for the given execution. Thin wrapper around groupExecutionInternal.
func (s *SQLiteStore) GroupCrashedExecution(
	ctx context.Context,
	executionID, projectID, signature string,
) (isNew bool, err error) {
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassCrashes, signature)
}

// timeBudgetThresholdMs is the hardcoded cutoff for "this execution
// took too long" detection in v0.0.1. Set artificially low (1s) for
// local-dev visibility; production default will be 60s (or 10min per
// the concept-doc step-budget detector spec) and configurable per
// project once the projects table gets per-project policy columns.
const timeBudgetThresholdMs int64 = 1000

// TimeBudgetSignature returns a coarse duration-bucket label so that
// "long-running executions" cluster into a small number of groups
// rather than one group per unique millisecond. Buckets: 1s+, 10s+,
// 60s+, 10m+, 1h+. Anything below the threshold is filtered upstream
// in the handler; this function assumes a positive duration that has
// already exceeded the threshold.
func TimeBudgetSignature(durationMs int64) string {
	switch {
	case durationMs < 10_000:
		return "time_budget_1s+"
	case durationMs < 60_000:
		return "time_budget_10s+"
	case durationMs < 600_000:
		return "time_budget_60s+"
	case durationMs < 3_600_000:
		return "time_budget_10m+"
	default:
		return "time_budget_1h+"
	}
}

// GroupTimeBudgetExceedance upserts a failure_group with
// failure_class=loops and a duration-bucketed signature. Called from
// HandleUpdateExecution after the crash check, so crash-classified
// executions are already linked to a crashes group and this call
// becomes a no-op via the idempotency check.
func (s *SQLiteStore) GroupTimeBudgetExceedance(
	ctx context.Context,
	executionID, projectID string,
	durationMs int64,
) (isNew bool, err error) {
	signature := TimeBudgetSignature(durationMs)
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassLoops, signature)
}

// StepCountSignature buckets event counts so high-step-count executions
// cluster into a small number of groups rather than one group per
// distinct count. Buckets: 10+, 50+, 100+, 500+, 5000+. Anything below
// the threshold is filtered upstream in the handler.
func StepCountSignature(count int) string {
	switch {
	case count < 50:
		return "step_count_10+"
	case count < 100:
		return "step_count_50+"
	case count < 500:
		return "step_count_100+"
	case count < 5_000:
		return "step_count_500+"
	default:
		return "step_count_5000+"
	}
}

// ThrottlingSignature builds the cluster signature for an
// infrastructure_throttled grouping from the (provider, dimension,
// circuit-state) tuple captured on an InfrastructureEventPayload.
// Signatures intentionally collapse:
//
//   - All "rate_limit" events for the same (provider, quota_dimension)
//     into one group. SREs care about "Anthropic is rate-limiting our
//     tokens_per_minute," not about which exact agent's call hit it.
//
//   - "circuit_breaker" trips by (provider, circuit_state) so the
//     "half_open re-test failed" pattern stays distinct from the
//     "open trip" pattern.
//
//   - "quota_exhausted" by provider only (these are hard caps that
//     don't care about dimension).
//
// Format: "<reason>:<provider>" or "<reason>:<provider>:<dim>".
// Unknown providers fall back to "unknown" so the signature is still
// stable. The handler filters out events with empty Provider before
// reaching this function.
func ThrottlingSignature(reason, provider, dimension, circuitState string) string {
	if provider == "" {
		provider = "unknown"
	}
	switch reason {
	case "rate_limit":
		if dimension == "" {
			return "rate_limit:" + provider
		}
		return "rate_limit:" + provider + ":" + dimension
	case "circuit_breaker":
		if circuitState == "" {
			circuitState = "open"
		}
		return "circuit_breaker:" + provider + ":" + circuitState
	case "quota_exhausted":
		return "quota_exhausted:" + provider
	default:
		// Future-proof: unknown reasons cluster by provider so they're
		// at least groupable, not exploded one-per-execution.
		return reason + ":" + provider
	}
}

// GroupStepCountExceedance upserts a failure_group with
// failure_class=loops and an event-count-bucketed signature. Same
// idempotency contract as the other groupers, runs in the handler
// AFTER both crash and time-budget checks, so it's the lowest-priority
// classification of the three.
func (s *SQLiteStore) GroupStepCountExceedance(
	ctx context.Context,
	executionID, projectID string,
	eventCount int,
) (isNew bool, err error) {
	signature := StepCountSignature(eventCount)
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassLoops, signature)
}

// GroupToolFailure upserts a failure_group with
// failure_class=tool_failures and signature=signature. Same idempotency
// contract as the other groupers, if the execution is already linked
// to a higher-priority group (crash, time-budget, step-count), this is
// a no-op.
//
// granular-sig wave: signature is now "<tool>:<exception_type>" when
// the caller has access to exception_type from the failed tool_call
// event; falls back to "<tool>" for backward compat. The store layer
// is signature-agnostic, concat happens in the handler.
func (s *SQLiteStore) GroupToolFailure(
	ctx context.Context,
	executionID, projectID, signature string,
) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassToolFailures, signature)
}

// GroupInfrastructureThrottled upserts a failure_group with
// failure_class=infrastructure_throttled and the caller-supplied
// signature (built by ThrottlingSignature). Same idempotency
// contract as the other groupers: if the execution is already linked
// to a higher-priority group (crash, time-budget, step-count), this
// is a no-op.
func (s *SQLiteStore) GroupInfrastructureThrottled(
	ctx context.Context,
	executionID, projectID, signature string,
) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassInfraThrottled, signature)
}

// GroupDataLeakage upserts a failure_group with
// failure_class=data_leakage and signature=ruleID (e.g.
// "aws_access_key"). One group per rule per project. Idempotent: if
// the execution is already linked to a higher-priority group, this
// is a no-op.
func (s *SQLiteStore) GroupDataLeakage(
	ctx context.Context,
	executionID, projectID, ruleID string,
) (isNew bool, err error) {
	if ruleID == "" {
		return false, fmt.Errorf("ruleID required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassDataLeakage, ruleID)
}

// GroupSemanticLoop upserts a failure_group with
// failure_class=semantic_loop and the caller-supplied signature.
func (s *SQLiteStore) GroupSemanticLoop(
	ctx context.Context,
	executionID, projectID, signature string,
) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassSemanticLoop, signature)
}

// GroupToolSchemaDrift upserts a failure_group with
// failure_class=tool_schema_drift and the caller-supplied signature.
func (s *SQLiteStore) GroupToolSchemaDrift(
	ctx context.Context,
	executionID, projectID, signature string,
) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassToolSchemaDrift, signature)
}

// GroupContextOverflow upserts a failure_group with
// failure_class=context_overflow and the caller-supplied signature.
func (s *SQLiteStore) GroupContextOverflow(
	ctx context.Context,
	executionID, projectID, signature string,
) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassContextOverflow, signature)
}

// GroupTokenWaste upserts a failure_group with
// failure_class=token_waste and the caller-supplied signature.
func (s *SQLiteStore) GroupTokenWaste(
	ctx context.Context,
	executionID, projectID, signature string,
) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassTokenWaste, signature)
}

// GroupSandboxEscape upserts a failure_group with
// failure_class=sandbox_escape and the caller-supplied signature.
func (s *SQLiteStore) GroupSandboxEscape(
	ctx context.Context,
	executionID, projectID, signature string,
) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassSandboxEscape, signature)
}

// GroupGroundingFailure upserts a failure_group with
// failure_class=grounding_failure and the caller-supplied signature.
func (s *SQLiteStore) GroupGroundingFailure(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassGroundingFailure, signature)
}

// GroupCascadingFailure upserts a failure_group with
// failure_class=cascading_failure and the detector-supplied
// signature.
func (s *SQLiteStore) GroupCascadingFailure(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassCascadingFailure, signature)
}

// GroupCoordinationDeadlock upserts a failure_group with
// failure_class=coordination_deadlock and the detector-supplied
// signature.
func (s *SQLiteStore) GroupCoordinationDeadlock(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassCoordinationDeadlock, signature)
}

// GroupProviderIncident upserts a failure_group with
// failure_class=provider_incident and the detector-supplied
// signature.
func (s *SQLiteStore) GroupProviderIncident(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassProviderIncident, signature)
}

// GroupHITLTimeout upserts a failure_group with
// failure_class=hitl_timeout and the detector-supplied signature.
func (s *SQLiteStore) GroupHITLTimeout(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassHITLTimeout, signature)
}

// GroupRecordIntegrity upserts a failure_group with
// failure_class=record_integrity. Thin wrapper over the shared
// grouping path, identical in shape to every other Group* method ,
// this detector needs no special persistence, only a distinct class.
func (s *SQLiteStore) GroupRecordIntegrity(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassRecordIntegrity, signature)
}

// GroupHITLRejectionSpike upserts a failure_group with
// failure_class=hitl_rejection_spike and the detector-supplied
// signature.
func (s *SQLiteStore) GroupHITLRejectionSpike(ctx context.Context, executionID, projectID, signature string) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassHITLRejectionSpike, signature)
}

// GroupValidatorFailure upserts a failure_group with
// failure_class=validator_failures and signature=validatorName. Same
// idempotency contract.
func (s *SQLiteStore) GroupValidatorFailure(
	ctx context.Context,
	executionID, projectID, validatorName string,
) (isNew bool, err error) {
	if validatorName == "" {
		return false, fmt.Errorf("validatorName required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassValidator, validatorName)
}

// GroupPromptInjection upserts a failure_group with
// failure_class=prompt_injection and signature=patternName. Detection
// logic (regex pattern matching) lives in detectors/injection.go;
// this method just records the classification.
func (s *SQLiteStore) GroupPromptInjection(
	ctx context.Context,
	executionID, projectID, patternName string,
) (isNew bool, err error) {
	if patternName == "" {
		return false, fmt.Errorf("patternName required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassInjection, patternName)
}

// GroupIdenticalCallLoop upserts a failure_group with
// failure_class=loops and signature="identical_call_<callHash>".
// callHash is computed in the handler from (model + user_message) and
// truncated to a short hex prefix. Same idempotency contract.
func (s *SQLiteStore) GroupIdenticalCallLoop(
	ctx context.Context,
	executionID, projectID, callHash string,
) (isNew bool, err error) {
	if callHash == "" {
		return false, fmt.Errorf("callHash required")
	}
	signature := "identical_call_" + callHash
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassLoops, signature)
}

// GroupSimilarCallLoop upserts a failure_group with
// failure_class=loops and signature="similar_call_<callHash>".
// callHash is computed in the handler as a hash of the dominant
// trigrams in the cluster, different stuck-pattern clusters get
// different signatures so they aggregate as distinct rows in the
// dashboard.
func (s *SQLiteStore) GroupSimilarCallLoop(
	ctx context.Context,
	executionID, projectID, callHash string,
) (isNew bool, err error) {
	if callHash == "" {
		return false, fmt.Errorf("callHash required")
	}
	signature := "similar_call_" + callHash
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassLoops, signature)
}

// GroupDriftSignal upserts a failure_group with failure_class=drift
// and the caller-supplied signature. Same idempotency contract as the
// other groupers, if the execution is already in a higher-priority
// group (crash, injection), this is a no-op.
func (s *SQLiteStore) GroupDriftSignal(
	ctx context.Context,
	executionID, projectID, signature string,
) (isNew bool, err error) {
	if signature == "" {
		return false, fmt.Errorf("drift signature required")
	}
	return s.groupExecutionInternal(ctx, executionID, projectID, FailureClassDrift, signature)
}
