// Provider-incident and HITL detectors, carved out of
// HandleUpdateExecution on 2026-09-10 (the split's third carve).
// Bodies moved by exact line range with exactly two substitutions,
// both reads of patch.Status becoming the patchStatus parameter;
// every other identifier kept its name so the diff against the
// parent is the enclosing function and nothing else.
package api

import (
	"encoding/json"
	"net/http"
	"time"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/detectors"
	"mesedi/backend/internal/store"
)

// runProviderIncidentAndHITLDetectors runs the cross-tenant
// provider-outage detector and both HITL SLA detectors for one
// terminal execution.
func (h *Handlers) runProviderIncidentAndHITLDetectors(r *http.Request, executionID, authProjectID string, patchStatus events.ExecutionStatus, detectorThresholds ProjectDetectorThresholds) {
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
	if isTerminalStatus(patchStatus) {
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
			// One indexed count per distinct (provider, error_class)
			// pair observed in THIS execution's events. Deliberately
			// per-pair, examined and kept 2026-09-10: cardinality is
			// bounded by the pairs one execution can produce (one to
			// three in practice, a dozen at the theoretical ceiling),
			// not by data volume, so this is not the N+1 shape where
			// a loop scales with rows. A batched store method would
			// add interface surface on both stores to save two
			// indexed counts. The named helper keeps the intent
			// legible at the call site.
			countTenantsWith := func(provider, errClass string) (int, error) {
				return h.Store.CountDistinctTenantsWithProviderError(
					r.Context(), authProjectID, provider, errClass, since,
				)
			}
			for pair := range seen {
				provider, errClass := pair[0], pair[1]
				count, cErr := countTenantsWith(provider, errClass)
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
	if isTerminalStatus(patchStatus) {
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
}
