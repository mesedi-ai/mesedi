package api

// Detector-status handler (empty-states wave A). Single endpoint
// surfaces per-detector observability metadata customers need to
// understand whether a detector is silently no-op'ing for their
// project. Closes the backend half of:
//
//   - semantic_loop.G2 — detector invisible to customers who never
//     called mesedi.checkpoint(); response carries
//     has_checkpoint_data + last_checkpoint_at.
//   - tool_schema_drift.G2 — detector silently primes for the first
//     N (default 10) calls of each tool; response carries the per-
//     tool call_count plus the project's min_history_calls so the
//     dashboard can render the priming progress bar.
//
// Generic shape so future detector-status fields (grounding_failure
// no-eval-scores, infrastructure_throttled no-infra-events,
// sandbox_escape custom-patterns-dormant, etc.) can be added without
// a new endpoint per detector. Customers hit this once per
// dashboard-overview page load — not on any per-execution hot path.
//
// Auth: same Bearer-key flow as the other /me/* and /v1/* endpoints.
// Errors fall through to a permissive empty-data response shape so
// the dashboard renders a "status unavailable" chip rather than a
// crash; the response carries an `error` field naming the underlying
// failure for operator triage.

import (
	"net/http"
	"time"

	"mesedi/backend/internal/detectors"
)

// DetectorStatusResponse is the canonical detector-status payload.
// Generic-shaped so future detectors can add fields without
// breaking the existing keys.
type DetectorStatusResponse struct {
	ProjectID       string                       `json:"project_id"`
	SemanticLoop    SemanticLoopStatus           `json:"semantic_loop"`
	ToolSchemaDrift ToolSchemaDriftStatus        `json:"tool_schema_drift"`
	// Error is populated when the underlying store reads failed but
	// the response was still rendered so the dashboard can show a
	// non-crashing fallback. Empty when all data loaded cleanly.
	Error string `json:"error,omitempty"`
}

// SemanticLoopStatus carries the two signals the dashboard needs to
// render the empty-state banner: whether the project has ever
// emitted a checkpoint event + the most-recent one. The empty state
// fires when has_checkpoint_data is false.
type SemanticLoopStatus struct {
	HasCheckpointData  bool       `json:"has_checkpoint_data"`
	LastCheckpointAt   *time.Time `json:"last_checkpoint_at,omitempty"`
	CheckpointCount    int        `json:"checkpoint_count"`
}

// ToolSchemaDriftStatus carries the per-tool priming progress for
// every tool the project has invoked. MinHistoryCalls is the
// per-project threshold from the Theme B primitive — the dashboard
// compares each tool's call_count against it to decide whether to
// render "priming N/M observed" or "drift detection active".
type ToolSchemaDriftStatus struct {
	Tools           []ToolCallCount `json:"tools"`
	MinHistoryCalls int             `json:"min_history_calls"`
}

// ToolCallCount mirrors store.ToolCallCount in the API package so
// the JSON tags are owned by the api package's response surface.
// Two fields kept aligned with the store type by convention.
type ToolCallCount struct {
	ToolName  string `json:"tool_name"`
	CallCount int    `json:"call_count"`
}

// HandleGetDetectorStatus serves GET /v1/detector-status. Returns
// the generic observability surface; falls back to empty data with
// an error field on store failure rather than 500 so the dashboard
// degrades gracefully.
//
//	GET /v1/detector-status
//	→ 200 {project_id, semantic_loop: {...}, tool_schema_drift: {...}}
//
// Auth: Bearer key. Tier-agnostic; available to Hobby/Team/Enterprise.
func (h *Handlers) HandleGetDetectorStatus(w http.ResponseWriter, r *http.Request) {
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}

	resp := DetectorStatusResponse{
		ProjectID: authProjectID,
		SemanticLoop: SemanticLoopStatus{
			HasCheckpointData: false,
			CheckpointCount:   0,
		},
		ToolSchemaDrift: ToolSchemaDriftStatus{
			Tools:           []ToolCallCount{},
			MinHistoryCalls: detectors.DefaultToolSchemaDriftThresholds().MinHistoryCalls,
		},
	}

	// semantic_loop: count checkpoint events; success path populates
	// HasCheckpointData true when count > 0.
	count, lastAt, err := h.Store.CountCheckpointEventsForProject(r.Context(), authProjectID)
	if err != nil {
		h.Logger.Warn("detector-status: checkpoint count failed (returning empty)",
			"project_id", authProjectID, "error", err.Error())
		resp.Error = "partial: checkpoint count unavailable"
	} else {
		resp.SemanticLoop.CheckpointCount = count
		resp.SemanticLoop.HasCheckpointData = count > 0
		resp.SemanticLoop.LastCheckpointAt = lastAt
	}

	// tool_schema_drift: per-tool call count + per-project threshold.
	// Pull the customer's tool_schema_drift.min_history_calls override
	// from the Theme B per-project thresholds table; fall back to the
	// detector default if no override (or on store error — handled by
	// the loader's existing fallback path).
	tierForReads := TierHobby
	if proj, projErr := h.Store.GetProject(r.Context(), authProjectID); projErr == nil && proj != nil {
		tierForReads = normalizeTier(proj.Tier)
	}
	thresholds := LoadProjectDetectorThresholds(
		r.Context(), h.Store, h.Logger, authProjectID, tierForReads, nil,
	)
	resp.ToolSchemaDrift.MinHistoryCalls = thresholds.ToolSchemaDrift.MinHistoryCalls

	tools, err := h.Store.ListToolCallCountsForProject(r.Context(), authProjectID)
	if err != nil {
		h.Logger.Warn("detector-status: tool call counts failed (returning empty)",
			"project_id", authProjectID, "error", err.Error())
		if resp.Error == "" {
			resp.Error = "partial: tool call counts unavailable"
		} else {
			resp.Error = "partial: checkpoint + tool call counts unavailable"
		}
	} else {
		// Translate store.ToolCallCount → api.ToolCallCount. Loop is
		// O(N) where N = distinct tool count per project (small).
		api := make([]ToolCallCount, 0, len(tools))
		for _, t := range tools {
			api = append(api, ToolCallCount{
				ToolName:  t.ToolName,
				CallCount: t.CallCount,
			})
		}
		resp.ToolSchemaDrift.Tools = api
	}

	writeJSON(w, http.StatusOK, resp)
}
