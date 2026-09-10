// Events-driven post-processing, carved out of
// HandleUpdateExecution on 2026-09-10 (the split's third carve).
// Fetches the execution's events once and feeds cost computation,
// prompt-injection scanning, record-integrity checking and the
// cost-velocity detectors from that single list. Body moved by exact
// line range with ZERO substitutions: every parameter carries the
// same name the enclosing function used.
package api

import (
	"net/http"
	"time"

	"mesedi/backend/internal/detectors"
	"mesedi/backend/internal/events"
	"mesedi/backend/internal/pricing"
	"mesedi/backend/internal/store"
)

// runEventsPostProcessing walks the execution's events once and runs
// every detector that consumes them, then persists the resolved cost.
func (h *Handlers) runEventsPostProcessing(r *http.Request, executionID, authProjectID string, patch *events.Execution, currentExec *events.Execution, detectorThresholds ProjectDetectorThresholds, effectiveDurationMs, totalPausedMs int64, now time.Time) {
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
			// handlers_update_cost.go since the split's first carve.
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

}
