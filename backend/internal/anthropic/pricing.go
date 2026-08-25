package anthropic

// Per-model pricing for cost-attribution math (admin dashboard).
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
	// Sonnet 5 is both NEWER and CHEAPER than sonnet-4-6 ($2/$10 vs
	// $3/$15). Added 2026-08-20 when the premium analysis tier started
	// using it; without this entry LookupRate fell through to
	// UnknownModelRate, which happens to be $2/$10 as well — correct by
	// coincidence, not by design.
	"claude-sonnet-5":   {InputUSDPerMTok: 2.00, OutputUSDPerMTok: 10.00},
	"claude-sonnet-4":   {InputUSDPerMTok: 3.00, OutputUSDPerMTok: 15.00},
	"claude-sonnet-4-6": {InputUSDPerMTok: 3.00, OutputUSDPerMTok: 15.00},
	"claude-opus-4":     {InputUSDPerMTok: 15.00, OutputUSDPerMTok: 75.00},
	"claude-opus-4-6":   {InputUSDPerMTok: 15.00, OutputUSDPerMTok: 75.00},
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

// HasExplicitRate reports whether modelID resolves to a real entry in
// the rate table (exact or prefix match) rather than falling through to
// UnknownModelRate.
//
// This exists because LookupRate's return value alone cannot answer the
// question: a listed model whose price happens to equal UnknownModelRate
// is indistinguishable from an unlisted one. claude-sonnet-5 is exactly
// that case — it really is $2/$10, identical to the fallback — so a test
// comparing rates by value reported it as missing even after it was
// added. Callers that need "is this model actually known to us" (cost
// reporting, drift guards) must use this, not a value comparison.
func HasExplicitRate(modelID string) bool {
	id := strings.ToLower(strings.TrimSpace(modelID))
	if id == "" {
		return false
	}
	if _, ok := modelRates[id]; ok {
		return true
	}
	for known := range modelRates {
		if strings.HasPrefix(id, known) {
			return true
		}
	}
	return false
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

// Prompt-caching multipliers, expressed against the model's base
// input rate. Anthropic prices cached tokens relative to normal
// input rather than publishing separate per-model numbers, so one
// set of multipliers covers every model in the table above.
//
//	writing to a 5-minute cache costs 1.25x input
//	writing to a 1-hour cache costs 2.00x input
//	reading from either cache costs 0.10x input
//
// These exist because the Admin API's usage report breaks tokens
// into uncached input, cache-creation (split by TTL) and cache-read
// buckets. Summing them as if they were all plain input would
// over-report reads by 10x and under-report writes — and today they
// are all zero only because prompt caching is not in use yet. The
// moment it is, a naive sum silently becomes wrong.
const (
	cacheWrite5mMultiplier = 1.25
	cacheWrite1hMultiplier = 2.00
	cacheReadMultiplier    = 0.10
)

// TokenUsage is a full breakdown of one model's token consumption
// over some window, matching the shape the Admin API's usage report
// returns.
type TokenUsage struct {
	UncachedInputTokens  int
	CacheWrite5mTokens   int
	CacheWrite1hTokens   int
	CacheReadInputTokens int
	OutputTokens         int
}

// TotalInputTokens returns every input-side token regardless of
// cache treatment. Useful for display ("2.1M tokens in") where the
// billing distinction does not matter.
func (u TokenUsage) TotalInputTokens() int {
	return u.UncachedInputTokens + u.CacheWrite5mTokens +
		u.CacheWrite1hTokens + u.CacheReadInputTokens
}

// ComputeUsageCostUSD prices a full token breakdown, applying the
// cache multipliers above. Prefer this over ComputeCostUSD anywhere
// the caller has cache detail; ComputeCostUSD remains correct for
// the analyze path, which makes uncached calls.
func ComputeUsageCostUSD(modelID string, u TokenUsage) float64 {
	r := LookupRate(modelID)
	const tokensPerMTok = 1_000_000.0
	inRate := r.InputUSDPerMTok / tokensPerMTok

	cost := float64(u.UncachedInputTokens) * inRate
	cost += float64(u.CacheWrite5mTokens) * inRate * cacheWrite5mMultiplier
	cost += float64(u.CacheWrite1hTokens) * inRate * cacheWrite1hMultiplier
	cost += float64(u.CacheReadInputTokens) * inRate * cacheReadMultiplier
	cost += float64(u.OutputTokens) / tokensPerMTok * r.OutputUSDPerMTok
	return cost
}
