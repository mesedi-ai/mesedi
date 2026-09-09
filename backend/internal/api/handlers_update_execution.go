// HandleUpdateExecution and nothing else. Moved verbatim from
// handlers.go on 2026-09-09 (task #35): 1,501 lines, a quarter of
// that file in one function, next largest 334. This move changes no
// behaviour; Go does not care which file a function lives in. It
// exists so the interior can be carved into named stages in its own
// home, which is what unblocks the cost-velocity attribution work
// (#48) and wiring the schema-drift classifier (#49a).
package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"mesedi/backend/internal/detectors"
	"mesedi/backend/internal/events"
	"mesedi/backend/internal/pricing"
	"mesedi/backend/internal/store"
)

// HandleUpdateExecution marks an existing execution as completed, crashed,
// halted, etc. Idempotent, repeated PATCH calls with the same status are
// silently accepted.
//
// Phase 3a addition: if the PATCH transitions an execution to status=crashed
// AND a crash_signature is provided, the execution is grouped into the
// appropriate failure_group via Store.GroupCrashedExecution. The grouping
// step is best-effort: if it fails, the request still returns 200 because
// the execution's primary update has already succeeded; only the
// dashboard's grouping view is degraded.
//
// (This doc comment was stranded in handlers.go by the Phase C file
// move and reunited with its function during Phase D.)
func (h *Handlers) HandleUpdateExecution(w http.ResponseWriter, r *http.Request) {
	executionID := r.PathValue("id")
	if executionID == "" {
		writeError(w, http.StatusBadRequest, "execution_id path parameter required")
		return
	}

	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context (auth middleware not engaged)")
		return
	}

	var patch events.Execution
	if err := decodeJSON(r, &patch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	patch.ExecutionID = executionID

	//  read the current state to drive the lifecycle
	// state machine. Pause / resume transitions take a dedicated
	// code path (Store.PauseExecution / Store.ResumeExecution),
	// distinct from the normal terminal write through
	// UpdateExecution. We need the prior status to decide which.
	currentExec, err := h.Store.GetExecution(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "execution not found: "+executionID)
			return
		}
		h.Logger.Error("get execution for lifecycle check failed",
			"execution_id", executionID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "lifecycle probe failed: "+err.Error())
		return
	}
	if currentExec.ProjectID != authProjectID {
		// Cross-tenant access returns 404 to avoid leaking that the
		// id exists on a different project, same posture as
		// HandleGetExecution.
		writeError(w, http.StatusNotFound, "execution not found: "+executionID)
		return
	}
	priorStatus := currentExec.Status

	// Validate the transition. Rejecting illegal transitions early
	// keeps the rest of the handler clean.
	if !isValidLifecycleTransition(priorStatus, patch.Status) {
		writeError(w, http.StatusConflict, fmt.Sprintf(
			"invalid lifecycle transition: %s -> %s", priorStatus, patch.Status,
		))
		return
	}

	// Branch on the four meaningful transitions.
	now := time.Now().UTC()
	switch {
	case priorStatus == events.StatusStarted && patch.Status == events.StatusAwaitingHuman:
		// Pure pause. No ended_at, no detector chain (not terminal).
		if err := h.Store.PauseExecution(r.Context(), executionID, authProjectID, now); err != nil {
			h.Logger.Error("pause execution failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
			writeError(w, http.StatusInternalServerError, "pause failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"execution_id": executionID,
			"status":       events.StatusAwaitingHuman,
		})
		return

	case priorStatus == events.StatusAwaitingHuman && patch.Status == events.StatusStarted:
		// Pure resume. No ended_at, no detector chain.
		if err := h.Store.ResumeExecution(r.Context(), executionID, authProjectID, now); err != nil {
			h.Logger.Error("resume execution failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
			writeError(w, http.StatusInternalServerError, "resume failed: "+err.Error())
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"ok":           true,
			"execution_id": executionID,
			"status":       events.StatusStarted,
		})
		return

	case priorStatus == events.StatusAwaitingHuman && patch.Status.IsTerminal():
		// Terminal from paused. Flush accumulated paused time first
		// by resuming (which writes total_paused_ms + clears
		// paused_at), then fall through to the normal terminal
		// update path. The resume here is a synthetic internal
		// transition; from the customer's perspective the
		// execution went paused -> terminal in one PATCH.
		if err := h.Store.ResumeExecution(r.Context(), executionID, authProjectID, now); err != nil {
			h.Logger.Error("flush paused time before terminal failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
			writeError(w, http.StatusInternalServerError, "lifecycle flush failed: "+err.Error())
			return
		}
		// Fall through to UpdateExecution below.
	}

	// Default to ended_at = now for the terminal path so legacy
	// SDKs that don't supply it still get a sensible value.
	// Non-terminal transitions return above before reaching here.
	if patch.EndedAt == nil {
		patch.EndedAt = &now
	}

	//  compute the effective (working-time) duration by
	// subtracting accumulated paused time from the wall-clock
	// duration the SDK supplied. Detectors that gate on
	// "the agent worked for too long" (currently only time_budget)
	// must use effectiveDurationMs so a HITL wait does not falsely
	// trip them.
	//
	// totalPausedMs reflects the FINAL accumulated paused time:
	// every closed pause cycle plus, if we just flushed the
	// terminal-from-paused branch above, the final cycle that the
	// synthetic resume just wrote to the row. We do not re-read the
	// execution from the store here; we reconstruct the same value
	// the resume call would have produced.
	totalPausedMs := currentExec.TotalPausedMs
	if priorStatus == events.StatusAwaitingHuman && patch.Status.IsTerminal() && currentExec.PausedAt != nil {
		flushMs := now.Sub(*currentExec.PausedAt).Milliseconds()
		if flushMs > 0 {
			totalPausedMs += flushMs
		}
	}
	effectiveDurationMs := patch.DurationMs - totalPausedMs
	if effectiveDurationMs < 0 {
		// Defensive: SDK clock skew or pause arithmetic drift.
		// Falling back to the raw wall-clock keeps the detector
		// chain from getting confused by a negative duration.
		effectiveDurationMs = patch.DurationMs
	}

	if err := h.Store.UpdateExecution(r.Context(), &patch); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "execution not found: "+patch.ExecutionID)
			return
		}
		h.Logger.Error("update execution failed",
			"execution_id", patch.ExecutionID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "persist failed: "+err.Error())
		return
	}

	// Phase 7 v0.0.1: model-drift detector, runs FIRST in the detection
	// chain so it wins the idempotency claim over crashes when the crash
	// IS caused by a new model. The classic case: agent calls a model
	// that doesn't exist (deprecated, typo, misrouted), Anthropic
	// returns 404, the agent crashes. Without this ordering, the
	// execution lands in `crashes` with a generic stack-trace signature
	// and the customer never sees the actionable "you used a new model"
	// classification. With drift first, the same execution lands in
	// `drift / new_model:<name>`, which is the right surface for "why
	// did my agent suddenly start failing today."
	//
	// For executions that crashed WITHOUT a model change, drift's
	// condition is false (current models all in historical) → it
	// no-ops, and crashes claims normally below. So this change is
	// safe: it only diverts the model-driven crashes, leaving
	// non-model-related crashes unaffected.
	//
	// Best-effort throughout, drift query failures log and continue
	// rather than blocking the rest of the detection pipeline.

	//, load per-project detector thresholds once per
	// terminal-status pipeline. Bulk-reads every override row for
	// the 6 in-scope detectors and assembles a typed aggregate.
	// Store errors / parse errors silently fall back to defaults
	// per-knob; the aggregate is always populated.
	//
	// telemetry: any fallback path (store error OR
	// per-knob parse error) writes a durable audit_event row via
	// the same `config_fallback` shape used by . target_id
	// is "detector_threshold:<detector>:<key>" so the existing
	// dashboard config-fallback tile aggregates them under
	// DetectorThresholdsCount.
	detectorThresholds := DefaultProjectDetectorThresholds()
	if isTerminalStatus(patch.Status) {
		thresholdTier := TierHobby
		if proj, projErr := h.Store.GetProject(r.Context(), authProjectID); projErr == nil && proj != nil {
			thresholdTier = normalizeTier(proj.Tier)
		}
		auditWriter := func(detector, thresholdKey, reason string, metadata map[string]any) {
			meta := map[string]any{"reason": reason}
			for k, v := range metadata {
				meta[k] = v
			}
			h.recordSystemEventForProject(
				r.Context(),
				authProjectID, "config_fallback",
				"config_fallback", "project_config",
				"detector_threshold:"+detector+":"+thresholdKey,
				meta,
			)
		}
		detectorThresholds = LoadProjectDetectorThresholds(
			r.Context(), h.Store, h.Logger, authProjectID, thresholdTier, auditWriter,
		)
	}

	if isTerminalStatus(patch.Status) {
		// 7-day historical window, same for both drift signals.
		cutoff := time.Now().Add(-7 * 24 * time.Hour)

		// ── Drift v1, model-mix signal ─────────────────────────────
		// Catches: this execution used a model the project hasn't seen.
		currentModels, mErr := h.Store.ListModelsForExecution(r.Context(), executionID)
		if mErr != nil {
			h.Logger.Warn("drift: list models for execution failed (skipping model-drift)",
				"execution_id", executionID,
				"error", mErr.Error(),
			)
		} else if len(currentModels) > 0 {
			historicalModels, hErr := h.Store.ListModelsForProjectSince(r.Context(), authProjectID, cutoff, executionID)
			if hErr != nil {
				h.Logger.Warn("drift: list project models failed (skipping model-drift)",
					"project_id", authProjectID,
					"error", hErr.Error(),
				)
			} else if len(historicalModels) > 0 {
				if signature, drift := detectors.DetectModelDrift(currentModels, historicalModels); drift {
					isNew, dErr := h.Store.GroupDriftSignal(r.Context(), executionID, authProjectID, signature)
					if dErr != nil {
						h.Logger.Warn("drift grouping failed (continuing)",
							"execution_id", executionID,
							"signature", signature,
							"error", dErr.Error(),
						)
					} else {
						h.Logger.Info("model drift detected",
							"execution_id", executionID,
							"signature", signature,
							"current_models", currentModels,
							"historical_models_count", len(historicalModels),
						)
					}
					h.maybeFireWebhook(r, authProjectID, store.FailureClassDrift, signature, isNew, dErr)
				}
			}
		}

		// Drift v2 (lexical) moved to the tail of the detector chain ,
		// see the cost_velocity block below.
	}

	// Phase 3a: link crashed executions to their failure_group. Best-effort ,
	// a grouping failure doesn't fail the PATCH because the execution itself
	// is already correctly recorded. Runs AFTER drift, if drift already
	// claimed this execution, GroupCrashedExecution's idempotency check
	// short-circuits as a no-op.
	//
	// Allowlist.b: check the per-project crashes allowlist first.
	// When the crash_signature (= exception_type, e.g. "ValueError")
	// matches a customer allowlist entry, SKIP both the grouping
	// AND the webhook. Closes crashes.G3.
	if patch.Status == events.StatusCrashed && patch.CrashSignature != "" {
		if h.checkAllowlistAndMaybeSkip(r, authProjectID, "crashes", patch.CrashSignature) {
			// Suppressed by customer allowlist; no failure_group,
			// no webhook. match_count incremented inside the helper.
		} else {
			isNew, err := h.Store.GroupCrashedExecution(r.Context(), executionID, authProjectID, patch.CrashSignature)
			if err != nil {
				h.Logger.Warn("crash grouping failed (continuing)",
					"execution_id", executionID,
					"crash_signature", patch.CrashSignature,
					"error", err.Error(),
				)
			}
			h.maybeFireWebhook(r, authProjectID, store.FailureClassCrashes, patch.CrashSignature, isNew, err)
		}
	}

	// Phase 3b sub-slice 11: step-count detector. Any terminal execution
	// with > StepCountThreshold events gets grouped as loops/step-count.
	// Runs after the crash and time-budget checks, so it's the lowest-
	// priority classification, an execution that crashed, took too long,
	// AND emitted lots of events ends up classified as crashes (first
	// match wins via the failure_group_id short-circuit). Default 10 is
	// artificially low for v0.0.1 demo visibility; iterative-refinement
	// workflows that legitimately emit many events should tune this via
	// the per-project detector_thresholds primitive (loops-thresholds
	// wave, closes loops.G2).
	if isTerminalStatus(patch.Status) {
		count, err := h.Store.CountEventsForExecution(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("count events for step-count check failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if count > detectorThresholds.Loops.StepCountThreshold {
			isNew, gErr := h.Store.GroupStepCountExceedance(r.Context(), executionID, authProjectID, count)
			if gErr != nil {
				h.Logger.Warn("step-count grouping failed (continuing)",
					"execution_id", executionID,
					"event_count", count,
					"error", gErr.Error(),
				)
			}
			h.maybeFireWebhook(r, authProjectID, store.FailureClassLoops, store.StepCountSignature(count), isNew, gErr)
		}
	}

	//, context_overflow and token_waste detectors.
	// Both consume the execution's llm_call events so we query
	// them once and feed both detectors from the result. They run
	// AFTER loops/step_count (which catch coarser patterns) and
	// BEFORE semantic_loop / tool_schema_drift (which catch finer
	// downstream patterns). context_overflow fires on cumulative
	// input_tokens vs configured model window; token_waste fires on
	// repeating user_prompt prefixes.
	if isTerminalStatus(patch.Status) {
		llmPayloads, err := h.Store.ListLLMCallPayloads(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("list llm_call payloads for detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if len(llmPayloads) > 0 {
			rawPayloads := make([]json.RawMessage, len(llmPayloads))
			for i, p := range llmPayloads {
				rawPayloads[i] = json.RawMessage(p)
			}
			// context_overflow first; fail-level overrides
			// token_waste claim if both fired on the same exec.
			if sig, fired := detectors.DetectContextOverflowWithThresholds(rawPayloads, detectorThresholds.ContextOverflow); fired {
				isNew, gErr := h.Store.GroupContextOverflow(r.Context(), executionID, authProjectID, sig)
				if gErr != nil {
					h.Logger.Warn("context-overflow grouping failed (continuing)",
						"execution_id", executionID,
						"signature", sig,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassContextOverflow, sig, isNew, gErr)
			}
			if sig, fired := detectors.DetectTokenWasteWithThresholds(rawPayloads, detectorThresholds.TokenWaste); fired {
				isNew, gErr := h.Store.GroupTokenWaste(r.Context(), executionID, authProjectID, sig)
				if gErr != nil {
					h.Logger.Warn("token-waste grouping failed (continuing)",
						"execution_id", executionID,
						"signature", sig,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassTokenWaste, sig, isNew, gErr)
			}
		}
	}

	//, semantic_loop detector. Hashes the canonical
	// state across all checkpoint events on the execution; if any
	// hash recurs 3+ times the execution is clustered as
	// semantic_loop. Catches the "agent revisits the same logical
	// state via different surface text" pattern that step_count and
	// identical_call_loops cannot see. Runs AFTER the loops family
	// so simpler patterns (exact-call repeat, time-budget breach)
	// claim first; this detector picks up only the residue.
	if isTerminalStatus(patch.Status) {
		checkpoints, err := h.Store.ListCheckpointPayloads(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("list checkpoint payloads for detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if len(checkpoints) > 0 {
			payloads := make([]json.RawMessage, len(checkpoints))
			for i, p := range checkpoints {
				payloads[i] = json.RawMessage(p)
			}
			if sig, fired := detectors.DetectSemanticLoopWithThresholds(payloads, detectorThresholds.SemanticLoop); fired {
				isNew, gErr := h.Store.GroupSemanticLoop(r.Context(), executionID, authProjectID, sig)
				if gErr != nil {
					h.Logger.Warn("semantic-loop grouping failed (continuing)",
						"execution_id", executionID,
						"signature", sig,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassSemanticLoop, sig, isNew, gErr)
			}
		}
	}

	// Tool-schema-drift detection lives in handlers_update_drift.go
	// since the second carve of the split.
	h.runToolSchemaDriftDetector(r, executionID, authProjectID, patch.Status, detectorThresholds.ToolSchemaDrift)

	// Phase 3b sub-slice 13: tool-failures detector. If any tool_call
	// event in the execution had payload.status="failed", classify the
	// execution as tool_failures with signature=tool_name. Different
	// from crashes (where the exception escaped @wrap), tool-failures
	// catches the silent-degradation pattern where the agent recovers
	// from a tool exception and ran to completion but produced
	// degraded output. Runs after the loop detectors so an execution
	// that BOTH had a failed tool AND was a runaway loop classifies
	// as the loop (loops are higher-priority, they waste more).
	if isTerminalStatus(patch.Status) {
		toolName, exceptionType, err := h.Store.FindFirstFailedTool(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("find failed tool for detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if toolName != "" {
			// granular-sig wave: signature is "<tool>:<exception_type>"
			// when the SDK reported exception_type on the failing
			// tool_call event; falls back to bare "<tool>" for legacy
			// tool_call events that pre-date exception_type capture.
			// Allowlist matching still happens on the BARE tool_name
			// (customer allowlists "my_search_tool", not
			// "my_search_tool:ConnectionError") so the existing
			// closure of tool_failures.G4 keeps working unchanged.
			toolSig := toolFailureSignature(toolName, exceptionType)
			// Allowlist.b: customer allowlist by tool_name suppresses
			// detection for known-flaky tools. Closes tool_failures.G4.
			if h.checkAllowlistAndMaybeSkip(r, authProjectID, "tool_failures", toolName) {
				// Suppressed; skip grouping + webhook.
			} else {
				isNew, gErr := h.Store.GroupToolFailure(r.Context(), executionID, authProjectID, toolSig)
				if gErr != nil {
					h.Logger.Warn("tool-failure grouping failed (continuing)",
						"execution_id", executionID,
						"tool_signature", toolSig,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassToolFailures, toolSig, isNew, gErr)
			}
		}
	}

	//, infrastructure_throttled detector. If any
	// infrastructure_event was recorded for this execution, classify
	// it as infrastructure_throttled with a signature derived from
	// (reason, provider, dimension). Distinguishes "your provider is
	// rate-limiting you" / "your circuit breaker tripped" / "you hit
	// your monthly quota" from generic tool_failures, so SREs get a
	// distinct alert chain with a distinct playbook (raise quota vs
	// debug code).
	//
	// Runs after tool_failures so an execution that experienced both
	// a tool failure AND a transport throttling event surfaces under
	// the throttling group (which actionably points at the underlying
	// cause; the tool failure was just the symptom).
	if isTerminalStatus(patch.Status) {
		throttleSig, err := h.Store.FindFirstThrottlingSignal(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("find throttling signal for detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if throttleSig != "" {
			isNew, gErr := h.Store.GroupInfrastructureThrottled(r.Context(), executionID, authProjectID, throttleSig)
			if gErr != nil {
				h.Logger.Warn("infrastructure-throttled grouping failed (continuing)",
					"execution_id", executionID,
					"signature", throttleSig,
					"error", gErr.Error(),
				)
			}
			h.maybeFireWebhook(r, authProjectID, store.FailureClassInfraThrottled, throttleSig, isNew, gErr)
		}
	}

	//, sandbox_escape detector. Scans every tool_call
	// payload for known sandbox-escape patterns (os.system, raw
	// sockets, /proc/self, instance-metadata endpoints, secret
	// file paths). Runs at the security tier alongside
	// data_leakage; both fire same-priority and the dashboard
	// renders both red.
	if isTerminalStatus(patch.Status) {
		toolPayloads, err := h.Store.ListAllToolCallPayloads(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("list tool_call payloads for sandbox-escape failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if len(toolPayloads) > 0 {
			rawPayloads := make([]json.RawMessage, len(toolPayloads))
			for i, p := range toolPayloads {
				rawPayloads[i] = json.RawMessage(p)
			}
			//  load per-project custom sandbox-escape
			// patterns and union with built-ins at scan time.
			customPatterns, _ := h.loadCustomPatternsForDetector(
				r.Context(), authProjectID, "sandbox_escape",
			)
			// all-matches-recorded wave (sandbox_escape.G1): emit one
			// failure_group per matched pattern instead of stopping
			// at first. A defense-in-depth security test exercising
			// 9 built-in patterns now surfaces 9 clusters (was 1).
			// Capped at MaxSandboxEscapeMatchesPerExecution = 20.
			matches := detectors.DetectSandboxEscapeAllMatchesWithCustom(
				rawPayloads, customPatterns,
			)
			for _, m := range matches {
				isNew, gErr := h.Store.GroupSandboxEscape(r.Context(), executionID, authProjectID, m.Signature)
				if gErr != nil {
					h.Logger.Warn("sandbox-escape grouping failed (continuing)",
						"execution_id", executionID,
						"signature", m.Signature,
						"error", gErr.Error(),
					)
					continue
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassSandboxEscape, m.Signature, isNew, gErr)
				h.incrementCustomPatternMatch(r.Context(), authProjectID, m.MatchedPatternID)
			}
		}
	}

	//, data_leakage detector. If any dlp_scan_result
	// event for this execution recorded critical/high hits, cluster
	// the execution under data_leakage with the matched rule_id as
	// the signature. Runs after infrastructure_throttled so an
	// execution that experienced both transport throttling AND a
	// credential leak surfaces under the leak (which is a security
	// incident; throttling is operational); the lower-priority
	// classifier is a no-op once a higher one has claimed the
	// execution.
	if isTerminalStatus(patch.Status) {
		// data_leakage.G5, per-project severity-firing policy.
		// EffectiveAllowedSeverities() handles defensive fallback to
		// the historical default ["critical", "high"] on bad config.
		allowedSeverities := detectorThresholds.DataLeakage.EffectiveAllowedSeverities()
		dlpSig, err := h.Store.FindFirstDLPSignalForSeverities(r.Context(), executionID, allowedSeverities)
		if err != nil {
			h.Logger.Warn("find dlp signal for detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if dlpSig != "" {
			isNew, gErr := h.Store.GroupDataLeakage(r.Context(), executionID, authProjectID, dlpSig)
			if gErr != nil {
				h.Logger.Warn("data-leakage grouping failed (continuing)",
					"execution_id", executionID,
					"rule_id", dlpSig,
					"error", gErr.Error(),
				)
			}
			h.maybeFireWebhook(r, authProjectID, store.FailureClassDataLeakage, dlpSig, isNew, gErr)
		}
	}

	// Phase 3b sub-slice 14: validator-failures detector. If any
	// validator_result event in the execution had payload.passed=false,
	// classify the execution as validator_failures with
	// signature=validator_name. Same silent-degradation family as
	// tool-failures, the agent ran to completion but produced output
	// that a downstream quality check failed.
	if isTerminalStatus(patch.Status) {
		validatorName, severityHint, category, err := h.Store.FindFirstFailedValidator(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("find failed validator for detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if validatorName != "" {
			// granular-sig wave: signature is "<name>:<category>" when
			// the SDK customer supplied an optional category arg to
			// validator_result(); falls back to bare "<name>" when
			// category absent (backward compat, existing customers
			// who don't opt in see zero behavior change). Allowlist
			// matching still happens on the BARE validator_name so
			// the existing closure of validator_failures.G5 keeps
			// working unchanged.
			validatorSig := validatorFailureSignature(validatorName, category)
			// Allowlist.b: customer allowlist by validator_name
			// suppresses detection for known-flaky validators.
			// Closes validator_failures.G5. When suppressed, also
			// skip the severity_hint persistence (no failure_group
			// to attach the hint to) and the webhook fire.
			if h.checkAllowlistAndMaybeSkip(r, authProjectID, "validator_failures", validatorName) {
				// Suppressed; skip grouping + severity_hint + webhook.
			} else {
				isNew, gErr := h.Store.GroupValidatorFailure(r.Context(), executionID, authProjectID, validatorSig)
				if gErr != nil {
					h.Logger.Warn("validator-failure grouping failed (continuing)",
						"execution_id", executionID,
						"validator_signature", validatorSig,
						"error", gErr.Error(),
					)
				}
				// validator_failures.G1: persist the SDK-supplied
				// severity hint on the freshly-created/updated row so
				// the severity resolution chain (webhook_dispatch.go +
				// dashboard read paths) can honor it. Best-effort ,
				// failure here doesn't suppress the failure_group itself.
				if gErr == nil && severityHint != "" {
					groupID := store.DeriveFailureGroupID(
						authProjectID, store.FailureClassValidator, validatorSig,
					)
					if hErr := h.Store.UpdateFailureGroupSeverityHint(
						r.Context(), groupID, severityHint,
					); hErr != nil && !errors.Is(hErr, store.ErrNotFound) {
						h.Logger.Warn("update failure_group severity_hint failed",
							"group_id", groupID,
							"severity_hint", severityHint,
							"error", hErr.Error(),
						)
					}
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassValidator, validatorSig, isNew, gErr)
			}
		}
	}

	//, grounding_failure detector. Aggregates the
	// eval_score events ingested via emit_eval_score. Fires
	// when any external evaluator returned passed=false, or when
	// mean score across higher_is_better evaluators fell below 0.5.
	// Runs after validator_failures because:
	//   - validator_result represents the agent's OWN self-check;
	//     eval_score represents an EXTERNAL evaluator's verdict.
	//     Both fire on quality issues but at different rigor levels.
	if isTerminalStatus(patch.Status) {
		evalPayloads, err := h.Store.ListEvalScorePayloads(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("list eval_score payloads for grounding detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if len(evalPayloads) > 0 {
			rawPayloads := make([]json.RawMessage, len(evalPayloads))
			for i, p := range evalPayloads {
				rawPayloads[i] = json.RawMessage(p)
			}
			// all-matches-recorded wave (grounding_failure.G2): emit
			// one failure_group per failing (evaluator, metric) pair
			// instead of stopping at first. A RAG pipeline failing
			// faithfulness + answer_relevance + factuality now
			// surfaces 3 clusters (was 1). Capped at
			// MaxGroundingFailureMatchesPerExecution = 20.
			sigs := detectors.DetectGroundingFailureAllMatchesWithThresholds(rawPayloads, detectorThresholds.GroundingFailure)
			for _, sig := range sigs {
				isNew, gErr := h.Store.GroupGroundingFailure(r.Context(), executionID, authProjectID, sig)
				if gErr != nil {
					h.Logger.Warn("grounding-failure grouping failed (continuing)",
						"execution_id", executionID,
						"signature", sig,
						"error", gErr.Error(),
					)
					continue
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassGroundingFailure, sig, isNew, gErr)
			}
		}
	}

	//, cascading_failure detector. Joins this
	// execution's agent_handoff events with the terminal
	// status of each referenced child execution and fires when a
	// handoff was followed by the child reaching a failure
	// terminal state. Runs after grounding_failure because:
	//   - grounding/eval signals are per-evaluator and orthogonal;
	//   - cascading_failure is a CROSS-execution signal that
	//     subsumes the child's own per-execution failure_group
	//     into a "this run is part of a chain" cluster, which is
	//     a more useful framing for the customer than the raw
	//     two-separate-bugs view.
	if isTerminalStatus(patch.Status) {
		handoffs, err := h.Store.ListHandoffsWithChildStatus(r.Context(), executionID, authProjectID)
		if err != nil {
			h.Logger.Warn("list handoffs with child status for cascade detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if len(handoffs) > 0 {
			// extensions wave, cascading_failure.G2 + G3.
			// Per-project cascade-window + spawn-handoff exclusion
			// flow in via detectorThresholds.CascadingFailure.
			if sig, fired := detectors.DetectCascadingFailureWithThresholds(handoffs, detectorThresholds.CascadingFailure); fired {
				isNew, gErr := h.Store.GroupCascadingFailure(r.Context(), executionID, authProjectID, sig)
				if gErr != nil {
					h.Logger.Warn("cascading-failure grouping failed (continuing)",
						"execution_id", executionID,
						"signature", sig,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassCascadingFailure, sig, isNew, gErr)
			}
		}
	}

	//, coordination_deadlock detector. Walks the
	// topology subtree rooted at this execution, collects every
	// agent_handoff edge, and fires on the first 2-cycle in the
	// agent-role graph (A→B AND B→A in the same subtree). Runs
	// after cascading_failure so that a deadlock that ALSO
	// produced a cascade gets attributed to the more specific
	// "deadlock" class (timeout-without-progress is a more
	// actionable framing than "child crashed").
	if isTerminalStatus(patch.Status) {
		edges, err := h.Store.ListHandoffEdgesInTopology(r.Context(), executionID, authProjectID, 0)
		if err != nil {
			h.Logger.Warn("list handoff edges for deadlock detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if len(edges) >= 2 {
			if sig, fired := detectors.DetectCoordinationDeadlock(edges); fired {
				isNew, gErr := h.Store.GroupCoordinationDeadlock(r.Context(), executionID, authProjectID, sig)
				if gErr != nil {
					h.Logger.Warn("coordination-deadlock grouping failed (continuing)",
						"execution_id", executionID,
						"signature", sig,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassCoordinationDeadlock, sig, isNew, gErr)
			}
		}
	}

	//, provider_incident detector. Scans this
	// execution's llm_call payloads for provider errors, then
	// asks the store how many DISTINCT tenants in the same
	// project saw the same (provider, error_class) error in the
	// recent window. Fires when the cross-tenant count meets
	// MinTenantsForProviderIncident.
	//
	// Order rationale: runs last in the failure-detection chain
	// because it is a cross-cutting signal (provider-side, not
	// agent-side) and should not preempt the agent-level
	// classes above. A provider_incident group does not preclude
	// other groupings on the same execution; the dashboard
	// surfaces all of them.
	if isTerminalStatus(patch.Status) {
		llmPayloads, err := h.Store.ListLLMCallPayloads(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("list llm_call payloads for provider-incident detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if len(llmPayloads) > 0 {
			// Extract distinct (provider, error_class) pairs
			// emitted by THIS execution. We only check the cross-
			// tenant count for pairs we already saw locally; that
			// keeps the query budget linear in this execution's
			// own provider-error footprint, not in the project's
			// total provider diversity.
			seen := map[[2]string]struct{}{}
			for _, raw := range llmPayloads {
				var p struct {
					Provider   string `json:"provider"`
					ErrorClass string `json:"error_class"`
				}
				if jErr := json.Unmarshal(raw, &p); jErr != nil {
					continue
				}
				if p.Provider == "" || p.ErrorClass == "" {
					continue
				}
				// Drop customer-side error classes
				// (invalid_api_key, client_error, unknown) from
				// provider_incident aggregation. They get stored
				// on the llm_call event for observability but a
				// rate of bad-key errors across tenants is not a
				// provider outage, it's customer key rotation
				// failures. The PROVIDER_SIDE_ERROR_CLASSES set
				// matches what the SDK ships in mesedi/errors.py
				// and src/errors.ts.
				if !detectors.IsProviderSideErrorClass(p.ErrorClass) {
					continue
				}
				seen[[2]string{p.Provider, p.ErrorClass}] = struct{}{}
			}
			// Per-project threshold (/migration 040).
			// : cascading resolver walks project → org default
			// → hardcoded constant (which is the same value as
			// detectors.MinTenantsForProviderIncident). Records a
			// config_fallback system_event internally on errors.
			threshold, _ := h.ResolveProviderIncidentMinTenants(
				r.Context(), authProjectID,
			)
			// Look back 15 minutes, long enough to span a
			// rolling provider blip, short enough to keep the
			// signal current. The constant is intentionally not
			// configurable at v1; a future iteration can promote
			// it to a per-project setting.
			since := time.Now().Add(-15 * time.Minute)
			for pair := range seen {
				provider, errClass := pair[0], pair[1]
				count, cErr := h.Store.CountDistinctTenantsWithProviderError(
					r.Context(), authProjectID, provider, errClass, since,
				)
				if cErr != nil {
					h.Logger.Warn("count distinct tenants with provider error failed",
						"execution_id", executionID,
						"provider", provider,
						"error_class", errClass,
						"error", cErr.Error(),
					)
					continue
				}
				if sig, fired := detectors.DetectProviderIncident(
					provider, errClass, count, threshold,
				); fired {
					isNew, gErr := h.Store.GroupProviderIncident(r.Context(), executionID, authProjectID, sig)
					if gErr != nil {
						h.Logger.Warn("provider-incident grouping failed (continuing)",
							"execution_id", executionID,
							"signature", sig,
							"error", gErr.Error(),
						)
					}
					h.maybeFireWebhook(r, authProjectID, store.FailureClassProviderIncident, sig, isNew, gErr)
				}
			}
		}
	}

	//, hitl_timeout detector. Aggregates the
	// human_intervention events on this execution and fires
	// when the customer's HITL SLA was breached. Two firing
	// conditions: response_kind=="timeout" (explicit) OR
	// wait_duration_ms > sla_seconds*1000 (sla_exceeded). Runs
	// after provider_incident because HITL signals are scoped to
	// THIS run; provider_incident is the cross-tenant signal
	// that subsumes per-run causes when present.
	if isTerminalStatus(patch.Status) {
		hiPayloads, err := h.Store.ListHumanInterventionPayloads(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("list human_intervention payloads for hitl_timeout detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if len(hiPayloads) > 0 {
			raw := make([]json.RawMessage, len(hiPayloads))
			for i, p := range hiPayloads {
				raw[i] = json.RawMessage(p)
			}
			// all-matches-recorded wave (hitl_timeout.G3): emit both
			// explicit AND sla_exceeded clusters when both firing
			// conditions are present in the same execution. Legacy
			// first-match-wins suppressed the SLA-exceeded view
			// when an explicit timeout also fired.
			// hitl_timeout.G4, per-project fire-mode toggle.
			// Defaults `["explicit", "sla_exceeded"]` match historical
			// posture; customers can mute either mode via the
			// detector_thresholds primitive.
			sigs := detectors.DetectHITLTimeoutAllMatchesWithThresholds(raw, detectorThresholds.HITLTimeout)
			for _, sig := range sigs {
				isNew, gErr := h.Store.GroupHITLTimeout(r.Context(), executionID, authProjectID, sig)
				if gErr != nil {
					h.Logger.Warn("hitl-timeout grouping failed (continuing)",
						"execution_id", executionID,
						"signature", sig,
						"error", gErr.Error(),
					)
					continue
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassHITLTimeout, sig, isNew, gErr)
			}

			//, hitl_rejection_spike detector. Cross-
			// execution signal: if a high fraction of recent HITL
			// runs in this project came back rejected or edited,
			// the agent's behavior likely regressed. We only run
			// this when THIS execution itself recorded at least
			// one human_intervention event (no point asking
			// "did rejections spike" if we have no fresh HITL
			// data anyway). Default 60-minute window matches the
			// provider_incident posture; per-project tunable via
			// the detector_thresholds primitive (
			// extensions wave, closes hitl_rejection_spike.G3).
			windowMinutes := detectorThresholds.HITLRejectionSpike.EffectiveWindowMinutes()
			since := time.Now().Add(-time.Duration(windowMinutes) * time.Minute)
			counts, sErr := h.Store.CountHITLOutcomesInWindow(r.Context(), authProjectID, since)
			if sErr != nil {
				h.Logger.Warn("count hitl outcomes in window failed",
					"execution_id", executionID,
					"error", sErr.Error(),
				)
			} else if sig, fired := detectors.DetectHITLRejectionSpike(
				counts,
				detectors.MinHITLSampleForRejectionSpike,
				detectors.RejectionSpikeRateBp,
				detectors.EditSpikeRateBp,
			); fired {
				isNew, gErr := h.Store.GroupHITLRejectionSpike(r.Context(), executionID, authProjectID, sig)
				if gErr != nil {
					h.Logger.Warn("hitl-rejection-spike grouping failed (continuing)",
						"execution_id", executionID,
						"signature", sig,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassHITLRejectionSpike, sig, isNew, gErr)
			}
		}
	}

	// Phase 3b sub-slices 12 + 15: events-driven post-processing. Both
	// cost computation and prompt-injection detection walk the same
	// event list, so fetch ONCE and feed both. Best-effort throughout ,
	// failures here never fail the PATCH.
	if isTerminalStatus(patch.Status) {
		evts, err := h.Store.ListEventsForExecution(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("list events for post-PATCH processing failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else {
			// record_integrity detector. Runs FIRST inside this block,
			// before cost computation and every other event-driven
			// pass, and the ordering is the point: everything below
			// derives a number from this event list. If the list has a
			// hole in it, the operator should be told that before they
			// are handed totals computed from it.
			//
			// Reads only Event.Sequence. No clock, no payload, no
			// provider lookup, so it cannot be skewed by a customer
			// running agents across several machines, and it works on
			// telemetry already being collected by every SDK version.
			//
			// Best-effort like its neighbours: a grouping failure is
			// logged and the PATCH still succeeds. An integrity check
			// that could fail a customer's execution close would be a
			// worse availability problem than the one it reports.
			if len(evts) > 0 {
				seqs := customerSequences(evts)
				for _, sig := range detectors.DetectRecordIntegrityAllMatches(seqs) {
					isNew, gErr := h.Store.GroupRecordIntegrity(r.Context(), executionID, authProjectID, sig)
					if gErr != nil {
						h.Logger.Warn("record-integrity grouping failed (continuing)",
							"execution_id", executionID,
							"signature", sig,
							"error", gErr.Error(),
						)
						continue
					}
					h.Logger.Info("record integrity signal",
						"execution_id", executionID,
						"signature", sig,
						"event_count", len(seqs),
						"missing", detectors.MissingSequences(seqs),
						"duplicated", detectors.DuplicatedSequences(seqs),
					)
					h.maybeFireWebhook(r, authProjectID, store.FailureClassRecordIntegrity, sig, isNew, gErr)
				}
			}

			// Sub-slice 12: cost computation., backend is now
			// the source of truth for known models. The SDK-shipped
			// per-event estimated_cost_usd is fallback only for models
			// missing from pricing.priceTable. This means new model
			// pricing or pricing changes ship with a backend deploy
			// without waiting for an SDK release.
			//
			// Always walks events and recomputes; the patch body's
			// EstimatedCostUSD is kept as a defensive last-resort
			// fallback when the walk produced no value (e.g. tool-only
			// executions with no llm_call events). When the walk
			// produces a value, it overrides whatever the SDK rolled
			// up, that's the inversion.
			//
			// Unknown models surface via an audit_event (one per
			// execution, not per event) so the dashboard's existing
			// config-fallback chip can show "Mesedi doesn't know how
			// to price model X, using SDK fallback" tile.
			//, pass the per-project pricing overrides
			// loaded earlier in this handler (line ~752) so any
			// (detector="pricing", threshold_key="custom_model_pricing")
			// row beats the canonical priceTable for exact-name matches.
			cost, unknownModels := computeExecutionCost(
				evts, detectorThresholds.Pricing.CustomModelPricing,
			)
			effectiveCost := cost
			if effectiveCost == 0 {
				effectiveCost = patch.EstimatedCostUSD
			}
			if effectiveCost > 0 {
				if err := h.Store.SetExecutionCost(r.Context(), executionID, effectiveCost); err != nil {
					h.Logger.Warn("set execution cost failed",
						"execution_id", executionID,
						"computed_cost_usd", effectiveCost,
						"backend_cost_usd", cost,
						"sdk_rollup_cost_usd", patch.EstimatedCostUSD,
						"error", err.Error(),
					)
				} else {
					h.Logger.Info("execution cost computed",
						"execution_id", executionID,
						"cost_usd", effectiveCost,
						"backend_cost_usd", cost,
						"unknown_model_count", len(unknownModels),
					)
				}
			}
			if len(unknownModels) > 0 {
				h.recordSystemEventForProject(
					r.Context(),
					authProjectID, "pricing",
					"pricing_unknown_model", "execution", executionID,
					map[string]any{
						"models":                unknownModels,
						"pricing_table_version": pricing.PricingTableVersion,
					},
				)
			}

			// Sub-slice 17: identical-call loop detector. Hashes
			// (model + user_message) per llm_call; if the same hash
			// appears IdenticalCallMinRepeats+ times in one execution,
			// group as loops/identical_call. Runs BEFORE the injection
			// check because a runaway loop generating the same prompt
			// repeatedly is a more urgent resource-waste signal than
			// a single injection attempt embedded in the same prompt.
			// loops-thresholds wave: per-project tunable (closes
			// loops.G3); default 3 matches the historical hardcoded
			// behavior.
			if callHash, found := scanForIdenticalCalls(evts, detectorThresholds.Loops.IdenticalCallMinRepeats); found {
				isNew, gErr := h.Store.GroupIdenticalCallLoop(r.Context(), executionID, authProjectID, callHash)
				if gErr != nil {
					h.Logger.Warn("identical-call grouping failed (continuing)",
						"execution_id", executionID,
						"call_hash", callHash,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassLoops, "identical_call_"+callHash, isNew, gErr)
			}

			// Sub-slice 81: similar-call loop detector. Catches the
			// "stuck-loop with paraphrased prompts" pattern that
			// identical_call misses, different exact text, same
			// semantic intent, ≥ SimilarCallMinClusterSize near-
			// duplicates within one execution. Runs AFTER identical_
			// call so exact-text loops win the more-specific signature;
			// only loops with varied wording reach this code path.
			// Uses the same trigram substrate as drift v2. loops-
			// thresholds wave: distance + cluster size both per-project
			// tunable via DetectSimilarCallLoopWithThresholds (closes
			// loops.G4). Defaults match the historical hardcoded behavior.
			similarMsgs := extractLLMUserMessages(evts)
			if len(similarMsgs) >= detectorThresholds.Loops.SimilarCallMinClusterSize {
				if callHash, found := detectors.DetectSimilarCallLoopWithThresholds(similarMsgs, detectorThresholds.Loops); found {
					isNew, gErr := h.Store.GroupSimilarCallLoop(r.Context(), executionID, authProjectID, callHash)
					if gErr != nil {
						h.Logger.Warn("similar-call grouping failed (continuing)",
							"execution_id", executionID,
							"call_hash", callHash,
							"error", gErr.Error(),
						)
					}
					h.maybeFireWebhook(r, authProjectID, store.FailureClassLoops, "similar_call_"+callHash, isNew, gErr)
				}
			}

			// Sub-slice 15: prompt-injection detection. Scan each
			// llm_call event's user_message + system_prompt for known
			// injection patterns. First match wins; the pattern name
			// becomes the failure_group signature so all executions
			// hitting the same attack pattern cluster together.
			//
			// PRIORITY NOTE: injection runs BEFORE cost-velocity (just
			// below) because a prompt-injection is a security event ,
			// "this execution was attacked" is a more important
			// classification than "this execution was expensive."
			// The failure_group_id idempotency short-circuit means an
			// injection-classified execution skips cost-velocity even
			// if it would otherwise have matched.
			//  load per-project custom prompt-injection
			// patterns and union with built-ins at scan time.
			customPatterns, _ := h.loadCustomPatternsForDetector(
				r.Context(), authProjectID, "prompt_injection",
			)
			if pattern, matchedPatternID, found := scanForInjection(
				evts, customPatterns,
			); found {
				isNew, gErr := h.Store.GroupPromptInjection(r.Context(), executionID, authProjectID, pattern)
				if gErr != nil {
					h.Logger.Warn("prompt-injection grouping failed (continuing)",
						"execution_id", executionID,
						"pattern", pattern,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassInjection, pattern, isNew, gErr)
				h.incrementCustomPatternMatch(r.Context(), authProjectID, matchedPatternID)
			}

			// Cost-velocity detection, both forms, lives in
			// handlers_update_cost.go since the #35 Phase D carve.
			h.runCostVelocityDetectors(r, executionID, authProjectID, effectiveCost, currentExec.TenantID, currentExec.APIKeyID)

			// Time-budget detector. Catch-all for executions that ran
			// long without a more specific cause. MOVED HERE from the
			// top of the chain (where it ran at line 737 with a 1s
			// threshold) because the original placement greedy-claimed
			// every execution > 1s and the failure_group_id idempotency
			// short-circuit then silently suppressed ~7 more specific
			// detectors (token_waste, semantic_loop, tool_schema_drift,
			// data_leakage, cascading_failure, prompt_injection,
			// cost_velocity). Real customer agents that take >1s of
			// wall-clock are normal, they should be classified by the
			// specific failure they experienced, not by a generic
			// "slow" bucket. Time-budget is now the second-to-last
			// detector in the chain, falling through only when no
			// specific detector claimed the execution.
			//
			// Threshold raised from 1s (v0.0.1 demo placeholder) to
			// 60s, the production default the original placeholder
			// comment intended. Real "agent stuck running" alerts
			// belong in the minute+ range, not the second range.
			//
			//  subtract accumulated paused time so a HITL
			// wait does not falsely trip the time budget.
			// effectiveDurationMs reflects the agent's actual working
			// time (wall-clock minus all paused intervals).
			//
			//  threshold is now per-project (migration
			// 041). Default 60_000 ms matches the historical
			// hardcoded constant; a chat-agent project can lower it
			// to 30_000, a research-agent project can raise it to
			// 300_000. : cascading resolver walks
			// project → org default → hardcoded constant, emitting
			// a config_fallback system_event on store-layer
			// errors so silent degradation surfaces in the dashboard.
			thresholdMs, _ := h.ResolveTimeBudgetMs(r.Context(), authProjectID)
			if isTerminalStatus(patch.Status) && effectiveDurationMs >= int64(thresholdMs) {
				isNew, err := h.Store.GroupTimeBudgetExceedance(r.Context(), executionID, authProjectID, effectiveDurationMs)
				if err != nil {
					h.Logger.Warn("time-budget grouping failed (continuing)",
						"execution_id", executionID,
						"effective_duration_ms", effectiveDurationMs,
						"wall_clock_duration_ms", patch.DurationMs,
						"total_paused_ms", totalPausedMs,
						"error", err.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassLoops, store.TimeBudgetSignature(effectiveDurationMs), isNew, err)
			}

			// Drift v2, lexical signal. Char-3-gram cosine distance
			// between current execution's user_messages and the
			// project's recent history. Runs LAST in the chain on
			// purpose: lexical drift is a SOFT behavioral signal that
			// should only surface for executions nothing else
			// classified. The idempotency short-circuit in
			// GroupDriftSignal means any execution already grouped
			// (crashes, loops, tool_failures, validator_failures,
			// prompt_injection, cost_velocity, or model-drift) skips
			// drift v2, which is the right priority: specific causal
			// classifications beat the "prompts have shifted" pattern.
			//
			// The signal still gets logged when computed but
			// not-grouped, so dashboards / detectors can be tuned
			// against real data later without changing the order.
			driftCutoff := time.Now().Add(-7 * 24 * time.Hour)
			currentMsgs, cErr := h.Store.ListLLMUserMessagesForExecution(r.Context(), executionID)
			if cErr != nil {
				h.Logger.Warn("drift: list user_messages for execution failed (skipping lexical-drift)",
					"execution_id", executionID,
					"error", cErr.Error(),
				)
			} else if len(currentMsgs) > 0 {
				historicalMsgs, hErr := h.Store.ListLLMUserMessagesForProjectSince(r.Context(), authProjectID, driftCutoff, executionID, 500)
				if hErr != nil {
					h.Logger.Warn("drift: list project user_messages failed (skipping lexical-drift)",
						"project_id", authProjectID,
						"error", hErr.Error(),
					)
				} else if len(historicalMsgs) > 0 {
					if signature, distance, drift := detectors.DetectLexicalDriftWithThresholds(currentMsgs, historicalMsgs, detectorThresholds.Drift); drift {
						isNew, dErr := h.Store.GroupDriftSignal(r.Context(), executionID, authProjectID, signature)
						if dErr != nil {
							h.Logger.Warn("lexical drift grouping failed (continuing)",
								"execution_id", executionID,
								"signature", signature,
								"distance", distance,
								"error", dErr.Error(),
							)
						} else {
							h.Logger.Info("lexical drift detected",
								"execution_id", executionID,
								"signature", signature,
								"distance", distance,
								"current_msgs_count", len(currentMsgs),
								"historical_msgs_count", len(historicalMsgs),
							)
						}
						h.maybeFireWebhook(r, authProjectID, store.FailureClassDrift, signature, isNew, dErr)
					}
				}
			}
		}
	}

	h.Logger.Info("execution updated",
		"execution_id", patch.ExecutionID,
		"status", patch.Status,
		"ended_at", patch.EndedAt.Format(time.RFC3339),
		"duration_ms", patch.DurationMs,
		"total_tokens_in", patch.TotalTokensIn,
		"total_tokens_out", patch.TotalTokensOut,
		"estimated_cost_usd", patch.EstimatedCostUSD,
		"crash_signature", patch.CrashSignature,
	)

	//, OpenTelemetry parallel emission. After the
	// terminal-status write + detector chain have both committed,
	// fire-and-forget a goroutine that translates this execution
	// (plus its events) into an OTel trace and ships it via OTLP
	// to the customer-configured collector. Best-effort: emission
	// failures are logged inside the emitter and never surface to
	// the customer. Reads the full event list once, separate from
	// the earlier post-PATCH loop that did per-event cost +
	// injection scanning, because the OTel write should reflect
	// the FINAL execution state (with any post-detector writes
	// already persisted, including failure_group_id set by
	// detectors above).
	if h.OTel.Enabled() && isTerminalStatus(patch.Status) {
		go func() {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			finalExec, gErr := h.Store.GetExecution(ctx, executionID)
			if gErr != nil {
				h.Logger.Warn("otel: load execution for emission failed",
					"execution_id", executionID,
					"error", gErr.Error(),
				)
				return
			}
			evts, eErr := h.Store.ListEventsForExecution(ctx, executionID)
			if eErr != nil {
				h.Logger.Warn("otel: load events for emission failed",
					"execution_id", executionID,
					"error", eErr.Error(),
				)
				return
			}
			h.OTel.Emit(ctx, finalExec, evts)
		}()
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"execution_id": patch.ExecutionID,
		"status":       patch.Status,
	})
}
