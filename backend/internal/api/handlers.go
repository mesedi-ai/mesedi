// Package api wires HTTP handlers for the Mesedi backend service.
//
// Phase 1 scope: ingest endpoints accept JSON, validate shape, log to
// stdout. Storage (Postgres) and detection (loops, drift, etc.) come
// online in Phase 1.5 and Phases 3+.
//
// Handlers do not authenticate yet — Phase 1.5 adds bearer-token auth
// via middleware. For local dev today, any caller can post to /events
// and /executions; that is intentional and matches the "ship phase
// acceptance, iterate after" principle of the development checklist.
package api

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"mesedi/backend/internal/detectors"
	"mesedi/backend/internal/events"
	"mesedi/backend/internal/playbooks"
	"mesedi/backend/internal/pricing"
	"mesedi/backend/internal/store"
	"mesedi/backend/internal/webhooks"
)

// Handlers carries dependencies needed by HTTP handlers. As more
// subsystems come online (storage, detectors, etc.) they get attached
// here rather than passed through each handler signature.
type Handlers struct {
	Logger        *slog.Logger
	Store         store.Store
	HaltSubs      *HaltSubscribers // sub-slice 21b — SSE halt-channel registry
	WebhookClient *http.Client     // task #83 — outbound dispatcher HTTP client
}

// New constructs the Handlers value. Done as a constructor (rather than
// a literal) so the dependencies become explicit as the surface grows.
func New(logger *slog.Logger, s store.Store) *Handlers {
	return &Handlers{
		Logger:        logger,
		Store:         s,
		HaltSubs:      NewHaltSubscribers(),
		WebhookClient: webhooks.DefaultHTTPClient(),
	}
}

// RegisterRoutes attaches every protected route to the provided ServeMux.
// Keep this list short and explicit — it doubles as the API surface
// inventory for documentation.
// RegisterPublicRoutes attaches handlers that intentionally bypass
// bearer-token auth. /signup is public because a browser visiting it
// has no API key yet; abuse is bounded by signup.go's in-process IP
// rate limiter.
func (h *Handlers) RegisterPublicRoutes(mux *http.ServeMux) {
	mux.HandleFunc("POST /signup", h.HandleSignup)
}

func (h *Handlers) RegisterRoutes(mux *http.ServeMux) {
	// Phase 1 ingest surface.
	mux.HandleFunc("POST /executions", h.HandleCreateExecution)
	// #118 Slice 1 — read-side project surface for the dashboard.
	mux.HandleFunc("GET /project", h.HandleGetProject)
	mux.HandleFunc("PATCH /executions/{id}", h.HandleUpdateExecution)
	mux.HandleFunc("POST /events", h.HandleIngestEvents)
	// Phase 3b — read-side execution surface for the dashboard.
	mux.HandleFunc("GET /executions", h.HandleListExecutions)
	mux.HandleFunc("GET /executions/{id}", h.HandleGetExecution)
	mux.HandleFunc("GET /stats", h.HandleStats)
	// Phase 3a — read-side failure_group surface for the dashboard.
	mux.HandleFunc("GET /failure-groups", h.HandleListFailureGroups)
	mux.HandleFunc("GET /failure-groups/{id}", h.HandleGetFailureGroup)
	// Phase 3b sub-slice 9 — executions inside a failure_group.
	mux.HandleFunc("GET /failure-groups/{id}/executions", h.HandleListExecutionsInFailureGroup)
	// Phase 3b sub-slice 18 — API key management surface.
	mux.HandleFunc("GET /api-keys", h.HandleListAPIKeys)
	mux.HandleFunc("POST /api-keys", h.HandleCreateAPIKey)
	mux.HandleFunc("DELETE /api-keys/{id}", h.HandleRevokeAPIKey)
	// Sub-slice 21b — SSE remote-halt channel.
	mux.HandleFunc("GET /executions/{id}/halt-stream", h.HandleHaltStream)
	mux.HandleFunc("POST /executions/{id}/halt", h.HandleTriggerHalt)
	// Tier 1 Playbooks — canonical fix descriptions per failure-class
	// signature. Plain GET with query params; content is the embedded
	// markdown shipped in internal/playbooks/content/.
	mux.HandleFunc("GET /playbooks", h.HandleGetPlaybook)
	// Task #83 — webhook escalation config + dispatcher.
	mux.HandleFunc("GET /webhooks", h.HandleListWebhooks)
	mux.HandleFunc("POST /webhooks", h.HandleCreateWebhook)
	mux.HandleFunc("DELETE /webhooks/{id}", h.HandleDeleteWebhook)
	// Slice 2: manual test-delivery trigger + deliveries log read.
	mux.HandleFunc("POST /webhooks/{id}/test", h.HandleTestWebhook)
	mux.HandleFunc("GET /webhooks/{id}/deliveries", h.HandleListWebhookDeliveries)
}

// HandleGetPlaybook returns the markdown content for the playbook
// matching (failure_class, signature) query parameters. Returns:
//
//	200 + text/markdown  — content
//	400                  — missing or empty query params
//	404                  — no playbook matches this (class, signature)
//
// No auth-context project check needed — playbook content is
// universal (doesn't reference any particular project's data), so
// authenticated callers in any project can read any playbook.
func (h *Handlers) HandleGetPlaybook(w http.ResponseWriter, r *http.Request) {
	failureClass := r.URL.Query().Get("failure_class")
	signature := r.URL.Query().Get("signature")
	if failureClass == "" || signature == "" {
		writeError(w, http.StatusBadRequest,
			"failure_class and signature query parameters are both required")
		return
	}

	body, err := playbooks.Load(failureClass, signature)
	if err != nil {
		if errors.Is(err, playbooks.ErrNotFound) {
			writeError(w, http.StatusNotFound,
				"no playbook for failure_class="+failureClass+" signature="+signature)
			return
		}
		h.Logger.Error("playbook load failed",
			"failure_class", failureClass,
			"signature", signature,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "playbook load failed: "+err.Error())
		return
	}

	w.Header().Set("Content-Type", "text/markdown; charset=utf-8")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(body))
}

// HandleCreateExecution accepts an Execution at the agent's entry point
// and records the start of a run. Phase 1 implementation just validates
// shape and logs; Phase 1.5 persists to Postgres.
func (h *Handlers) HandleCreateExecution(w http.ResponseWriter, r *http.Request) {
	var exec events.Execution
	if err := decodeJSON(r, &exec); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if exec.ExecutionID == "" {
		writeError(w, http.StatusBadRequest, "execution_id is required")
		return
	}

	// Auth-attached project_id is the source of truth. If the request
	// body provided one and it doesn't match, reject — this catches
	// SDK bugs where the wrong API key was used for the wrong project.
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context (auth middleware not engaged)")
		return
	}
	if exec.ProjectID != "" && exec.ProjectID != authProjectID {
		writeError(w, http.StatusForbidden,
			"project_id in body does not match authenticated project")
		return
	}
	exec.ProjectID = authProjectID

	if exec.Status == "" {
		exec.Status = events.StatusStarted
	}
	if exec.StartedAt.IsZero() {
		exec.StartedAt = time.Now().UTC()
	}

	if err := h.Store.CreateExecution(r.Context(), &exec); err != nil {
		h.Logger.Error("create execution failed",
			"execution_id", exec.ExecutionID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "persist failed: "+err.Error())
		return
	}

	h.Logger.Info("execution created",
		"execution_id", exec.ExecutionID,
		"project_id", exec.ProjectID,
		"status", exec.Status,
		"started_at", exec.StartedAt.Format(time.RFC3339),
		"sdk_language", exec.SDKLanguage,
	)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"execution_id": exec.ExecutionID,
		"status":       exec.Status,
	})
}

// HandleUpdateExecution marks an existing execution as completed, crashed,
// halted, etc. Idempotent — repeated PATCH calls with the same status are
// silently accepted.
//
// Phase 3a addition: if the PATCH transitions an execution to status=crashed
// AND a crash_signature is provided, the execution is grouped into the
// appropriate failure_group via Store.GroupCrashedExecution. The grouping
// step is best-effort: if it fails, the request still returns 200 because
// the execution's primary update has already succeeded; only the
// dashboard's grouping view is degraded.
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

	if patch.EndedAt == nil {
		now := time.Now().UTC()
		patch.EndedAt = &now
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

	// Phase 7 v0.0.1: model-drift detector — runs FIRST in the detection
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
	// Best-effort throughout — drift query failures log and continue
	// rather than blocking the rest of the detection pipeline.
	if isTerminalStatus(patch.Status) {
		// 7-day historical window — same for both drift signals.
		cutoff := time.Now().Add(-7 * 24 * time.Hour)

		// ── Drift v1 — model-mix signal ─────────────────────────────
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

		// Drift v2 (lexical) moved to the tail of the detector chain —
		// see the cost_velocity block below.
	}

	// Phase 3a: link crashed executions to their failure_group. Best-effort —
	// a grouping failure doesn't fail the PATCH because the execution itself
	// is already correctly recorded. Runs AFTER drift — if drift already
	// claimed this execution, GroupCrashedExecution's idempotency check
	// short-circuits as a no-op.
	if patch.Status == events.StatusCrashed && patch.CrashSignature != "" {
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

	// Phase 3b sub-slice 10: time-budget detector. Any terminal execution
	// whose duration exceeded the threshold gets grouped as loops/time-budget.
	// Runs AFTER the crash check so a crashed-and-slow execution is
	// classified as crashes (the idempotency check in the store short-
	// circuits this call). The threshold is hardcoded at 1s for v0.0.1;
	// production default will be 60s, configurable per-project later.
	if isTerminalStatus(patch.Status) && patch.DurationMs > 1000 {
		isNew, err := h.Store.GroupTimeBudgetExceedance(r.Context(), executionID, authProjectID, patch.DurationMs)
		if err != nil {
			h.Logger.Warn("time-budget grouping failed (continuing)",
				"execution_id", executionID,
				"duration_ms", patch.DurationMs,
				"error", err.Error(),
			)
		}
		h.maybeFireWebhook(r, authProjectID, store.FailureClassLoops, store.TimeBudgetSignature(patch.DurationMs), isNew, err)
	}

	// Phase 3b sub-slice 11: step-count detector. Any terminal execution
	// with > 10 events gets grouped as loops/step-count. Runs after the
	// crash and time-budget checks, so it's the lowest-priority
	// classification — an execution that crashed, took too long, AND
	// emitted lots of events ends up classified as crashes (first match
	// wins via the failure_group_id short-circuit). Threshold of 10 is
	// artificially low for v0.0.1 demo visibility; production default
	// 50+ per the concept doc.
	if isTerminalStatus(patch.Status) {
		count, err := h.Store.CountEventsForExecution(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("count events for step-count check failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if count > 10 {
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

	// Phase 3b sub-slice 13: tool-failures detector. If any tool_call
	// event in the execution had payload.status="failed", classify the
	// execution as tool_failures with signature=tool_name. Different
	// from crashes (where the exception escaped @wrap) — tool-failures
	// catches the silent-degradation pattern where the agent recovers
	// from a tool exception and ran to completion but produced
	// degraded output. Runs after the loop detectors so an execution
	// that BOTH had a failed tool AND was a runaway loop classifies
	// as the loop (loops are higher-priority — they waste more).
	if isTerminalStatus(patch.Status) {
		toolName, err := h.Store.FindFirstFailedToolName(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("find failed tool for detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if toolName != "" {
			isNew, gErr := h.Store.GroupToolFailure(r.Context(), executionID, authProjectID, toolName)
			if gErr != nil {
				h.Logger.Warn("tool-failure grouping failed (continuing)",
					"execution_id", executionID,
					"tool_name", toolName,
					"error", gErr.Error(),
				)
			}
			h.maybeFireWebhook(r, authProjectID, store.FailureClassToolFailures, toolName, isNew, gErr)
		}
	}

	// Phase 3b sub-slice 14: validator-failures detector. If any
	// validator_result event in the execution had payload.passed=false,
	// classify the execution as validator_failures with
	// signature=validator_name. Same silent-degradation family as
	// tool-failures — the agent ran to completion but produced output
	// that a downstream quality check failed.
	if isTerminalStatus(patch.Status) {
		validatorName, err := h.Store.FindFirstFailedValidator(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("find failed validator for detection failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else if validatorName != "" {
			isNew, gErr := h.Store.GroupValidatorFailure(r.Context(), executionID, authProjectID, validatorName)
			if gErr != nil {
				h.Logger.Warn("validator-failure grouping failed (continuing)",
					"execution_id", executionID,
					"validator_name", validatorName,
					"error", gErr.Error(),
				)
			}
			h.maybeFireWebhook(r, authProjectID, store.FailureClassValidator, validatorName, isNew, gErr)
		}
	}

	// Phase 3b sub-slices 12 + 15: events-driven post-processing. Both
	// cost computation and prompt-injection detection walk the same
	// event list, so fetch ONCE and feed both. Best-effort throughout —
	// failures here never fail the PATCH.
	if isTerminalStatus(patch.Status) {
		evts, err := h.Store.ListEventsForExecution(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("list events for post-PATCH processing failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else {
			// Sub-slice 12: cost computation. Skip if the SDK pre-filled
			// estimated_cost_usd (caller knows better than us). The
			// resolved cost (effectiveCost) flows into the cost-velocity
			// detector below so we don't recompute.
			effectiveCost := patch.EstimatedCostUSD
			if effectiveCost == 0 {
				cost := computeExecutionCost(evts)
				if cost > 0 {
					if err := h.Store.SetExecutionCost(r.Context(), executionID, cost); err != nil {
						h.Logger.Warn("set execution cost failed",
							"execution_id", executionID,
							"computed_cost_usd", cost,
							"error", err.Error(),
						)
					} else {
						h.Logger.Info("execution cost computed",
							"execution_id", executionID,
							"cost_usd", cost,
						)
					}
					effectiveCost = cost
				}
			}

			// Sub-slice 17: identical-call loop detector. Hashes
			// (model + user_message) per llm_call; if the same hash
			// appears 3+ times in one execution, group as
			// loops/identical_call. Runs BEFORE the injection check
			// because a runaway loop generating the same prompt
			// repeatedly is a more urgent resource-waste signal than
			// a single injection attempt embedded in the same prompt.
			if callHash, found := scanForIdenticalCalls(evts, 3); found {
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
			// identical_call misses — different exact text, same
			// semantic intent, ≥3 near-duplicates within one
			// execution. Runs AFTER identical_call so exact-text
			// loops win the more-specific signature; only loops with
			// varied wording reach this code path. Uses the same
			// trigram substrate as drift v2.
			similarMsgs := extractLLMUserMessages(evts)
			if len(similarMsgs) >= detectors.SimilarCallMinClusterSize {
				if callHash, found := detectors.DetectSimilarCallLoop(similarMsgs); found {
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
			// below) because a prompt-injection is a security event —
			// "this execution was attacked" is a more important
			// classification than "this execution was expensive."
			// The failure_group_id idempotency short-circuit means an
			// injection-classified execution skips cost-velocity even
			// if it would otherwise have matched.
			if pattern, found := scanForInjection(evts); found {
				isNew, gErr := h.Store.GroupPromptInjection(r.Context(), executionID, authProjectID, pattern)
				if gErr != nil {
					h.Logger.Warn("prompt-injection grouping failed (continuing)",
						"execution_id", executionID,
						"pattern", pattern,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassInjection, pattern, isNew, gErr)
			}

			// Sub-slice 16: cost-velocity detector. Any execution whose
			// resolved cost exceeds the v0.0.1 absolute threshold gets
			// grouped as cost_velocity with a cost-bucketed signature.
			// Production version (Phase 5+) compares against a project
			// rolling baseline rather than an absolute value.
			if effectiveCost > 0 {
				isNew, gErr := h.Store.GroupCostVelocity(r.Context(), executionID, authProjectID, effectiveCost)
				if gErr != nil {
					h.Logger.Warn("cost-velocity grouping failed (continuing)",
						"execution_id", executionID,
						"cost_usd", effectiveCost,
						"error", gErr.Error(),
					)
				}
				h.maybeFireWebhook(r, authProjectID, store.FailureClassCostVelocity, store.CostVelocitySignature(effectiveCost), isNew, gErr)
			}

			// Drift v2 — lexical signal. Char-3-gram cosine distance
			// between current execution's user_messages and the
			// project's recent history. Runs LAST in the chain on
			// purpose: lexical drift is a SOFT behavioral signal that
			// should only surface for executions nothing else
			// classified. The idempotency short-circuit in
			// GroupDriftSignal means any execution already grouped
			// (crashes, loops, tool_failures, validator_failures,
			// prompt_injection, cost_velocity, or model-drift) skips
			// drift v2 — which is the right priority: specific causal
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
					if signature, distance, drift := detectors.DetectLexicalDrift(currentMsgs, historicalMsgs); drift {
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

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":           true,
		"execution_id": patch.ExecutionID,
		"status":       patch.Status,
	})
}

// HandleListFailureGroups returns the calling project's failure groups,
// sorted by most-recent first. Supports `limit` (default 50, max 200) and
// `offset` (default 0) via query string.
func (h *Handlers) HandleListFailureGroups(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	limit := parseIntQuery(r, "limit", 50, 1, 200)
	offset := parseIntQuery(r, "offset", 0, 0, 1_000_000)

	groups, err := h.Store.ListFailureGroups(r.Context(), authProjectID, limit, offset)
	if err != nil {
		h.Logger.Error("list failure_groups failed",
			"project_id", authProjectID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "list failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":             true,
		"failure_groups": groups,
		"count":          len(groups),
		"limit":          limit,
		"offset":         offset,
	})
}

// HandleGetFailureGroup returns a single failure_group by id. Returns 404
// both when the group doesn't exist AND when the group belongs to a
// different project than the caller (don't leak group_id existence
// across tenants).
func (h *Handlers) HandleGetFailureGroup(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	if groupID == "" {
		writeError(w, http.StatusBadRequest, "group_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	group, err := h.Store.GetFailureGroup(r.Context(), groupID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "failure group not found")
			return
		}
		h.Logger.Error("get failure_group failed",
			"group_id", groupID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}

	if group.ProjectID != authProjectID {
		// Don't reveal that the group exists in another project — return
		// 404 same as a non-existent group.
		writeError(w, http.StatusNotFound, "failure group not found")
		return
	}

	writeJSON(w, http.StatusOK, group)
}

// HandleIngestEvents accepts a batch of Events. Batching is required —
// the SDK buffers events client-side and flushes in groups of ~100, so
// the ingest path is array-shaped from day one. A single-event POST is
// accepted as a 1-element array; rejecting non-array bodies catches
// SDK bugs early.
func (h *Handlers) HandleIngestEvents(w http.ResponseWriter, r *http.Request) {
	var batch []events.Event
	if err := decodeJSON(r, &batch); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON: "+err.Error())
		return
	}
	if len(batch) == 0 {
		writeError(w, http.StatusBadRequest, "empty event batch")
		return
	}

	// First pass: validate and defaulting. Reject malformed events
	// individually so a single bad event in a batch doesn't poison the
	// whole transaction.
	accepted := make([]events.Event, 0, len(batch))
	rejected := 0
	for i := range batch {
		evt := &batch[i]
		if evt.EventID == "" || evt.ExecutionID == "" || evt.EventType == "" {
			rejected++
			h.Logger.Warn("event rejected: required field missing",
				"event_index", i,
				"event_id", evt.EventID,
				"execution_id", evt.ExecutionID,
				"event_type", evt.EventType,
			)
			continue
		}
		if evt.Timestamp.IsZero() {
			evt.Timestamp = time.Now().UTC()
		}
		accepted = append(accepted, *evt)
	}

	if err := h.Store.SaveEvents(r.Context(), accepted); err != nil {
		h.Logger.Error("save events failed", "error", err.Error(), "batch_size", len(accepted))
		writeError(w, http.StatusInternalServerError, "persist failed: "+err.Error())
		return
	}

	for _, evt := range accepted {
		h.Logger.Info("event ingested",
			"event_id", evt.EventID,
			"execution_id", evt.ExecutionID,
			"event_type", evt.EventType,
			"sequence", evt.Sequence,
			"duration_ms", evt.DurationMs,
			"payload_bytes", len(evt.Payload),
		)
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"accepted": len(accepted),
		"rejected": rejected,
	})
}

// ─────────────────────────────────────────────────────────────────────────
// Helpers
// ─────────────────────────────────────────────────────────────────────────

// decodeJSON enforces strict decoding — unknown JSON fields cause a 400.
// Strict decoding catches schema drift early during SDK development;
// once the schema is stable post-Phase 4 we may relax to forward-compat.
func decodeJSON(r *http.Request, dst any) error {
	dec := json.NewDecoder(r.Body)
	dec.DisallowUnknownFields()
	return dec.Decode(dst)
}

// writeJSON writes a JSON response with the given status code. Errors
// during write are logged-and-ignored: there's nothing useful to do at
// that point and the client has already received the status.
func writeJSON(w http.ResponseWriter, status int, body any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if err := json.NewEncoder(w).Encode(body); err != nil {
		// Cannot send another response now (header is committed); just log.
		fmt.Fprintf(w, `{"ok":false,"error":"response encode failed: %s"}`, err.Error())
	}
}

// writeError is a convenience wrapper for the standard error response shape.
func writeError(w http.ResponseWriter, status int, message string) {
	writeJSON(w, status, map[string]any{
		"ok":    false,
		"error": message,
	})
}

// HandleListExecutions returns the calling project's executions, sorted
// by started_at DESC. Supports `limit` (default 50, max 200) and `offset`
// (default 0).
func (h *Handlers) HandleListExecutions(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	limit := parseIntQuery(r, "limit", 50, 1, 200)
	offset := parseIntQuery(r, "offset", 0, 0, 1_000_000)

	execs, err := h.Store.ListExecutions(r.Context(), authProjectID, limit, offset)
	if err != nil {
		h.Logger.Error("list executions failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "list failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"executions": execs,
		"count":      len(execs),
		"limit":      limit,
		"offset":     offset,
	})
}

// HandleGetExecution returns a single execution + its events (sorted by
// sequence ASC). Cross-tenant access returns 404 to avoid leaking which
// execution IDs exist on other projects.
func (h *Handlers) HandleGetExecution(w http.ResponseWriter, r *http.Request) {
	executionID := r.PathValue("id")
	if executionID == "" {
		writeError(w, http.StatusBadRequest, "execution_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	exec, err := h.Store.GetExecution(r.Context(), executionID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "execution not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if exec.ProjectID != authProjectID {
		writeError(w, http.StatusNotFound, "execution not found")
		return
	}

	evts, err := h.Store.ListEventsForExecution(r.Context(), executionID)
	if err != nil {
		h.Logger.Warn("list events failed (returning execution without events)",
			"execution_id", executionID,
			"error", err.Error(),
		)
		evts = nil
	}

	// Server-side token aggregation: if the SDK didn't explicitly PATCH
	// total_tokens_in / total_tokens_out on the terminal-status update,
	// derive them from the llm_call event payloads. Lets adapters
	// (LangChain, CrewAI, etc.) and bare emit_llm_call() callers show
	// accurate execution-level totals without requiring every SDK to
	// thread a running counter into the @wrap exit path.
	//
	// SDK-supplied values win when present (non-zero) — a future SDK
	// slice can authoritatively report totals (e.g. accumulating
	// across streaming chunks the event payloads can't see) and the
	// dashboard will trust that report.
	if exec.TotalTokensIn == 0 && exec.TotalTokensOut == 0 {
		var sumIn, sumOut int
		for _, e := range evts {
			if e == nil || e.EventType != events.EventTypeLLMCall {
				continue
			}
			if e.Payload == nil {
				continue
			}
			// Payload is a json.RawMessage; cheaply parse just the
			// two numeric fields we need.
			var p struct {
				InputTokens  int `json:"input_tokens"`
				OutputTokens int `json:"output_tokens"`
			}
			if err := json.Unmarshal(e.Payload, &p); err == nil {
				sumIn += p.InputTokens
				sumOut += p.OutputTokens
			}
		}
		exec.TotalTokensIn = sumIn
		exec.TotalTokensOut = sumOut
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":        true,
		"execution": exec,
		"events":    evts,
	})
}

// HandleListExecutionsInFailureGroup returns the executions that belong
// to a given failure_group. Verifies cross-tenant access by first
// fetching the group and confirming group.project_id == auth project —
// 404 if it doesn't match (don't leak group_id existence).
func (h *Handlers) HandleListExecutionsInFailureGroup(w http.ResponseWriter, r *http.Request) {
	groupID := r.PathValue("id")
	if groupID == "" {
		writeError(w, http.StatusBadRequest, "group_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	// Authorization: verify the group belongs to the caller's project.
	group, err := h.Store.GetFailureGroup(r.Context(), groupID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "failure group not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	if group.ProjectID != authProjectID {
		writeError(w, http.StatusNotFound, "failure group not found")
		return
	}

	limit := parseIntQuery(r, "limit", 50, 1, 200)
	offset := parseIntQuery(r, "offset", 0, 0, 1_000_000)

	execs, err := h.Store.ListExecutionsByFailureGroup(r.Context(), groupID, limit, offset)
	if err != nil {
		h.Logger.Error("list executions by failure_group failed",
			"group_id", groupID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "list failed: "+err.Error())
		return
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"executions": execs,
		"count":      len(execs),
		"limit":      limit,
		"offset":     offset,
		"group_id":   groupID,
	})
}

// HandleStats returns top-line stat-card numbers for the dashboard:
// total executions, total failure groups, crashed-in-last-24h,
// average duration_ms across completed executions.
//
// Implementation is deliberately simple: a few small COUNT queries.
// When the executions table grows beyond hand-friendly size, we'll
// either cache these or migrate to time-bucketed aggregates.
func (h *Handlers) HandleStats(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	ctx := r.Context()

	totalExecutions, err := h.Store.CountExecutionsByStatusSince(ctx, authProjectID, "", time.Time{})
	if err != nil {
		// Fall back to 0 on individual count failures so the dashboard
		// degrades gracefully rather than 500-ing for one bad query.
		h.Logger.Warn("count total executions failed", "error", err.Error())
	}
	cutoff24h := time.Now().Add(-24 * time.Hour)
	crashed24h, err := h.Store.CountExecutionsByStatusSince(ctx, authProjectID, string(events.StatusCrashed), cutoff24h)
	if err != nil {
		h.Logger.Warn("count crashed-24h failed", "error", err.Error())
	}
	completedAllTime, err := h.Store.CountExecutionsByStatusSince(ctx, authProjectID, string(events.StatusCompleted), time.Time{})
	if err != nil {
		h.Logger.Warn("count completed failed", "error", err.Error())
	}

	groups, err := h.Store.ListFailureGroups(ctx, authProjectID, 1000, 0)
	if err != nil {
		h.Logger.Warn("list failure_groups for stats failed", "error", err.Error())
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":                  true,
		"total_executions":    totalExecutions,
		"completed_executions": completedAllTime,
		"crashed_24h":         crashed24h,
		"open_failure_groups": len(groups),
	})
}

// computeExecutionCost sums the estimated USD cost across every
// llm_call event in the slice. Each event's payload is unmarshaled into
// a small struct extracting only the three fields cost-computation
// needs (model, input_tokens, output_tokens); everything else is
// ignored, so changes to the payload schema (adding fields) don't
// affect this code.
//
// Events with unknown models contribute 0 (pricing.ComputeLLMCost
// returns 0 for keys not in the pricing table). Events whose payload
// fails to unmarshal are skipped silently — a single malformed event
// shouldn't break cost computation for the whole execution.
func computeExecutionCost(evts []*events.Event) float64 {
	total := 0.0
	for _, e := range evts {
		if e.EventType != events.EventTypeLLMCall {
			continue
		}
		if len(e.Payload) == 0 {
			continue
		}
		var p struct {
			Model        string `json:"model"`
			InputTokens  int    `json:"input_tokens"`
			OutputTokens int    `json:"output_tokens"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		total += pricing.ComputeLLMCost(p.Model, p.InputTokens, p.OutputTokens)
	}
	return total
}

// extractLLMUserMessages walks the event list and returns the
// user_message field from every llm_call event, in sequence order.
// Used by the similar-call loop detector to assemble the corpus for
// pairwise cosine-distance clustering. Skips events whose payload is
// missing or malformed — the detector handles empty slices.
//
// Mirrors computeExecutionCost's payload-shape-tolerant approach:
// unmarshal into a tiny struct that extracts only the field we need,
// ignore the rest. Survives any future payload schema additions.
func extractLLMUserMessages(evts []*events.Event) []string {
	out := make([]string, 0, len(evts))
	for _, e := range evts {
		if e == nil || e.EventType != events.EventTypeLLMCall {
			continue
		}
		if len(e.Payload) == 0 {
			continue
		}
		var p struct {
			UserMessage string `json:"user_message"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		if p.UserMessage != "" {
			out = append(out, p.UserMessage)
		}
	}
	return out
}

// ─────────────────────────────────────────────────────────────────────────
// API key management (sub-slice 18)
// ─────────────────────────────────────────────────────────────────────────

// HandleListAPIKeys returns the calling project's API keys (without
// the hash — never serialized).
func (h *Handlers) HandleListAPIKeys(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	keys, err := h.Store.ListAPIKeysForProject(r.Context(), authProjectID)
	if err != nil {
		h.Logger.Error("list api_keys failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "list failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"api_keys": keys,
		"count":    len(keys),
	})
}

// HandleCreateAPIKey mints a new API key for the calling project and
// returns the RAW KEY VALUE ONCE — this is the only moment a caller
// ever sees it. The server only persists the hash. Caller must store
// the raw key immediately; a lost raw key requires a new mint.
//
// Request body (optional): {"name": "human-readable label"}.
func (h *Handlers) HandleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	var body struct {
		Name string `json:"name,omitempty"`
	}
	// Empty body is fine — name is optional. Skip the strict-decode
	// path here because the field is intentionally permissive.
	if r.ContentLength > 0 {
		_ = json.NewDecoder(r.Body).Decode(&body)
	}

	rawKey, hash, prefix, err := MintAPIKey()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "mint key: "+err.Error())
		return
	}

	keyID := "key-" + prefix[len("mesedi_sk_"):] + "-" + fmt.Sprintf("%d", time.Now().UnixNano())
	rec := &store.APIKey{
		KeyID:     keyID,
		ProjectID: authProjectID,
		KeyHash:   hash,
		KeyPrefix: prefix,
		Name:      body.Name,
	}
	if err := h.Store.CreateAPIKey(r.Context(), rec); err != nil {
		writeError(w, http.StatusInternalServerError, "persist key: "+err.Error())
		return
	}

	h.Logger.Info("api key minted",
		"key_id", keyID,
		"prefix", prefix,
		"project_id", authProjectID,
		"name", body.Name,
	)

	// Return the raw key in this ONE response. The hash never leaves.
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"key_id":   keyID,
		"raw_key":  rawKey,
		"prefix":   prefix,
		"name":     body.Name,
		"warning":  "Store this raw_key now — it will never be shown again.",
	})
}

// HandleRevokeAPIKey hard-deletes an API key. Project-scoped via the
// Store method's project_id guard.
func (h *Handlers) HandleRevokeAPIKey(w http.ResponseWriter, r *http.Request) {
	keyID := r.PathValue("id")
	if keyID == "" {
		writeError(w, http.StatusBadRequest, "key_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	if err := h.Store.DeleteAPIKey(r.Context(), keyID, authProjectID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "api key not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.Logger.Info("api key revoked",
		"key_id", keyID,
		"project_id", authProjectID,
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":     true,
		"key_id": keyID,
	})
}

// ─────────────────────────────────────────────────────────────────────────
// Webhook escalation config (task #83 slice 1)
// ─────────────────────────────────────────────────────────────────────────

// validFailureClasses is the allowlist of class names accepted in the
// `enabled_classes` field on POST /webhooks. Bigger than the
// FailureClass* constants in store.go feels like duplication, but it
// lets us treat the webhook layer's accepted classes as an
// independently-evolving surface from the detector's emitted classes.
var validFailureClasses = map[string]struct{}{
	store.FailureClassCrashes:      {},
	store.FailureClassLoops:        {},
	store.FailureClassToolFailures: {},
	store.FailureClassValidator:    {},
	store.FailureClassDrift:        {},
	store.FailureClassCostVelocity: {},
	store.FailureClassInjection:    {},
}

// HandleListWebhooks returns the calling project's webhooks. The
// `secret` field is never serialized — it's only ever shown once at
// creation time.
func (h *Handlers) HandleListWebhooks(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	hooks, err := h.Store.ListProjectWebhooksForProject(r.Context(), authProjectID)
	if err != nil {
		h.Logger.Error("list webhooks failed", "error", err.Error())
		writeError(w, http.StatusInternalServerError, "list failed: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":       true,
		"webhooks": hooks,
		"count":    len(hooks),
	})
}

// HandleCreateWebhook registers a new webhook for the calling project
// and returns the generated secret ONCE. Subsequent list responses
// omit the secret.
//
// Request body:
//
//	{
//	  "name":            "string (optional, human label)",
//	  "url":             "https://... (required, must be http(s))",
//	  "enabled_classes": ["crashes","tool_failures"] (optional; empty/missing = all),
//	  "enabled":         true (optional, default true)
//	}
//
// The dispatcher (slice 2) will only deliver to webhooks where
// enabled=true and the failure_group's class is either in
// enabled_classes OR enabled_classes is empty.
func (h *Handlers) HandleCreateWebhook(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	var body struct {
		Name           string   `json:"name,omitempty"`
		URL            string   `json:"url"`
		EnabledClasses []string `json:"enabled_classes,omitempty"`
		Enabled        *bool    `json:"enabled,omitempty"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		writeError(w, http.StatusBadRequest, "invalid json: "+err.Error())
		return
	}

	// URL validation: must be parseable, must have http/https scheme,
	// must have a host. Anything else is a misconfiguration that would
	// just generate dispatcher-side errors later.
	body.URL = strings.TrimSpace(body.URL)
	if body.URL == "" {
		writeError(w, http.StatusBadRequest, "url required")
		return
	}
	parsed, err := url.Parse(body.URL)
	if err != nil || parsed.Host == "" {
		writeError(w, http.StatusBadRequest, "url is not a valid URL")
		return
	}
	if parsed.Scheme != "http" && parsed.Scheme != "https" {
		writeError(w, http.StatusBadRequest, "url must use http or https scheme")
		return
	}

	// Validate enabled_classes — every entry must match a known class.
	// Unknown class names would just silently never fire, which is the
	// worst failure mode for an alerting feature; reject loudly.
	for _, c := range body.EnabledClasses {
		if _, known := validFailureClasses[c]; !known {
			writeError(w, http.StatusBadRequest,
				"unknown failure_class: "+c+" (valid: crashes, loops, tool_failures, validator_failures, drift, cost_velocity, prompt_injection)")
			return
		}
	}

	enabled := true
	if body.Enabled != nil {
		enabled = *body.Enabled
	}

	// Generate webhook_id + secret. webhook_id is a short stable
	// identifier for client-side reference; secret is 32 bytes of
	// random entropy hex-encoded (256-bit HMAC key, industry-standard
	// strength for symmetric webhook signing).
	webhookID, err := newWebhookID()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate webhook_id: "+err.Error())
		return
	}
	secret, err := newWebhookSecret()
	if err != nil {
		writeError(w, http.StatusInternalServerError, "generate secret: "+err.Error())
		return
	}

	rec := &store.ProjectWebhook{
		WebhookID:      webhookID,
		ProjectID:      authProjectID,
		Name:           body.Name,
		URL:            body.URL,
		Secret:         secret,
		EnabledClasses: body.EnabledClasses,
		Enabled:        enabled,
	}
	if err := h.Store.CreateProjectWebhook(r.Context(), rec); err != nil {
		writeError(w, http.StatusInternalServerError, "persist webhook: "+err.Error())
		return
	}

	h.Logger.Info("webhook created",
		"webhook_id", webhookID,
		"project_id", authProjectID,
		"url", body.URL,
		"enabled", enabled,
		"class_filter_count", len(body.EnabledClasses),
	)

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":              true,
		"webhook_id":      webhookID,
		"url":             body.URL,
		"name":            body.Name,
		"enabled_classes": body.EnabledClasses,
		"enabled":         enabled,
		"secret":          secret,
		"warning":         "Store this secret now — it will never be shown again. Use it to verify the X-Mesedi-Signature header on inbound webhook deliveries.",
	})
}

// HandleDeleteWebhook hard-deletes a webhook. Project-scoped via the
// store method's project_id guard; cross-tenant id-guessing returns
// 404, not 403, to avoid leaking which ids exist.
func (h *Handlers) HandleDeleteWebhook(w http.ResponseWriter, r *http.Request) {
	webhookID := r.PathValue("id")
	if webhookID == "" {
		writeError(w, http.StatusBadRequest, "webhook_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	if err := h.Store.DeleteProjectWebhook(r.Context(), webhookID, authProjectID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "webhook not found")
			return
		}
		writeError(w, http.StatusInternalServerError, err.Error())
		return
	}
	h.Logger.Info("webhook deleted",
		"webhook_id", webhookID,
		"project_id", authProjectID,
	)
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"webhook_id": webhookID,
	})
}

// HandleTestWebhook fires a synthetic delivery against a webhook so an
// operator can verify the receiver is reachable and HMAC-verifying
// correctly. Blocks until the delivery resolves (delivered or failed
// after retries) so the response carries the outcome.
//
// Project-scoped: the webhook must belong to the calling project. The
// dashboard URL embedded in the payload is derived from the request's
// Host header — adequate for local-dev; a future slice will make this
// configurable via a flag/env var for production deployments.
func (h *Handlers) HandleTestWebhook(w http.ResponseWriter, r *http.Request) {
	webhookID := r.PathValue("id")
	if webhookID == "" {
		writeError(w, http.StatusBadRequest, "webhook_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	wh, err := h.Store.GetProjectWebhook(r.Context(), webhookID, authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "webhook not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "lookup webhook: "+err.Error())
		return
	}

	// Derive a dashboard base URL from the inbound request — best-effort
	// for slice 2. Production deployments would set this from config.
	scheme := "http"
	if r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") {
		scheme = "https"
	}
	dashboardBase := scheme + "://" + r.Host

	// Build a delivery_id up front so the payload echoes it back to the
	// receiver in the X-Mesedi-Event-Id header (the receiver can use it
	// for idempotency).
	rndBuf := make([]byte, 8)
	if _, err := rand.Read(rndBuf); err != nil {
		writeError(w, http.StatusInternalServerError, "generate delivery_id: "+err.Error())
		return
	}
	deliveryID := "del-" + hex.EncodeToString(rndBuf)

	payload := webhooks.BuildTestPayload(wh, dashboardBase, deliveryID)

	// Run delivery — synchronous for slice 2; slice 3's auto-fire path
	// will run this in a goroutine.
	result, attempts := webhooks.Deliver(r.Context(), h.Logger, h.WebhookClient, wh, payload)

	// Persist every attempt to the deliveries log (best-effort: a
	// persistence error here doesn't change the operator-visible
	// outcome, but does get logged).
	for i := range attempts {
		if err := h.Store.RecordWebhookDelivery(r.Context(), &attempts[i]); err != nil {
			h.Logger.Warn("record webhook delivery failed (continuing)",
				"webhook_id", webhookID,
				"attempt", attempts[i].Attempt,
				"error", err.Error(),
			)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          result.Status == "delivered",
		"webhook_id":  webhookID,
		"delivery_id": deliveryID,
		"status":      result.Status,
		"attempts":    result.Attempts,
		"http_status": result.HTTPStatus,
		"error":       result.Error,
		"duration_ms": result.DurationMs,
		"payload":     payload,
	})
}

// HandleListWebhookDeliveries returns the most recent delivery attempts
// for a webhook. Project-scoped via the webhook lookup. Default limit
// 50; capped at 200.
func (h *Handlers) HandleListWebhookDeliveries(w http.ResponseWriter, r *http.Request) {
	webhookID := r.PathValue("id")
	if webhookID == "" {
		writeError(w, http.StatusBadRequest, "webhook_id path parameter required")
		return
	}
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	// Confirm webhook belongs to project before returning its log.
	if _, err := h.Store.GetProjectWebhook(r.Context(), webhookID, authProjectID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "webhook not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "lookup webhook: "+err.Error())
		return
	}

	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n > 0 {
			limit = n
		}
	}
	if limit > 200 {
		limit = 200
	}

	deliveries, err := h.Store.ListDeliveriesForWebhook(r.Context(), webhookID, limit)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "list deliveries: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":         true,
		"webhook_id": webhookID,
		"deliveries": deliveries,
		"count":      len(deliveries),
	})
}

// newWebhookID returns a short stable identifier for a webhook row.
// Format: "wh-<16-hex-chars>" — readable in logs, sortable, no
// information leak about creation time. 64 bits of entropy is plenty
// for a per-project identifier space.
func newWebhookID() (string, error) {
	var buf [8]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return "wh-" + hex.EncodeToString(buf[:]), nil
}

// newWebhookSecret returns a 256-bit random secret as a 64-char hex
// string. Used as the HMAC key the dispatcher signs payloads with;
// the receiver verifies signatures using the same value.
func newWebhookSecret() (string, error) {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return "", err
	}
	return hex.EncodeToString(buf[:]), nil
}

// scanForIdenticalCalls returns the short-hex hash of an LLM call
// (model + user_message) that appears at least `threshold` times in
// the event slice, plus true. If no call repeats that many times,
// returns ("", false). The hash is the SHA-256 of model+user_message
// truncated to 8 hex chars — readable, collision-resistant at scale,
// and acts as the failure_group signature so distinct repeated
// prompts cluster into distinct groups.
//
// Detection fires on the FIRST event that pushes a hash to the
// threshold — earlier events of the same hash are already counted but
// haven't yet crossed the line. This makes the function cheap (O(n)
// with early return) without needing to scan the entire event list
// twice.
func scanForIdenticalCalls(evts []*events.Event, threshold int) (string, bool) {
	counts := make(map[string]int, 8)
	for _, e := range evts {
		if e.EventType != events.EventTypeLLMCall {
			continue
		}
		if len(e.Payload) == 0 {
			continue
		}
		var p struct {
			Model       string `json:"model"`
			UserMessage string `json:"user_message"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		sum := sha256.Sum256([]byte(p.Model + "\x00" + p.UserMessage))
		short := hex.EncodeToString(sum[:4])
		counts[short]++
		if counts[short] >= threshold {
			return short, true
		}
	}
	return "", false
}

// scanForInjection walks llm_call events looking for known prompt-
// injection signatures in the user_message and system_prompt fields.
// Returns the first matching pattern's name plus true; ("", false) if
// nothing matched. The scan is ordered by event sequence so the first
// injection chronologically wins.
//
// Both user_message and system_prompt are scanned because injections
// can come from either side — a compromised system prompt is rarer
// but more dangerous, so we want it caught.
func scanForInjection(evts []*events.Event) (string, bool) {
	for _, e := range evts {
		if e.EventType != events.EventTypeLLMCall {
			continue
		}
		if len(e.Payload) == 0 {
			continue
		}
		var p struct {
			UserMessage  string `json:"user_message"`
			SystemPrompt string `json:"system_prompt"`
		}
		if err := json.Unmarshal(e.Payload, &p); err != nil {
			continue
		}
		if name, found := detectors.DetectInjection(p.UserMessage); found {
			return name, true
		}
		if name, found := detectors.DetectInjection(p.SystemPrompt); found {
			return name, true
		}
	}
	return "", false
}

// isTerminalStatus returns true for any execution status that means
// "the agent run is over." Detection passes (time-budget, future
// drift / cost-velocity) only fire on terminal statuses — running
// executions don't have a final duration yet.
func isTerminalStatus(s events.ExecutionStatus) bool {
	switch s {
	case events.StatusCompleted,
		events.StatusCrashed,
		events.StatusHalted,
		events.StatusTimeout,
		events.StatusValidationFailed:
		return true
	default:
		return false
	}
}

// parseIntQuery returns the integer value of a URL query parameter,
// falling back to defaultVal if missing/invalid. Clamps the result to
// [min, max]. Used by list endpoints for limit/offset.
func parseIntQuery(r *http.Request, key string, defaultVal, min, max int) int {
	raw := r.URL.Query().Get(key)
	if raw == "" {
		return defaultVal
	}
	v, err := strconv.Atoi(raw)
	if err != nil {
		return defaultVal
	}
	if v < min {
		return min
	}
	if v > max {
		return max
	}
	return v
}

// HandleGetProject returns the authenticated project's identity. Used
// by the dashboard to show "Project: <name>" in the topbar and welcome
// screens, and by /app/settings to display + (eventually) rename the
// project.
//
// Returns project_id, name, owner_email, created_at. Does not return
// the API key prefix or any sensitive material — the calling client
// already has the key in localStorage and any rename/revoke flows
// happen through other endpoints that already audit-log by key_id.
func (h *Handlers) HandleGetProject(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
	project, err := h.Store.GetProject(r.Context(), authProjectID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			writeError(w, http.StatusNotFound, "project not found")
			return
		}
		writeError(w, http.StatusInternalServerError, "get project: "+err.Error())
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"ok":          true,
		"project_id":  project.ProjectID,
		"name":        project.Name,
		"owner_email": project.OwnerEmail,
		"created_at":  project.CreatedAt,
	})
}
