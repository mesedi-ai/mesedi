// Context-overflow detector.
//
// Catches the silent agent failure where cumulative input tokens
// across an execution exceed the configured foundation-model's
// context window. The agent doesn't crash; the provider quietly
// truncates the prompt and the agent continues against a fractured
// understanding of its system instructions. This is the
// "agent loses its system prompt at turn 12" failure mode that
// trace-first APMs cannot see.
//
// Implementation:
//
//  1. Per-execution rollup: walk every llm_call event in the
//     execution, group input_tokens by model, find the high-water
//     mark per model.
//
//  2. Lookup: consult models.ContextWindow for each observed model.
//     Skip models the registry doesn't know about (we'd rather
//     under-fire than over-fire on a model we haven't characterized).
//
//  3. Fire: WARN at 90% utilization, FAIL at 100%. The handler reads
//     the returned signature; both severities map to the same
//     failure_class but use distinct signatures so they cluster
//     separately on the dashboard.
//
//  4. The signature format is "<level>:<model_id>" so SREs see one
//     group per (model, severity). When a customer ships a new
//     prompt that takes claude-haiku from 95% → 102%, the new
//     "context_overflow:fail:claude-haiku-..." cluster fires
//     distinctly from the prior "context_overflow:warn:..." one,
//     making the regression timeline obvious.
package detectors

import (
	"encoding/json"
	"fmt"

	"mesedi/backend/internal/models"
)

const (
	// contextOverflowWarnPct is the soft alarm threshold. Below
	// this, context-headroom is treated as safe; at or above this,
	// the dashboard renders a warning-tier failure_group so the
	// customer can see they're approaching the cliff before they
	// fall off it.
	contextOverflowWarnPct = 0.90
	// contextOverflowFailPct is the hard alarm threshold. At or
	// above this, the provider is almost certainly truncating the
	// prompt; the failure_group renders in danger tone.
	contextOverflowFailPct = 1.00
)

// ContextOverflowThresholds carries the per-project tunable values
// for this detector. HighPct + CriticalPct default to
// the historical 0.90 / 1.00 for customers who don't tune.
// CustomModelWindows (context_overflow.G3 wave) maps customer-known
// model_ids to their effective context window in tokens; overrides
// win over the static models.ContextWindow registry. Defaults to
// empty map → every lookup falls through to the registry, matching
// historical behavior.
type ContextOverflowThresholds struct {
	HighPct            float64
	CriticalPct        float64
	CustomModelWindows map[string]int
}

// DefaultContextOverflowThresholds returns the historical hardcoded
// defaults. CustomModelWindows defaults to nil, equivalent to an
// empty map; every model lookup falls through to the registry.
func DefaultContextOverflowThresholds() ContextOverflowThresholds {
	return ContextOverflowThresholds{
		HighPct:     contextOverflowWarnPct,
		CriticalPct: contextOverflowFailPct,
	}
}

// effectiveContextWindow returns the customer's per-project override
// for the given model_id when present and in bounds, else the static
// registry's value. (0, false) when neither has a window for the
// model, caller treats that as "skip detection for this model".
// Defensive: customer-override values outside [1024, 10_000_000]
// fall through to the registry (validators registry rejects these
// at write time; we re-validate at read time for safety against
// any value that escaped the gate).
func (t ContextOverflowThresholds) effectiveContextWindow(model string) (int, bool) {
	if t.CustomModelWindows != nil {
		if w, ok := t.CustomModelWindows[model]; ok && w >= 1024 && w <= 10_000_000 {
			return w, true
		}
	}
	return models.ContextWindow(model)
}

// DetectContextOverflow walks the supplied llm_call payloads and
// reports the highest-severity overflow signal found. Returns
// (signature, true) on detection. Signature shape is
// "context_overflow:<level>:<model>" where level is "warn" or "fail".
//
// Preserved verbatim for backward compatibility; the production
// execution-close path uses DetectContextOverflowWithThresholds.
func DetectContextOverflow(payloads []json.RawMessage) (signature string, detected bool) {
	return DetectContextOverflowWithThresholds(payloads, DefaultContextOverflowThresholds())
}

// DetectContextOverflowWithThresholds is the per-project-aware
// variant. Defensive: out-of-range pcts OR HighPct >= CriticalPct
// fall back to defaults (validators registry rejects out-of-range
// at write time; cross-pct ordering is the detector's responsibility).
func DetectContextOverflowWithThresholds(
	payloads []json.RawMessage,
	t ContextOverflowThresholds,
) (signature string, detected bool) {
	warnPct := t.HighPct
	failPct := t.CriticalPct
	if warnPct < 0.5 || warnPct > 1.0 ||
		failPct < 0.5 || failPct > 1.0 ||
		warnPct >= failPct {
		warnPct = contextOverflowWarnPct
		failPct = contextOverflowFailPct
	}
	if len(payloads) == 0 {
		return "", false
	}
	// Track the high-water input_tokens we saw per model.
	highWater := map[string]int{}
	for _, raw := range payloads {
		var p struct {
			Model       string `json:"model"`
			InputTokens int    `json:"input_tokens"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		if p.Model == "" || p.InputTokens <= 0 {
			continue
		}
		if p.InputTokens > highWater[p.Model] {
			highWater[p.Model] = p.InputTokens
		}
	}
	if len(highWater) == 0 {
		return "", false
	}

	// Pick the worst offender across all observed models.
	worstLevel := ""
	worstModel := ""
	worstPct := 0.0
	for model, tokens := range highWater {
		// context_overflow.G3, customer override wins over the
		// static registry. Unknown models with no override silently
		// skip detection (same posture as before; the override path
		// is what unlocks coverage for Ollama / fine-tuned models).
		window, ok := t.effectiveContextWindow(model)
		if !ok {
			continue
		}
		pct := float64(tokens) / float64(window)
		if pct < warnPct {
			continue
		}
		level := "warn"
		if pct >= failPct {
			level = "fail"
		}
		// "fail" outranks "warn"; within same level, higher pct
		// wins; within identical pct, lexicographically-earlier
		// model wins (deterministic across iteration order).
		if betterAlarm(level, pct, model, worstLevel, worstPct, worstModel) {
			worstLevel = level
			worstModel = model
			worstPct = pct
		}
	}
	if worstLevel == "" {
		return "", false
	}
	return fmt.Sprintf("context_overflow:%s:%s", worstLevel, worstModel), true
}

// betterAlarm encodes the priority ordering used to pick a single
// alarm out of a per-model rollup. Pulled out for clarity rather
// than inlined as a multi-clause if.
func betterAlarm(newLevel string, newPct float64, newModel, curLevel string, curPct float64, curModel string) bool {
	if curLevel == "" {
		return true
	}
	// "fail" always beats "warn".
	if newLevel != curLevel {
		return newLevel == "fail"
	}
	// Same level, higher percentage wins.
	if newPct != curPct {
		return newPct > curPct
	}
	// Same level and same percentage, pick lexicographically earlier
	// model so the choice is stable across iteration order.
	return newModel < curModel
}
