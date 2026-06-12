package anthropic

// Per-model pricing for cost-attribution math (#199 admin dashboard).
//
// Anthropic publishes per-million-token rates ($/MTok) on
// https://www.anthropic.com/pricing. We track input + output rates
// only here; cache-write/cache-read discounts are intentionally
// out of scope for the founder accounting view (re-add when prompt
// caching lands in the Mesedi analyze call path).
//
// Rates are denominated in USD per 1,000,000 tokens, the same
// units as the Anthropic pricing page. Convert to per-token by
// dividing by 1_000_000 in ComputeCostUSD.
//
// Keep this list short and alphabetized; add new models when the
// codebase starts using them, not preemptively. Unknown model IDs
// fall through to UnknownModelRate so an unrecognized model still
// produces a non-zero cost estimate rather than silently zero-out
// the founder's accounting.

import "strings"

// ModelRate is the per-MTok pricing for one model.
type ModelRate struct {
	// InputUSDPerMTok is the price in USD per 1,000,000 input tokens.
	InputUSDPerMTok float64
	// OutputUSDPerMTok is the price in USD per 1,000,000 output
	// tokens. Output is typically 5x input for the Claude family.
	OutputUSDPerMTok float64
}

// modelRates lists known Anthropic model pricing. Add entries as
// the codebase adopts new models. Values reflect the published
// rates on anthropic.com/pricing as of June 2026; update when
// Anthropic adjusts pricing.
var modelRates = map[string]ModelRate{
	"claude-haiku-4-5":        {InputUSDPerMTok: 1.00, OutputUSDPerMTok: 5.00},
	"claude-haiku-4-5-2025xx": {InputUSDPerMTok: 1.00, OutputUSDPerMTok: 5.00},
	"claude-sonnet-4":         {InputUSDPerMTok: 3.00, OutputUSDPerMTok: 15.00},
	"claude-sonnet-4-6":       {InputUSDPerMTok: 3.00, OutputUSDPerMTok: 15.00},
	"claude-opus-4":           {InputUSDPerMTok: 15.00, OutputUSDPerMTok: 75.00},
	"claude-opus-4-6":         {InputUSDPerMTok: 15.00, OutputUSDPerMTok: 75.00},
}

// UnknownModelRate is the fallback when a model id isn't in
// modelRates. Conservative midpoint between Haiku and Sonnet so the
// founder sees a meaningful number rather than $0 when a new model
// id slips through.
var UnknownModelRate = ModelRate{
	InputUSDPerMTok:  2.00,
	OutputUSDPerMTok: 10.00,
}

// LookupRate returns the pricing for modelID with fuzzy fallback.
// Lookup tries exact match first, then a prefix match (e.g.,
// "claude-haiku-4-5-snapshot-20251001" -> claude-haiku-4-5), then
// the conservative UnknownModelRate.
func LookupRate(modelID string) ModelRate {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return UnknownModelRate
	}
	if r, ok := modelRates[id]; ok {
		return r
	}
	// Prefix match for model variants we haven't listed explicitly
	// (e.g., snapshot-suffixed IDs). Pick the longest matching prefix
	// to prefer specificity.
	var bestPrefix string
	for known := range modelRates {
		if strings.HasPrefix(id, known) && len(known) > len(bestPrefix) {
			bestPrefix = known
		}
	}
	if bestPrefix != "" {
		return modelRates[bestPrefix]
	}
	return UnknownModelRate
}

// ComputeCostUSD returns the USD cost of one call given token
// counts and the model id. Used by the analyze handler to fill in
// ai_analyses.cost_usd at write time so the cost is denormalized
// at the historical rate (later pricing changes don't rewrite the
// past).
func ComputeCostUSD(modelID string, inputTokens, outputTokens int) float64 {
	r := LookupRate(modelID)
	const tokensPerMTok = 1_000_000.0
	in := float64(inputTokens) / tokensPerMTok * r.InputUSDPerMTok
	out := float64(outputTokens) / tokensPerMTok * r.OutputUSDPerMTok
	return in + out
}
