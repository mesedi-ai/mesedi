package api

// Validators registry for per-project detector thresholds (Theme B.a).
//
// Each tunable knob the audit called out as a hardcoded-threshold
// gap registers a spec here. The spec carries:
//   - Default value used when no per-project override exists
//   - Parser that turns a JSON-encoded request value into the
//     correct Go type
//   - Validator that enforces global bounds AND the per-tier cap
//   - Human-readable description surfaced in the GET response
//
// Adding a future tunable: write a new spec entry, no schema
// change. The store accepts any string; the registry is the
// gatekeeper.
//
// Tier-cap discipline matches backend/internal/api/tier_caps.go:
// unknown / empty tier falls back to the Hobby cap (strictest).
//
// Theme B sequence:
//   B.a (this file) — registry + REST handlers + tier-cap accessor
//   B.b — wire detectors to read per-project values
//   B.c — dashboard editor
//   B.d — telemetry + integration tests + customer docs

import (
	"encoding/json"
	"fmt"
	"strings"
)

// DetectorThresholdSpec describes one tunable knob. Parse turns
// the request's value_json into a typed Go value (int or float64)
// after validating it against global bounds AND the supplied
// tier's per-tier cap.
type DetectorThresholdSpec struct {
	// Detector is the failure-class name the threshold belongs to
	// ("semantic_loop", "token_waste", etc).
	Detector string
	// ThresholdKey is the spec's unique key within its detector
	// ("revisit_threshold", "prefix_window_chars", etc).
	ThresholdKey string
	// ValueType is "int" or "float" — used by the handler to
	// surface the expected JSON shape on validation errors.
	ValueType string
	// Description is a one-line plain-English explanation surfaced
	// in the GET response. Customers (and the dashboard editor in
	// B.c) read it to know what they're tuning.
	Description string
	// Default is the typed value the registry returns when no
	// per-project override exists. MUST be the same value as the
	// detector's existing hardcoded default — backward compat.
	Default any
	// Parse validates valueJSON and returns the typed value or an
	// error. Implementations enforce global bounds + the tier cap.
	Parse func(valueJSON string, tier string) (any, error)
}

// detectorThresholdRegistry is the canonical list of tunable
// thresholds Theme B exposes. Keyed by "<detector>:<threshold_key>"
// so lookups are O(1) and the natural sort order in the dashboard
// editor (B.c) is by detector then by key.
var detectorThresholdRegistry = map[string]*DetectorThresholdSpec{}

func init() {
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "semantic_loop",
		ThresholdKey: "revisit_threshold",
		ValueType:    "int",
		Description: "How many times the same checkpoint state must repeat " +
			"before the detector flags a semantic loop. Default 3 matches " +
			"the loops-family threshold across detectors.",
		Default: 3,
		Parse: func(valueJSON, _ string) (any, error) {
			v, err := parseIntJSON(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundInt(v, 2, 1000, "revisit_threshold"); err != nil {
				return nil, err
			}
			return v, nil
		},
	})
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "token_waste",
		ThresholdKey: "prefix_window_chars",
		ValueType:    "int",
		Description: "Leading characters of each user_message hashed for " +
			"exact-prefix matching. Larger windows reduce false positives on " +
			"agents with short shared headers; smaller windows catch shorter " +
			"accumulation patterns. Default 2048.",
		Default: 2048,
		Parse: func(valueJSON, tier string) (any, error) {
			v, err := parseIntJSON(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundInt(v, 64, 65536, "prefix_window_chars"); err != nil {
				return nil, err
			}
			if err := capInt(v, tierCapTokenWastePrefixWindow(tier),
				"prefix_window_chars", tier); err != nil {
				return nil, err
			}
			return v, nil
		},
	})
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "token_waste",
		ThresholdKey: "min_repeats",
		ValueType:    "int",
		Description: "How many repeats of the same prefix hash trigger the " +
			"detector. Default 3 matches the loops-family threshold.",
		Default: 3,
		Parse: func(valueJSON, _ string) (any, error) {
			v, err := parseIntJSON(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundInt(v, 2, 100, "min_repeats"); err != nil {
				return nil, err
			}
			return v, nil
		},
	})
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "tool_schema_drift",
		ThresholdKey: "min_history_calls",
		ValueType:    "int",
		Description: "How many historical calls a tool needs before the " +
			"detector evaluates schema drift on a new call. Default 10 " +
			"balances quick signal against false-positive priming noise.",
		Default: 10,
		Parse: func(valueJSON, _ string) (any, error) {
			v, err := parseIntJSON(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundInt(v, 2, 1000, "min_history_calls"); err != nil {
				return nil, err
			}
			return v, nil
		},
	})
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "grounding_failure",
		ThresholdKey: "mean_floor",
		ValueType:    "float",
		Description: "Below this rolling mean score, grounding_failure " +
			"fires. Default 0.5 — half the evaluator's pass band.",
		Default: 0.5,
		Parse: func(valueJSON, _ string) (any, error) {
			v, err := parseFloatJSON(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundFloat(v, 0.0, 1.0, "mean_floor"); err != nil {
				return nil, err
			}
			return v, nil
		},
	})
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "drift",
		ThresholdKey: "lexical_threshold_low",
		ValueType:    "float",
		Description: "Lexical-drift cosine distance at which the LOW bucket " +
			"fires. Default 0.45. Must be < medium < high.",
		Default: 0.45,
		Parse: func(valueJSON, _ string) (any, error) {
			v, err := parseFloatJSON(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundFloat(v, 0.0, 1.0, "lexical_threshold_low"); err != nil {
				return nil, err
			}
			return v, nil
		},
	})
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "drift",
		ThresholdKey: "lexical_threshold_medium",
		ValueType:    "float",
		Description: "Lexical-drift cosine distance at which the MEDIUM " +
			"bucket fires. Default 0.55. Must be > low < high.",
		Default: 0.55,
		Parse: func(valueJSON, _ string) (any, error) {
			v, err := parseFloatJSON(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundFloat(v, 0.0, 1.0, "lexical_threshold_medium"); err != nil {
				return nil, err
			}
			return v, nil
		},
	})
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "drift",
		ThresholdKey: "lexical_threshold_high",
		ValueType:    "float",
		Description: "Lexical-drift cosine distance at which the HIGH " +
			"bucket fires. Default 0.70. Must be > medium.",
		Default: 0.70,
		Parse: func(valueJSON, _ string) (any, error) {
			v, err := parseFloatJSON(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundFloat(v, 0.0, 1.0, "lexical_threshold_high"); err != nil {
				return nil, err
			}
			return v, nil
		},
	})
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "context_overflow",
		ThresholdKey: "high_pct",
		ValueType:    "float",
		Description: "Fraction of model context window at which the HIGH " +
			"bucket fires (e.g. 0.90 = 90% utilization). Default 0.90.",
		Default: 0.90,
		Parse: func(valueJSON, _ string) (any, error) {
			v, err := parseFloatJSON(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundFloat(v, 0.5, 1.0, "high_pct"); err != nil {
				return nil, err
			}
			return v, nil
		},
	})
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "context_overflow",
		ThresholdKey: "critical_pct",
		ValueType:    "float",
		Description: "Fraction of model context window at which the " +
			"CRITICAL bucket fires (e.g. 1.0 = the request was rejected " +
			"for context length). Default 1.0.",
		Default: 1.0,
		Parse: func(valueJSON, _ string) (any, error) {
			v, err := parseFloatJSON(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundFloat(v, 0.5, 1.0, "critical_pct"); err != nil {
				return nil, err
			}
			return v, nil
		},
	})
	// ─────────────────────────────────────────────────────────────
	// Loops family — extends the Theme B primitive to a 4th detector
	// family. Closes loops.G2 (step_count), loops.G3 (identical_call_
	// loop), loops.G4 (similar_call_loop). All 4 knobs are pure
	// alerting-sensitivity (no cost asymmetry across tiers — no tier
	// caps). Defaults match the existing hardcoded values exactly so
	// customers who never tune see byte-identical detector behavior.
	// ─────────────────────────────────────────────────────────────
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "loops",
		ThresholdKey: "step_count_threshold",
		ValueType:    "int",
		Description: "Total events per execution above which loops/" +
			"step_count fires. Default 10 is artificially low for early-" +
			"v0 demo visibility; iterative-refinement workflows that " +
			"legitimately emit many events should raise this.",
		Default: 10,
		Parse: func(valueJSON, _ string) (any, error) {
			v, err := parseIntJSON(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundInt(v, 2, 10000, "step_count_threshold"); err != nil {
				return nil, err
			}
			return v, nil
		},
	})
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "loops",
		ThresholdKey: "identical_call_min_repeats",
		ValueType:    "int",
		Description: "How many times the same (model + user_message) " +
			"LLM call must repeat before loops/identical_call_loop fires. " +
			"Default 3 matches the loops-family threshold.",
		Default: 3,
		Parse: func(valueJSON, _ string) (any, error) {
			v, err := parseIntJSON(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundInt(v, 2, 100, "identical_call_min_repeats"); err != nil {
				return nil, err
			}
			return v, nil
		},
	})
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "loops",
		ThresholdKey: "similar_call_distance_threshold",
		ValueType:    "float",
		Description: "Cosine distance below which two LLM-call user " +
			"messages are considered near-duplicates (lower = stricter; " +
			"0.20 distance ≈ 80% similarity). Default 0.20.",
		Default: 0.20,
		Parse: func(valueJSON, _ string) (any, error) {
			v, err := parseFloatJSON(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundFloat(v, 0.05, 0.50, "similar_call_distance_threshold"); err != nil {
				return nil, err
			}
			return v, nil
		},
	})
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "loops",
		ThresholdKey: "similar_call_min_cluster_size",
		ValueType:    "int",
		Description: "How many near-duplicate LLM calls must cluster " +
			"before loops/similar_call_loop fires. Default 3 matches " +
			"the loops-family threshold.",
		Default: 3,
		Parse: func(valueJSON, _ string) (any, error) {
			v, err := parseIntJSON(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundInt(v, 2, 100, "similar_call_min_cluster_size"); err != nil {
				return nil, err
			}
			return v, nil
		},
	})
	// ─────────────────────────────────────────────────────────────
	// Theme B extensions wave — closes grounding_failure.G3 +
	// cascading_failure.G2 + cascading_failure.G3 + hitl_rejection_
	// spike.G3. Introduces two new ValueTypes ('bool' and 'json')
	// to the validators registry. After this wave, the Theme B
	// primitive supports the full {int, float, bool, json} value
	// space — any future detector with structured config can ride
	// it without inventing new storage.
	// ─────────────────────────────────────────────────────────────
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "grounding_failure",
		ThresholdKey: "per_evaluator_floors",
		ValueType:    "json",
		Description: "Per-evaluator floor overrides keyed " +
			"\"evaluator_id:metric_type\" → float in [0, 1]. " +
			"Unspecified evaluator:metric pairs fall back to the " +
			"global mean_floor (default 0.5). Max 50 entries.",
		Default: map[string]float64{},
		Parse: func(valueJSON, _ string) (any, error) {
			m, err := parseJSONMap(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundJSONMap(m, 50, 0.0, 1.0, "per_evaluator_floors"); err != nil {
				return nil, err
			}
			return m, nil
		},
	})
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "cascading_failure",
		ThresholdKey: "cascade_window_seconds",
		ValueType:    "int",
		Description: "Maximum seconds between handoff_emitted_at and " +
			"child_ended_at for a cascading failure to count. Default " +
			"86400 (24h) preserves the historical 'no-window' behavior; " +
			"customers can tighten to e.g. 300 (5min) to avoid grouping " +
			"long-lived spawn handoffs whose children fail hours later.",
		Default: 86400,
		Parse: func(valueJSON, _ string) (any, error) {
			v, err := parseIntJSON(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundInt(v, 10, 86400, "cascade_window_seconds"); err != nil {
				return nil, err
			}
			return v, nil
		},
	})
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "cascading_failure",
		ThresholdKey: "exclude_spawn_handoffs",
		ValueType:    "bool",
		Description: "When true, skip rows where handoff_kind = 'spawn' " +
			"before scoring. Spawn handoffs are fire-and-forget; a " +
			"parent that succeeded while a spawn child failed later is " +
			"arguably a supervision gap, not a cascade. Default false " +
			"preserves the historical behavior.",
		Default: false,
		Parse: func(valueJSON, _ string) (any, error) {
			v, err := parseBoolJSON(valueJSON)
			if err != nil {
				return nil, err
			}
			return v, nil
		},
	})
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "hitl_rejection_spike",
		ThresholdKey: "measurement_window_minutes",
		ValueType:    "int",
		Description: "Recency window (minutes) over which the detector " +
			"aggregates HITL outcomes. Default 60 (1h) matches the " +
			"existing posture; tighten for high-volume projects, widen " +
			"for low-volume projects with sparse HITL signal.",
		Default: 60,
		Parse: func(valueJSON, _ string) (any, error) {
			v, err := parseIntJSON(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundInt(v, 5, 1440, "measurement_window_minutes"); err != nil {
				return nil, err
			}
			return v, nil
		},
	})
}

// registerThresholdSpec adds a spec to the registry. Panics on
// duplicate (detector, threshold_key) — caught at startup.
func registerThresholdSpec(s *DetectorThresholdSpec) {
	k := s.Detector + ":" + s.ThresholdKey
	if _, exists := detectorThresholdRegistry[k]; exists {
		panic("duplicate detector threshold spec: " + k)
	}
	detectorThresholdRegistry[k] = s
}

// LookupDetectorThresholdSpec returns the spec for the given
// (detector, threshold_key) pair, or (nil, false) when no such
// spec is registered. Callers that need to validate a request body
// against the registry use this gateway.
func LookupDetectorThresholdSpec(detector, thresholdKey string) (*DetectorThresholdSpec, bool) {
	s, ok := detectorThresholdRegistry[detector+":"+thresholdKey]
	return s, ok
}

// ListDetectorThresholdSpecs returns every spec registered for the
// given detector, sorted by threshold_key for deterministic output.
// When detector is empty, returns ALL specs across all detectors,
// sorted by "<detector>:<threshold_key>". Used by the GET /me/
// detector-thresholds/{detector} handler.
func ListDetectorThresholdSpecs(detector string) []*DetectorThresholdSpec {
	out := make([]*DetectorThresholdSpec, 0, len(detectorThresholdRegistry))
	for _, spec := range detectorThresholdRegistry {
		if detector != "" && spec.Detector != detector {
			continue
		}
		out = append(out, spec)
	}
	// Stable sort: by detector, then by threshold_key.
	sortThresholdSpecsForOutput(out)
	return out
}

// sortThresholdSpecsForOutput orders specs deterministically. Pure
// stdlib so the package has no extra deps.
func sortThresholdSpecsForOutput(specs []*DetectorThresholdSpec) {
	for i := 1; i < len(specs); i++ {
		for j := i; j > 0; j-- {
			a, b := specs[j-1], specs[j]
			if a.Detector < b.Detector {
				break
			}
			if a.Detector == b.Detector && a.ThresholdKey < b.ThresholdKey {
				break
			}
			specs[j-1], specs[j] = specs[j], specs[j-1]
		}
	}
}

// ─────────────────────────────────────────────────────────────────
// Parse + bound helpers used by the per-spec Parse functions.
// ─────────────────────────────────────────────────────────────────

func parseIntJSON(valueJSON string) (int, error) {
	valueJSON = strings.TrimSpace(valueJSON)
	if valueJSON == "" {
		return 0, fmt.Errorf("empty value")
	}
	var n int
	if err := json.Unmarshal([]byte(valueJSON), &n); err != nil {
		return 0, fmt.Errorf("expected int, got %q: %w", valueJSON, err)
	}
	return n, nil
}

func parseFloatJSON(valueJSON string) (float64, error) {
	valueJSON = strings.TrimSpace(valueJSON)
	if valueJSON == "" {
		return 0, fmt.Errorf("empty value")
	}
	var f float64
	if err := json.Unmarshal([]byte(valueJSON), &f); err != nil {
		return 0, fmt.Errorf("expected float, got %q: %w", valueJSON, err)
	}
	return f, nil
}

func boundInt(v, lo, hi int, name string) error {
	if v < lo || v > hi {
		return fmt.Errorf("%s out of range [%d, %d]: %d", name, lo, hi, v)
	}
	return nil
}

func boundFloat(v, lo, hi float64, name string) error {
	if v < lo || v > hi {
		return fmt.Errorf("%s out of range [%g, %g]: %g", name, lo, hi, v)
	}
	return nil
}

func capInt(v, cap int, name, tier string) error {
	if v > cap {
		return fmt.Errorf("%s exceeds %s tier cap of %d: %d",
			name, normalizeTier(tier), cap, v)
	}
	return nil
}

func capFloat(v, cap float64, name, tier string) error {
	if v > cap {
		return fmt.Errorf("%s exceeds %s tier cap of %g: %g",
			name, normalizeTier(tier), cap, v)
	}
	return nil
}

// parseBoolJSON unmarshals a JSON-encoded boolean. Used by Theme B
// extensions wave 'bool' ValueType for simple per-project toggle
// knobs (e.g. cascading_failure.exclude_spawn_handoffs).
func parseBoolJSON(valueJSON string) (bool, error) {
	valueJSON = strings.TrimSpace(valueJSON)
	if valueJSON == "" {
		return false, fmt.Errorf("empty value")
	}
	var b bool
	if err := json.Unmarshal([]byte(valueJSON), &b); err != nil {
		return false, fmt.Errorf("expected bool, got %q: %w", valueJSON, err)
	}
	return b, nil
}

// parseJSONMap unmarshals a JSON-encoded object whose values are
// numeric (float64-coerced). Used by Theme B extensions wave 'json'
// ValueType for structured per-project config (e.g.
// grounding_failure.per_evaluator_floors).
func parseJSONMap(valueJSON string) (map[string]float64, error) {
	valueJSON = strings.TrimSpace(valueJSON)
	if valueJSON == "" {
		return nil, fmt.Errorf("empty value")
	}
	var m map[string]float64
	if err := json.Unmarshal([]byte(valueJSON), &m); err != nil {
		return nil, fmt.Errorf("expected object {string: number}, got %q: %w", valueJSON, err)
	}
	if m == nil {
		m = map[string]float64{}
	}
	return m, nil
}

// boundJSONMap enforces structural bounds on a parsed map: max key
// count + per-value range. Designed for the per_evaluator_floors
// shape (50-key cap, value range [0, 1]) but generic enough that
// future json-ValueType specs can reuse it. Returns the first
// violation encountered (deterministic by Go map iteration order
// would be non-deterministic, so we collect violators if any).
func boundJSONMap(m map[string]float64, maxKeys int, lo, hi float64, name string) error {
	if len(m) > maxKeys {
		return fmt.Errorf("%s map has %d entries; max is %d",
			name, len(m), maxKeys)
	}
	for k, v := range m {
		if v < lo || v > hi {
			return fmt.Errorf("%s[%s]=%g out of range [%g, %g]",
				name, k, v, lo, hi)
		}
	}
	return nil
}
