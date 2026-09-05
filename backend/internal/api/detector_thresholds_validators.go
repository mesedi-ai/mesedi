package api

// Validators registry for per-project detector thresholds.
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
// sequence:
//   B.a (this file), registry + REST handlers + tier-cap accessor
//   B.b, wire detectors to read per-project values
//   B.c, dashboard editor
//   B.d, telemetry + integration tests + customer docs

import (
	"encoding/json"
	"fmt"
	"strings"

	"mesedi/backend/internal/pricing"
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
	// ValueType is "int" or "float", used by the handler to
	// surface the expected JSON shape on validation errors.
	ValueType string
	// Description is a one-line plain-English explanation surfaced
	// in the GET response. Customers (and the dashboard editor in
	// B.c) read it to know what they're tuning.
	Description string
	// Default is the typed value the registry returns when no
	// per-project override exists. MUST be the same value as the
	// detector's existing hardcoded default, backward compat.
	Default any
	// Parse validates valueJSON and returns the typed value or an
	// error. Implementations enforce global bounds + the tier cap.
	Parse func(valueJSON string, tier string) (any, error)
}

// detectorThresholdRegistry is the canonical list of tunable
// thresholds exposes. Keyed by "<detector>:<threshold_key>"
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
		Description: "Below this mean score across the execution's " +
			"higher_is_better=true eval_score events, grounding_failure " +
			"fires. Default 0.5: half the evaluator's pass band.",
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
	// Loops family, extends the primitive to a 4th detector
	// family. Closes loops.G2 (step_count), loops.G3 (identical_call_
	// loop), loops.G4 (similar_call_loop). All 4 knobs are pure
	// alerting-sensitivity (no cost asymmetry across tiers, no tier
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
	// extensions wave, closes grounding_failure.G3 +
	// cascading_failure.G2 + cascading_failure.G3 + hitl_rejection_
	// spike.G3. Introduces two new ValueTypes ('bool' and 'json')
	// to the validators registry. After this wave, the
	// primitive supports the full {int, float, bool, json} value
	// space, any future detector with structured config can ride
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
	// hitl_timeout.G4, per-project fire-mode toggle. Closed set
	// ["explicit", "sla_exceeded"]; default both fire (matches the
	// historical hardcoded behavior). Customers can opt out of
	// either mode when their stack pages explicit timeouts via a
	// different channel or when SLA breaches are too noisy.
	// Second consumer of parseJSONStringSlice + boundStringSliceSubset
	// after data_leakage.G5.
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "hitl_timeout",
		ThresholdKey: "fire_modes",
		ValueType:    "json",
		Description: "HITL timeout firing modes the detector " +
			"promotes to failure_groups. Closed set " +
			"[\"explicit\", \"sla_exceeded\"]. Default both fire: " +
			"matches the historical hardcoded posture. Restrict to " +
			"[\"explicit\"] to mute SLA-exceeded clusters (e.g. " +
			"projects where SLA tracking lives outside Mesedi) or " +
			"[\"sla_exceeded\"] to mute the explicit-timeout " +
			"cluster (rare: usually treated as control flow).",
		Default: []string{"explicit", "sla_exceeded"},
		Parse: func(valueJSON, _ string) (any, error) {
			v, err := parseJSONStringSlice(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundStringSliceSubset(v,
				[]string{"explicit", "sla_exceeded"},
				"fire_modes"); err != nil {
				return nil, err
			}
			return v, nil
		},
	})
	// context_overflow.G3, per-project custom model windows. Unlocks
	// detection coverage for Ollama / fine-tuned models that aren't
	// in the static models.ContextWindow registry. Customer overrides
	// win even for KNOWN models (the customer knows their effective
	// window, e.g. behind an upstream proxy with a tighter limit ,
	// better than the registry). NO tier cap, observability of
	// self-hosted models is a Hobby-tier feature.
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "context_overflow",
		ThresholdKey: "custom_model_windows",
		ValueType:    "json",
		Description: "Per-model context window overrides keyed by " +
			"model_id → window_tokens. Wins over the static model " +
			"registry. Use to unlock context_overflow detection for " +
			"Ollama or fine-tuned models, or to enforce a tighter " +
			"window for known models behind a proxy. " +
			"Default empty map; per-entry bounds [1024, 10_000_000] " +
			"tokens; max 50 entries; model_id ≤ 200 chars without " +
			"colon or whitespace (signature stability).",
		Default: map[string]int{},
		Parse: func(valueJSON, _ string) (any, error) {
			m, err := parseJSONIntMap(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundJSONIntMap(m, 50, 1024, 10_000_000, "custom_model_windows"); err != nil {
				return nil, err
			}
			return m, nil
		},
	})
	//, per-project custom_model_pricing override.
	// Slots into the project_detector_thresholds bag under the
	// "pricing" detector key. Wins over the canonical priceTable for
	// the exact model name; per-prefix family entries in the
	// priceTable remain the default for everything else. Use to
	// declare non-zero rates for Ollama fine-tunes (GPU/electricity
	// amortization) or to ship pricing for an obscure commercial
	// provider Mesedi does not yet ship priceTable entries for.
	// NO tier cap, observability of self-hosted costs is a
	// Hobby-tier feature.
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "pricing",
		ThresholdKey: "custom_model_pricing",
		ValueType:    "json",
		Description: "Per-model pricing overrides keyed by " +
			"model_id → {input_per_1m, output_per_1m} USD rates. " +
			"Wins over the canonical priceTable for exact-name " +
			"matches. Use to declare non-zero rates for Ollama " +
			"fine-tunes or to ship pricing for an obscure commercial " +
			"provider Mesedi does not yet ship entries for. " +
			"Default empty map; per-entry rates in [0.0, 10000.0] " +
			"USD per 1M tokens (Mesedi's most expensive commercial " +
			"rate today is $75/1M for Claude Opus output; the cap " +
			"is set 100x higher to bound input sanity); max 50 " +
			"entries; model_id ≤ 200 chars without colon or " +
			"whitespace (signature stability).",
		Default: map[string]ModelPriceOverride{},
		Parse: func(valueJSON, _ string) (any, error) {
			m, err := parseJSONModelPricingMap(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundModelPricingMap(m, 50, 0.0, 10000.0,
				"custom_model_pricing"); err != nil {
				return nil, err
			}
			return m, nil
		},
	})
	// data_leakage.G5, per-project severity-firing policy. Closed
	// set ["critical", "high", "medium"]; default ["critical", "high"]
	// matches the historical hardcoded posture in
	// store.FindFirstDLPSignal's IN clause. NO tier cap, security
	// policy is customer judgment, not a cost knob (same posture as
	// custom DLP patterns + allowlist + prompt-injection patterns).
	registerThresholdSpec(&DetectorThresholdSpec{
		Detector:     "data_leakage",
		ThresholdKey: "severity_policy",
		ValueType:    "json",
		Description: "Severities whose dlp_scan_result hits promote " +
			"to a data_leakage failure_group. Closed set " +
			"[\"critical\", \"high\", \"medium\"]. Default " +
			"[\"critical\", \"high\"] matches the historical posture; " +
			"regulated-industry projects can tighten to include " +
			"\"medium\" so PII patterns page; low-noise projects can " +
			"restrict to [\"critical\"]. The full rule set scans " +
			"regardless: this knob only controls firing.",
		Default: []string{"critical", "high"},
		Parse: func(valueJSON, _ string) (any, error) {
			v, err := parseJSONStringSlice(valueJSON)
			if err != nil {
				return nil, err
			}
			if err := boundStringSliceSubset(v,
				[]string{"critical", "high", "medium"},
				"severity_policy"); err != nil {
				return nil, err
			}
			return v, nil
		},
	})
}

// registerThresholdSpec adds a spec to the registry. Panics on
// duplicate (detector, threshold_key), caught at startup.
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

// parseBoolJSON unmarshals a JSON-encoded boolean. Used by
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
// numeric (float64-coerced). Used by extensions wave 'json'
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

// parseJSONStringSlice unmarshals a JSON-encoded array of strings.
// Used by data_leakage.G5 for the closed-set severity_policy knob;
// generic enough for any future "set of allowed values" registry
// entry (e.g. provider_incident allowed_provider_ids).
func parseJSONStringSlice(valueJSON string) ([]string, error) {
	valueJSON = strings.TrimSpace(valueJSON)
	if valueJSON == "" {
		return nil, fmt.Errorf("empty value")
	}
	var s []string
	if err := json.Unmarshal([]byte(valueJSON), &s); err != nil {
		return nil, fmt.Errorf("expected array of strings, got %q: %w", valueJSON, err)
	}
	return s, nil
}

// boundStringSliceSubset enforces that every element of s is one of
// the values in `allowed`. Empty slice is rejected (caller should
// surface "use the default instead" via the registry's Default
// field, not via empty-array semantics which are too easy to misread
// as "disable detection entirely"). Duplicate elements are accepted
// since the downstream consumer dedups via map membership; rejecting
// them would force the customer to deduplicate before write.
func boundStringSliceSubset(s, allowed []string, name string) error {
	if len(s) == 0 {
		return fmt.Errorf("%s must contain at least one value; "+
			"use the default by deleting the override instead", name)
	}
	allowSet := make(map[string]struct{}, len(allowed))
	for _, a := range allowed {
		allowSet[a] = struct{}{}
	}
	for _, v := range s {
		if _, ok := allowSet[v]; !ok {
			return fmt.Errorf("%s contains invalid value %q; "+
				"allowed values: %v", name, v, allowed)
		}
	}
	return nil
}

// parseJSONIntMap unmarshals a JSON-encoded object whose values are
// integers. Used by context_overflow.G3 for the custom_model_windows
// knob (model_id → window_tokens); generic enough for any future
// per-key-int knob (e.g. per-tool token budgets, per-provider rate-
// limit caps).
func parseJSONIntMap(valueJSON string) (map[string]int, error) {
	valueJSON = strings.TrimSpace(valueJSON)
	if valueJSON == "" {
		return nil, fmt.Errorf("empty value")
	}
	var m map[string]int
	if err := json.Unmarshal([]byte(valueJSON), &m); err != nil {
		return nil, fmt.Errorf("expected object {string: integer}, got %q: %w", valueJSON, err)
	}
	if m == nil {
		m = map[string]int{}
	}
	return m, nil
}

// boundJSONIntMap enforces structural bounds on a parsed int-valued
// map: max key count + per-value range + per-key shape (≤200 chars
// without colon or whitespace, so the model_id can compose into the
// signature `context_overflow:<level>:<model_id>` without parsing
// hazards). The 200-char + no-colon constraints are general enough
// to reuse for future per-key-int knobs whose keys appear in
// failure_group signatures.
func boundJSONIntMap(m map[string]int, maxKeys, lo, hi int, name string) error {
	if len(m) > maxKeys {
		return fmt.Errorf("%s map has %d entries; max is %d",
			name, len(m), maxKeys)
	}
	for k, v := range m {
		if v < lo || v > hi {
			return fmt.Errorf("%s[%s]=%d out of range [%d, %d]",
				name, k, v, lo, hi)
		}
		if len(k) == 0 || len(k) > 200 {
			return fmt.Errorf("%s key %q has length %d; max 200, min 1",
				name, k, len(k))
		}
		if strings.ContainsAny(k, ": \t\n\r") {
			return fmt.Errorf("%s key %q contains forbidden character "+
				"(colon or whitespace); these break signature parsing", name, k)
		}
	}
	return nil
}

// ModelPriceOverride re-exports the pricing package's type so the
// validators registry can declare its Default map type without
// importing the pricing package at every call site. The two types
// are the same struct; this re-export prevents the duplicate-
// definition smell. .
type ModelPriceOverride = pricing.ModelPriceOverride

// parseJSONModelPricingMap accepts a JSON object whose values are
// {input_per_1m, output_per_1m} pairs. Returns a typed map ready for
// bounds checking. .
func parseJSONModelPricingMap(valueJSON string) (map[string]ModelPriceOverride, error) {
	valueJSON = strings.TrimSpace(valueJSON)
	if valueJSON == "" {
		return nil, fmt.Errorf("empty value")
	}
	var m map[string]ModelPriceOverride
	if err := json.Unmarshal([]byte(valueJSON), &m); err != nil {
		return nil, fmt.Errorf("expected object {string: {input_per_1m, "+
			"output_per_1m}}, got %q: %w", valueJSON, err)
	}
	if m == nil {
		m = map[string]ModelPriceOverride{}
	}
	return m, nil
}

// boundModelPricingMap enforces the same structural bounds as
// boundJSONIntMap (max-key-count + per-key 200-char-without-colon
// shape) plus per-rate range on BOTH input and output rates.
// .
func boundModelPricingMap(m map[string]ModelPriceOverride,
	maxKeys int, lo, hi float64, name string) error {
	if len(m) > maxKeys {
		return fmt.Errorf("%s map has %d entries; max is %d",
			name, len(m), maxKeys)
	}
	for k, v := range m {
		if len(k) == 0 || len(k) > 200 {
			return fmt.Errorf("%s key %q has length %d; max 200, min 1",
				name, k, len(k))
		}
		if strings.ContainsAny(k, ": \t\n\r") {
			return fmt.Errorf("%s key %q contains forbidden character "+
				"(colon or whitespace); these break signature parsing", name, k)
		}
		if v.InputPer1M < lo || v.InputPer1M > hi {
			return fmt.Errorf("%s[%s].input_per_1m=%v out of range [%v, %v]",
				name, k, v.InputPer1M, lo, hi)
		}
		if v.OutputPer1M < lo || v.OutputPer1M > hi {
			return fmt.Errorf("%s[%s].output_per_1m=%v out of range [%v, %v]",
				name, k, v.OutputPer1M, lo, hi)
		}
	}
	return nil
}
