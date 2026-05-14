// Package pricing maps foundation-model identifiers to per-token cost so
// the backend can compute estimated_cost_usd on executions and roll those
// costs up to failure_groups.
//
// **Posture for v0.0.1**: prices are hardcoded in this file with a
// last-updated note. When a model is launched at a new price, or when
// existing prices change, edit the map and bump the lastUpdated
// constant. Future versions will pull this from a config file, an
// admin-editable table, or a small external service.
//
// **Lookup behavior**: lookup is case-insensitive on a prefix match. The
// table keys are the canonical model identifiers (e.g. "claude-opus-4-6");
// matchers like "claude-opus-4-6-20260301" still hit the row because the
// stable price is keyed on the family. Unknown models return (0, 0, false)
// — cost stays $0 rather than guessing.
//
// **Units**: prices in the table are USD per 1 million tokens, matching
// the way every provider currently publishes them. The compute helper
// converts to per-token internally.
package pricing

import (
	"strings"
)

// lastUpdated documents the most recent time the price map was reviewed.
// Bump this when editing prices below so reviewers can spot a stale map.
const lastUpdated = "2026-05-14"

// modelPrice is the cost structure for one model: input + output rate
// per 1 million tokens.
type modelPrice struct {
	InputPer1M  float64
	OutputPer1M float64
}

// priceTable is the source-of-truth for foundation-model pricing.
//
// **Update protocol**: model launched, repriced, or deprecated →
//  1. Add or update the entry below
//  2. Bump lastUpdated to today
//  3. Add a comment with the source URL the price came from
//
// Order: most-recently launched first within each provider, since most
// production traffic goes to the newest model.
var priceTable = map[string]modelPrice{
	// ── Anthropic (claude.com pricing as of lastUpdated) ─────────────
	"claude-opus-4-6":    {InputPer1M: 15.00, OutputPer1M: 75.00},
	"claude-sonnet-4-6":  {InputPer1M: 3.00, OutputPer1M: 15.00},
	"claude-haiku-4-5":   {InputPer1M: 1.00, OutputPer1M: 5.00},
	"claude-opus-4-1":    {InputPer1M: 15.00, OutputPer1M: 75.00},
	"claude-sonnet-4":    {InputPer1M: 3.00, OutputPer1M: 15.00},
	"claude-3-5-sonnet":  {InputPer1M: 3.00, OutputPer1M: 15.00},
	"claude-3-5-haiku":   {InputPer1M: 1.00, OutputPer1M: 5.00},
	"claude-3-opus":      {InputPer1M: 15.00, OutputPer1M: 75.00},

	// ── OpenAI (platform.openai.com pricing as of lastUpdated) ───────
	"gpt-4o":             {InputPer1M: 2.50, OutputPer1M: 10.00},
	"gpt-4o-mini":        {InputPer1M: 0.15, OutputPer1M: 0.60},
	"gpt-4-turbo":        {InputPer1M: 10.00, OutputPer1M: 30.00},
	"gpt-4":              {InputPer1M: 30.00, OutputPer1M: 60.00},
	"gpt-3.5-turbo":      {InputPer1M: 0.50, OutputPer1M: 1.50},
	"o1":                 {InputPer1M: 15.00, OutputPer1M: 60.00},
	"o1-mini":            {InputPer1M: 3.00, OutputPer1M: 12.00},
}

// ComputeLLMCost returns the estimated USD cost of a single LLM call
// given the model identifier and observed input/output token counts.
//
// Resolution: tries the model name as given (case-insensitive), then
// falls back to prefix matches against the table keys. Unknown models
// return 0.0 (no estimated cost) rather than guessing — the dashboard
// renders this as "—" naturally.
//
// Example:
//
//	cost := ComputeLLMCost("claude-opus-4-6", 1200, 800)
//	// = 1200/1_000_000 * 15.00 + 800/1_000_000 * 75.00
//	// = 0.018 + 0.060 = $0.078
func ComputeLLMCost(model string, inputTokens, outputTokens int) float64 {
	p, ok := lookup(model)
	if !ok {
		return 0
	}
	return (float64(inputTokens)/1_000_000.0)*p.InputPer1M +
		(float64(outputTokens)/1_000_000.0)*p.OutputPer1M
}

// IsKnownModel reports whether the model identifier resolves to a
// pricing row. Useful for callers that want to distinguish "unknown
// model" from "zero cost."
func IsKnownModel(model string) bool {
	_, ok := lookup(model)
	return ok
}

// LastUpdated returns the date the price table was last reviewed.
// Useful in admin / observability surfaces.
func LastUpdated() string { return lastUpdated }

// lookup tries exact match first (case-insensitive), then prefix-match
// against the table keys. The prefix match handles dated suffixes like
// "claude-opus-4-6-20260301" that providers sometimes use.
func lookup(model string) (modelPrice, bool) {
	if model == "" {
		return modelPrice{}, false
	}
	lower := strings.ToLower(model)
	if p, ok := priceTable[lower]; ok {
		return p, true
	}
	// Prefix-match: scan the table, prefer the longest key that's a
	// prefix of the input. This avoids accidentally matching
	// "claude-opus-4-1" with a row keyed "claude" — we want the most
	// specific known family.
	var (
		best    modelPrice
		bestLen int
		found   bool
	)
	for k, v := range priceTable {
		if strings.HasPrefix(lower, k) && len(k) > bestLen {
			best, bestLen, found = v, len(k), true
		}
	}
	return best, found
}
