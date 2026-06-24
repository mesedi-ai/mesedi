package api

// Per-project detector-threshold loader (Theme B.b).
//
// Reads every override row for the (projectID, detector) pairs the
// six in-scope Theme B detectors care about, parses each via the
// validators registry's per-spec Parse function, and assembles a
// ProjectDetectorThresholds aggregate the handler passes to each
// detector's *WithThresholds variant at execution-close time.
//
// Hot-path discipline:
//   - O(detectors) store reads per execution-close (6 indexed
//     SELECTs). Matches Wave 2.1.b's CustomPattern bulk-load shape.
//   - Returns all-defaults on store error rather than failing the
//     execution close. Logged via the caller's slog.
//   - Per-knob parse errors fall back to that knob's default with a
//     warning log; the rest of the detector's thresholds are
//     unaffected.
//
// Tier-cap is enforced at the API write path (B.a). The loader
// trusts stored values to be already-validated against the
// project's tier at the time they were written.

import (
	"context"
	"log/slog"

	"mesedi/backend/internal/detectors"
	"mesedi/backend/internal/store"
)

// DetectorThresholdAuditWriter is the minimal callback the loader
// uses to promote fallback warnings to durable audit_events rows
// (Theme B.d). Same shape as the `config_fallback` action shipped
// by #276.d for the existing per-project primitives. Detector +
// threshold_key + reason name the fallback; metadata carries the
// underlying error string when applicable. The handler call site
// wires this to `recordAuditEventForProject` with action
// "config_fallback" and target_type "detector_thresholds" so the
// dashboard tile that aggregates config-fallback events picks
// these up automatically.
//
// Passing nil makes the loader skip audit writes (used by tests
// and any future caller that doesn't want durable telemetry).
type DetectorThresholdAuditWriter func(
	detector, thresholdKey, reason string,
	metadata map[string]any,
)

// ProjectDetectorThresholds bundles every detector's typed Thresholds
// struct for one project, so handlers load once and pass each detector
// its own slice.
type ProjectDetectorThresholds struct {
	SemanticLoop     detectors.SemanticLoopThresholds
	TokenWaste       detectors.TokenWasteThresholds
	ToolSchemaDrift  detectors.ToolSchemaDriftThresholds
	GroundingFailure detectors.GroundingFailureThresholds
	Drift            detectors.DriftThresholds
	ContextOverflow  detectors.ContextOverflowThresholds
	// Loops bundles the 4 loops-family thresholds extended via the
	// loops-thresholds wave (loops.G2/G3/G4): step_count_threshold,
	// identical_call_min_repeats, similar_call distance + min cluster
	// size. step_count and identical_call_loop consume their values
	// directly at the handler call site (literal comparisons);
	// similar_call_loop consumes via DetectSimilarCallLoopWithThresholds.
	Loops detectors.LoopsThresholds
}

// DefaultProjectDetectorThresholds returns the all-defaults aggregate
// — used as the fallback when the store read fails or before the
// loader has run. Every Default<Name>Thresholds() function matches
// the detector's historical hardcoded values.
func DefaultProjectDetectorThresholds() ProjectDetectorThresholds {
	return ProjectDetectorThresholds{
		SemanticLoop:     detectors.DefaultSemanticLoopThresholds(),
		TokenWaste:       detectors.DefaultTokenWasteThresholds(),
		ToolSchemaDrift:  detectors.DefaultToolSchemaDriftThresholds(),
		GroundingFailure: detectors.DefaultGroundingFailureThresholds(),
		Drift:            detectors.DefaultDriftThresholds(),
		ContextOverflow:  detectors.DefaultContextOverflowThresholds(),
		Loops:            detectors.DefaultLoopsThresholds(),
	}
}

// LoadProjectDetectorThresholds bulk-reads every override row for
// projectID across the six in-scope Theme B detectors and returns
// the typed aggregate. The "tier" param is for parse-time tier-cap
// re-validation — stored values were validated against the project's
// tier when written, but we re-validate on read to defend against
// tier downgrade (Hobby customer was on Team when they set the
// override; downgraded to Hobby later; their override now exceeds
// their tier cap and must be clamped to default).
//
// Returns the all-defaults aggregate on any store error. Logs the
// failure via the supplied logger so operators see the fallback
// rather than silently losing customer config.
func LoadProjectDetectorThresholds(
	ctx context.Context,
	st store.Store,
	logger *slog.Logger,
	projectID string,
	tier string,
	auditWriter DetectorThresholdAuditWriter,
) ProjectDetectorThresholds {
	out := DefaultProjectDetectorThresholds()

	for _, detector := range []string{
		"semantic_loop",
		"token_waste",
		"tool_schema_drift",
		"grounding_failure",
		"drift",
		"context_overflow",
		"loops",
	} {
		rows, err := st.ListProjectDetectorThresholds(ctx, projectID, detector)
		if err != nil {
			logger.Warn("detector_thresholds load failed; using defaults",
				"project_id", projectID,
				"detector", detector,
				"error", err.Error())
			if auditWriter != nil {
				auditWriter(detector, "*", "store_error",
					map[string]any{"error": err.Error()})
			}
			continue
		}
		for _, row := range rows {
			spec, ok := LookupDetectorThresholdSpec(detector, row.ThresholdKey)
			if !ok {
				// Orphaned row (detector removed its key, or schema
				// drift). Skip silently — don't surface a warning per
				// execution close.
				continue
			}
			parsed, perr := spec.Parse(row.ValueJSON, tier)
			if perr != nil {
				logger.Warn("detector_threshold parse failed; using default for this key",
					"project_id", projectID,
					"detector", detector,
					"threshold_key", row.ThresholdKey,
					"error", perr.Error())
				if auditWriter != nil {
					auditWriter(detector, row.ThresholdKey, "parse_error",
						map[string]any{
							"error":          perr.Error(),
							"stored_value":   row.ValueJSON,
							"fallback_value": spec.Default,
						})
				}
				continue
			}
			applyDetectorThresholdValue(&out, detector, row.ThresholdKey, parsed)
		}
	}
	return out
}

// applyDetectorThresholdValue writes a parsed override into the
// correct field of the aggregate. The switch is explicit per
// (detector, threshold_key) so the compiler enforces full coverage:
// a future tunable added to the registry must also be wired here, or
// the loader silently drops it (which we catch via the Theme B.d
// integration tests).
func applyDetectorThresholdValue(
	out *ProjectDetectorThresholds,
	detector, key string,
	value any,
) {
	switch detector {
	case "semantic_loop":
		if key == "revisit_threshold" {
			if v, ok := value.(int); ok {
				out.SemanticLoop.RevisitThreshold = v
			}
		}
	case "token_waste":
		switch key {
		case "prefix_window_chars":
			if v, ok := value.(int); ok {
				out.TokenWaste.PrefixWindowChars = v
			}
		case "min_repeats":
			if v, ok := value.(int); ok {
				out.TokenWaste.MinRepeats = v
			}
		}
	case "tool_schema_drift":
		if key == "min_history_calls" {
			if v, ok := value.(int); ok {
				out.ToolSchemaDrift.MinHistoryCalls = v
			}
		}
	case "grounding_failure":
		if key == "mean_floor" {
			if v, ok := value.(float64); ok {
				out.GroundingFailure.MeanFloor = v
			}
		}
	case "drift":
		switch key {
		case "lexical_threshold_low":
			if v, ok := value.(float64); ok {
				out.Drift.LexicalLow = v
			}
		case "lexical_threshold_medium":
			if v, ok := value.(float64); ok {
				out.Drift.LexicalMedium = v
			}
		case "lexical_threshold_high":
			if v, ok := value.(float64); ok {
				out.Drift.LexicalHigh = v
			}
		}
	case "context_overflow":
		switch key {
		case "high_pct":
			if v, ok := value.(float64); ok {
				out.ContextOverflow.HighPct = v
			}
		case "critical_pct":
			if v, ok := value.(float64); ok {
				out.ContextOverflow.CriticalPct = v
			}
		}
	case "loops":
		switch key {
		case "step_count_threshold":
			if v, ok := value.(int); ok {
				out.Loops.StepCountThreshold = v
			}
		case "identical_call_min_repeats":
			if v, ok := value.(int); ok {
				out.Loops.IdenticalCallMinRepeats = v
			}
		case "similar_call_distance_threshold":
			if v, ok := value.(float64); ok {
				out.Loops.SimilarCallDistanceThreshold = v
			}
		case "similar_call_min_cluster_size":
			if v, ok := value.(int); ok {
				out.Loops.SimilarCallMinClusterSize = v
			}
		}
	}
}
