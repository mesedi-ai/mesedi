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
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"mesedi/backend/internal/detectors"
	"mesedi/backend/internal/events"
	"mesedi/backend/internal/pricing"
	"mesedi/backend/internal/store"
)

// Handlers carries dependencies needed by HTTP handlers. As more
// subsystems come online (storage, detectors, etc.) they get attached
// here rather than passed through each handler signature.
type Handlers struct {
	Logger *slog.Logger
	Store  store.Store
}

// New constructs the Handlers value. Done as a constructor (rather than
// a literal) so the dependencies become explicit as the surface grows.
func New(logger *slog.Logger, s store.Store) *Handlers {
	return &Handlers{Logger: logger, Store: s}
}

// RegisterRoutes attaches every protected route to the provided ServeMux.
// Keep this list short and explicit — it doubles as the API surface
// inventory for documentation.
func (h *Handlers) RegisterRoutes(mux *http.ServeMux) {
	// Phase 1 ingest surface.
	mux.HandleFunc("POST /executions", h.HandleCreateExecution)
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

	// Phase 3a: link crashed executions to their failure_group. Best-effort —
	// a grouping failure doesn't fail the PATCH because the execution itself
	// is already correctly recorded.
	if patch.Status == events.StatusCrashed && patch.CrashSignature != "" {
		if err := h.Store.GroupCrashedExecution(r.Context(), executionID, authProjectID, patch.CrashSignature); err != nil {
			h.Logger.Warn("crash grouping failed (continuing)",
				"execution_id", executionID,
				"crash_signature", patch.CrashSignature,
				"error", err.Error(),
			)
		}
	}

	// Phase 3b sub-slice 10: time-budget detector. Any terminal execution
	// whose duration exceeded the threshold gets grouped as loops/time-budget.
	// Runs AFTER the crash check so a crashed-and-slow execution is
	// classified as crashes (the idempotency check in the store short-
	// circuits this call). The threshold is hardcoded at 1s for v0.0.1;
	// production default will be 60s, configurable per-project later.
	if isTerminalStatus(patch.Status) && patch.DurationMs > 1000 {
		if err := h.Store.GroupTimeBudgetExceedance(r.Context(), executionID, authProjectID, patch.DurationMs); err != nil {
			h.Logger.Warn("time-budget grouping failed (continuing)",
				"execution_id", executionID,
				"duration_ms", patch.DurationMs,
				"error", err.Error(),
			)
		}
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
			if err := h.Store.GroupStepCountExceedance(r.Context(), executionID, authProjectID, count); err != nil {
				h.Logger.Warn("step-count grouping failed (continuing)",
					"execution_id", executionID,
					"event_count", count,
					"error", err.Error(),
				)
			}
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
			if err := h.Store.GroupToolFailure(r.Context(), executionID, authProjectID, toolName); err != nil {
				h.Logger.Warn("tool-failure grouping failed (continuing)",
					"execution_id", executionID,
					"tool_name", toolName,
					"error", err.Error(),
				)
			}
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
			if err := h.Store.GroupValidatorFailure(r.Context(), executionID, authProjectID, validatorName); err != nil {
				h.Logger.Warn("validator-failure grouping failed (continuing)",
					"execution_id", executionID,
					"validator_name", validatorName,
					"error", err.Error(),
				)
			}
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

			// Sub-slice 16: cost-velocity detector. Any execution whose
			// resolved cost exceeds the v0.0.1 absolute threshold gets
			// grouped as cost_velocity with a cost-bucketed signature.
			// Production version (Phase 5+) compares against a project
			// rolling baseline rather than an absolute value.
			if effectiveCost > 0 {
				if err := h.Store.GroupCostVelocity(r.Context(), executionID, authProjectID, effectiveCost); err != nil {
					h.Logger.Warn("cost-velocity grouping failed (continuing)",
						"execution_id", executionID,
						"cost_usd", effectiveCost,
						"error", err.Error(),
					)
				}
			}

			// Sub-slice 15: prompt-injection detection. Scan each
			// llm_call event's user_message + system_prompt for known
			// injection patterns. First match wins; the pattern name
			// becomes the failure_group signature so all executions
			// hitting the same attack pattern cluster together.
			if pattern, found := scanForInjection(evts); found {
				if err := h.Store.GroupPromptInjection(r.Context(), executionID, authProjectID, pattern); err != nil {
					h.Logger.Warn("prompt-injection grouping failed (continuing)",
						"execution_id", executionID,
						"pattern", pattern,
						"error", err.Error(),
					)
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
