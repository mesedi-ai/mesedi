// Context-overflow detector (Mesedi #3).
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

// DetectContextOverflow walks the supplied llm_call payloads and
// reports the highest-severity overflow signal found. Returns
// (signature, true) on detection. Signature shape is
// "context_overflow:<level>:<model>" where level is "warn" or "fail".
//
// The detector takes the MAX cumulative input_tokens per (model)
// across the execution: agents that make multiple LLM calls
// independently (e.g. retry + retry-with-context-reduction) shouldn't
// trip just because they spike once. The high-water mark captures
// the actual ceiling the agent operated under.
//
// Returns ("", false) when:
//   - payloads is empty
//   - no payload had a usable model + input_tokens
//   - no observed model is in the registry
//   - the highest utilization observed is below
//     contextOverflowWarnPct
func DetectContextOverflow(payloads []json.RawMessage) (signature string, detected bool) {
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
		window, ok := models.ContextWindow(model)
		if !ok {
			continue
		}
		pct := float64(tokens) / float64(window)
		if pct < contextOverflowWarnPct {
			continue
		}
		level := "warn"
		if pct >= contextOverflowFailPct {
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
