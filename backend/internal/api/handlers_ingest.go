// Execution creation and event ingest.
// Split out of handlers.go on 2026-09-12; every declaration moved verbatim.
package api

import (
	"fmt"
	"net/http"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
)

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
	// body provided one and it doesn't match, reject, this catches
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

	// Stamp the authenticated API key onto the execution. This
	// is the only place the column is set; the SDK never supplies it
	// (the api_key_id field on the wire is server-stamped and any
	// body-supplied value is overwritten here). Without this, the
	// Terms commitment to "share the information we have about the
	// key's recent use" on a compromise report cannot be honored
	// per-key, only per-project.
	if authKeyID, hasKey := APIKeyIDFromContext(r.Context()); hasKey && authKeyID != "" {
		k := authKeyID
		exec.APIKeyID = &k
	}

	if exec.Status == "" {
		exec.Status = events.StatusStarted
	}
	if exec.StartedAt.IsZero() {
		exec.StartedAt = time.Now().UTC()
	}

	// Billing cap enforcement (Mesedi pricing alignment).
	//
	// Before persisting the execution, check whether the project's
	// per-period paid-overage cost has already crossed billing_cap_usd.
	// When it has, silent-drop the execution with 402 + a structured
	// "billing cap reached" message so the SDK can surface it without
	// retrying. Enterprise tier is exempt (no per-execution overage).
	//
	// The cap check uses the current row, so a request arriving while
	// the meter is exactly at the cap will be accepted; the *next*
	// request after the increment is the one that gets blocked. That's
	// the desired behavior: the customer never pays past the cap,
	// modulo a single-execution slop on the boundary.
	if proj, err := h.Store.GetProject(r.Context(), authProjectID); err == nil && proj != nil {
		if blocked, capUSD, costUSD := capExceeded(proj); blocked {
			h.Logger.Info("ingest blocked: billing cap reached",
				"project_id", authProjectID,
				"tier", proj.Tier,
				"cost_usd", costUSD,
				"cap_usd", capUSD,
			)
			writeError(w, http.StatusPaymentRequired,
				fmt.Sprintf("billing cap reached ($%.2f of $%.2f). New executions are paused until the next billing period or until the cap is raised.", costUSD, capUSD))
			return
		}
	}

	if err := h.Store.CreateExecution(r.Context(), &exec); err != nil {
		h.Logger.Error("create execution failed",
			"execution_id", exec.ExecutionID,
			"error", err.Error(),
		)
		writeError(w, http.StatusInternalServerError, "persist failed: "+err.Error())
		return
	}

	// , increment the per-period execution counter. Best-effort:
	// a failure here logs a warning but does not propagate to the
	// caller. Counting must never block ingest. Enforcement (Hobby
	// silent-drop, Pro overage metering) lands in a follow-up slice
	// that gates on this counter before the CreateExecution call.
	if err := h.Store.IncrementExecutionsThisPeriod(r.Context(), exec.ProjectID); err != nil {
		h.Logger.Warn("increment executions counter failed (continuing)",
			"project_id", exec.ProjectID,
			"error", err.Error(),
		)
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

// MaxEventPayloadBytes caps the JSON-serialized byte count of a
// single event's Payload field at the ingest boundary.
// Defense-in-depth against SDKs that did not (or could not) apply
// the same cap client-side: pre-v0.4 SDKs, direct curl/custom HTTP
// integrations, and the rare case of a customer deliberately
// disabling SDK-side truncation. Matches the SDK default
// `DEFAULT_MAX_PAYLOAD_BYTES`; the SDK should always reach this
// before the backend does, but the backend still enforces so a
// single oversized event from any source cannot bloat the
// executions / events tables.
//
// Per-event, not per-batch: a legitimate 100-event batch with
// 32 KB payloads each totals ~3.2 MB on the wire, which is fine.
const MaxEventPayloadBytes = 32 * 1024

// payloadOverCap returns true iff the event's serialized payload
// exceeds the backend's hard cap. Extracted as a tiny named helper so
// the unit test can drive it without standing up the full ingest
// handler.
func payloadOverCap(evt *events.Event) bool {
	return len(evt.Payload) > MaxEventPayloadBytes
}

// HandleIngestEvents accepts a batch of Events. Batching is required ,
// the SDK buffers events client-side and flushes in groups of ~100, so
// the ingest path is array-shaped from day one. A single-event POST is
// accepted as a 1-element array; rejecting non-array bodies catches
// SDK bugs early.
func (h *Handlers) HandleIngestEvents(w http.ResponseWriter, r *http.Request) {
	//  the DLP layer now consumes per-project custom
	// patterns, so the ingest path needs the authenticated
	// project_id at the top. Existing event-level project_id checks
	// downstream still apply.
	authProjectID, ok := ProjectIDFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "no project context")
		return
	}
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
	accepted, rejected := validateIngestBatch(batch, h.Logger)

	// +  scan + redact outbound LLM /
	// tool payloads against the DLP rule registry (built-ins) plus
	// the project's custom data_leakage patterns. Matched secrets
	// are replaced with `[REDACTED:rule_id]` tokens; high / critical
	// hits also generate a sibling dlp_scan_result event that the
	// data_leakage detector consumes downstream. Nil scanner (local
	// dev) leaves the batch unchanged.
	var customMatched []string
	accepted, customMatched = h.applyDLPToBatch(r.Context(), authProjectID, accepted)
	//  increment match_count once per matched custom
	// pattern_id (de-dup; a single pattern firing 10 times in one
	// batch should bump the counter once, not ten times, the
	// dashboard "this rule is doing work" signal is more useful as
	// "fired on N executions" than "matched N times across executions").
	for _, pid := range uniqueStrings(customMatched) {
		h.incrementCustomPatternMatch(r.Context(), authProjectID, pid)
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
