// Tool-schema-drift detection, carved out of HandleUpdateExecution
// on 2026-09-09 (the split's second carve) so the shape-change
// ranking could land in a named home instead of the interior of a
// 1,400-line function. The body moved by exact line range with three
// disclosed substitutions: patch.Status and two reads of
// detectorThresholds.ToolSchemaDrift became the patchStatus and
// driftThresholds parameters.
package api

import (
	"context"
	"encoding/json"
	"net/http"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/detectors"
	"mesedi/backend/internal/store"
)

// runToolSchemaDriftDetector compares each invoked tool's latest
// return shape against the project's historical majority, checking
// description drift first (the tool-poisoning class outranks a
// version bump). At most one drift signal fires per execution.
func (h *Handlers) runToolSchemaDriftDetector(r *http.Request, executionID, authProjectID string, patchStatus events.ExecutionStatus, driftThresholds detectors.ToolSchemaDriftThresholds) {
	//, tool_schema_drift detector. For each tool the
	// execution invoked successfully, compare the LAST successful
	// return_value's shape on this execution against the project's
	// historical roll-up for the same tool. Fires when a previously-
	// stable tool returns a new shape, catching silent third-party
	// API version bumps.
	//
	// Runs AFTER semantic_loop because:
	//   - loop-class errors are about the AGENT's behavior; schema
	//     drift is about the WORLD's behavior; resolving the loop
	//     first lets SREs see the simpler root cause if both exist.
	//   - if multiple drift signals fire across multiple tools, only
	//     the first-found one claims the execution (deterministic
	//     iteration order via ListToolNamesInExecution's distinct
	//     scan).
	if isTerminalStatus(patchStatus) {
		toolNames, err := h.Store.ListToolNamesInExecution(r.Context(), executionID)
		if err != nil {
			h.Logger.Warn("list tool names for schema-drift failed",
				"execution_id", executionID,
				"error", err.Error(),
			)
		} else {
			// : per-project cap on return_value bytes used
			// for fingerprinting. Returns above this threshold are
			// excluded from the comparison (treated as inconclusive,
			// mirroring the SDK's "<truncated>" sentinel). Default
			// 8192 on store error so a transient DB blip can't
			// silence the detector entirely.
			// : cascading resolver walks project → org default →
			// hardcoded constant, emitting a config_fallback
			// system_event internally on store-layer errors.
			maxBytes, _ := h.ResolveToolReturnValueMaxBytes(r.Context(), authProjectID)
			for _, toolName := range toolNames {
				// Current-execution shape: query the most recent
				// successful tool_call for this tool on THIS
				// execution. The detector compares it against the
				// project's historical roll-up.
				currentReturns, err := h.Store.ListSuccessfulToolReturns(r.Context(), authProjectID, toolName, "", 1)
				if err != nil || len(currentReturns) == 0 {
					// "" excludes nothing, gets the most recent
					// project-wide which includes the current
					// execution. If it returns nothing, skip.
					continue
				}
				if len(currentReturns[0]) > maxBytes {
					// Exceeds the per-project cap; treat as
					// non-comparable rather than hashing a partial
					// or oversized structure.
					continue
				}
				currentShape := detectors.ReturnShapeHash(json.RawMessage(currentReturns[0]))
				if currentShape == "" {
					continue
				}
				history, err := h.Store.ListSuccessfulToolReturns(r.Context(), authProjectID, toolName, executionID, 100)
				if err != nil {
					h.Logger.Warn("list tool-return history failed",
						"execution_id", executionID,
						"tool_name", toolName,
						"error", err.Error(),
					)
					continue
				}
				shapeCounts := map[string]int{}
				for _, raw := range history {
					if len(raw) > maxBytes {
						// Same cap for history rows, keeps
						// fingerprint computation symmetric so a
						// customer raising/lowering the cap sees
						// consistent comparisons.
						continue
					}
					shape := detectors.ReturnShapeHash(json.RawMessage(raw))
					if shape == "" {
						continue
					}
					shapeCounts[shape]++
				}
				// Description drift, checked BEFORE return-shape
				// drift. A rewritten description is the signal that a
				// tool the model trusts was tampered with (the MCP
				// tool-poisoning class, CVE-2026-75130), whereas a
				// changed return shape is usually its author shipping
				// a release. When both moved, the security reading is
				// the one worth surfacing, and the `break` below means
				// only one drift signal fires per execution.
				if descSig, descFired := h.detectToolDescriptionDrift(
					r.Context(), authProjectID, executionID, toolName,
					driftThresholds,
				); descFired {
					isNew, gErr := h.Store.GroupToolSchemaDrift(r.Context(), executionID, authProjectID, descSig)
					if gErr != nil {
						h.Logger.Warn("tool-description-drift grouping failed (continuing)",
							"execution_id", executionID,
							"signature", descSig,
							"error", gErr.Error(),
						)
					}
					h.maybeFireWebhook(r, authProjectID, store.FailureClassToolSchemaDrift, descSig, isNew, gErr)
					break
				}

				// Definition drift, checked BETWEEN description and
				// return-shape: a changed declared schema is a
				// contract change (the mcp-pin class), more specific
				// than a changed return shape and less alarming than
				// a rewritten description. Same one-signal-per-
				// execution rule via the break.
				if defSig, defFired := h.detectToolDefinitionDrift(
					r.Context(), authProjectID, executionID, toolName,
					driftThresholds,
				); defFired {
					isNew, gErr := h.Store.GroupToolSchemaDrift(r.Context(), executionID, authProjectID, defSig)
					if gErr != nil {
						h.Logger.Warn("tool-definition-drift grouping failed (continuing)",
							"execution_id", executionID,
							"signature", defSig,
							"error", gErr.Error(),
						)
					}
					h.maybeFireWebhook(r, authProjectID, store.FailureClassToolSchemaDrift, defSig, isNew, gErr)
					break
				}

				if sig, fired := detectors.DetectSchemaDriftWithThresholds(toolName, currentShape, shapeCounts, driftThresholds); fired {
					// Rank the drift before emitting anything. A
					// removed or retyped field is not the same event
					// as an added one; the kind rides the signature so
					// breaking and compatible drift form separate
					// groups an operator can route differently, and
					// the field-level reasons go to a system event so
					// nobody has to diff two JSON blobs by hand.
					sig = sig + ":" + string(h.rankShapeChange(
						r.Context(), authProjectID, executionID, toolName,
						currentReturns[0], history, shapeCounts, maxBytes,
					))
					isNew, gErr := h.Store.GroupToolSchemaDrift(r.Context(), executionID, authProjectID, sig)
					if gErr != nil {
						h.Logger.Warn("tool-schema-drift grouping failed (continuing)",
							"execution_id", executionID,
							"signature", sig,
							"error", gErr.Error(),
						)
					}
					h.maybeFireWebhook(r, authProjectID, store.FailureClassToolSchemaDrift, sig, isNew, gErr)
					break // one drift signal per execution is enough
				}
			}
		}
	}
}

// rankShapeChange classifies the fired drift against the same
// majority baseline the detector used, returning the kind that is
// appended to the group signature. The baseline's structural shape is
// recovered by rehashing history rows until one matches the dominant
// hash; the classifier needs the shape string itself, which the hash
// deliberately discards.
//
// Reasons (dotted-path diffs like "result.price: number -> string")
// are recorded as a system event, capped at ten, and logged. Failure
// to classify is not failure to detect: an unrecoverable baseline
// ranks as indeterminate rather than silencing the signal.
func (h *Handlers) rankShapeChange(
	ctx context.Context,
	authProjectID, executionID, toolName string,
	currentRaw []byte,
	history [][]byte,
	shapeCounts map[string]int,
	maxBytes int,
) detectors.ShapeChangeKind {
	dominantHash, _, _ := detectors.DominantShape(shapeCounts)
	baselineShape := ""
	for _, raw := range history {
		if len(raw) > maxBytes {
			continue
		}
		if detectors.ReturnShapeHash(json.RawMessage(raw)) == dominantHash {
			baselineShape = detectors.ReturnShape(json.RawMessage(raw))
			break
		}
	}
	change := detectors.ClassifyShapeChange(baselineShape, detectors.ReturnShape(json.RawMessage(currentRaw)))

	reasons := change.Reasons
	if len(reasons) > 10 {
		reasons = reasons[:10]
	}
	h.Logger.Info("tool-schema-drift ranked",
		"execution_id", executionID,
		"tool_name", toolName,
		"kind", string(change.Kind),
		"reasons", reasons,
	)
	h.recordSystemEventForProject(
		ctx,
		authProjectID, "tool_schema_drift",
		"shape_change_ranked", "tool", toolName,
		map[string]any{
			"execution_id": executionID,
			"kind":         string(change.Kind),
			"reasons":      reasons,
		},
	)
	return change.Kind
}
