// Package pricing maps foundation-model identifiers to per-token cost so
// the backend can compute estimated_cost_usd on executions and roll those
// costs up to failure_groups.
//
// **Posture**: prices are hardcoded in this file with a version stamp.
// When a model is launched at a new price, or when existing prices change,
// edit the map and bump the PricingTableVersion constant. Future versions
// may pull this from a config file, an admin-editable table, or a small
// external service.
//
// **Lookup behavior**: lookup is case-insensitive with prefix-match
// fallback. The table keys are the canonical model identifiers (e.g.
// "claude-opus-4-6"); matchers like "claude-opus-4-6-20260301" still hit
// the row because the stable price is keyed on the family. Unknown models
// return (0, false), cost stays $0 from this package's perspective. The
// caller (handler) decides what to do with the (0, false) result —
// typically falling back to the SDK-shipped per-event estimated_cost_usd.
//
// **Source of truth (Wave 0.3)**: the backend is now authoritative for
// known models. The handler always calls into this package at execution
// close; SDK-shipped per-event cost is fallback for unknown models only.
// This means new model pricing (or pricing changes) ship with a backend
// deploy — no SDK release wait.
//
// **Units**: prices in the table are USD per 1 million tokens, matching
// the way every provider currently publishes them. The compute helper
// converts to per-token internally.
//
// **Sources**: each provider block links to the official pricing page.
// Verify quarterly (or whenever PricingTableVersion was last bumped).
package pricing

import (
	"sort"
	"strings"
)

// PricingTableVersion documents the most recent time the price map was
// reviewed. Bump when editing prices below so reviewers + customers can
// see staleness. Exposed via GET /me/pricing-info so customers can see
// which prices Mesedi is using.
const PricingTableVersion = "2026-06-22"

// modelPrice is the cost structure for one model: input + output rate
// per 1 million tokens, plus optional long-context tier rates that
// some providers (notably Google for Gemini Pro families) apply when
// a single call's input_tokens exceed a breakpoint.
//
// Tier-flip semantics (Wave 0.6): when TierBreakpointInputTokens is
// > 0 AND an individual call's input_tokens > TierBreakpointInputTokens,
// the call gets billed at the over-tier rates for BOTH input and
// output — matching Google's documented behavior where exceeding the
// threshold flips the ENTIRE call to long-context pricing, not just
// the tokens above the threshold. For models without tiered pricing
// (every Anthropic / OpenAI / Cohere / Mistral / Llama entry, every
// Gemini Flash variant), TierBreakpointInputTokens is left at 0 and
// the over-tier fields are ignored — the existing flat-rate semantics
// are unchanged.
type modelPrice struct {
	InputPer1M              float64
	OutputPer1M             float64
	InputPer1MOverTier      float64
	OutputPer1MOverTier     float64
	TierBreakpointInputTokens int
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
	// ── Anthropic (docs.anthropic.com/en/docs/about-claude/pricing) ──
	"claude-opus-4-6":   {InputPer1M: 15.00, OutputPer1M: 75.00},
	"claude-sonnet-4-6": {InputPer1M: 3.00, OutputPer1M: 15.00},
	"claude-haiku-4-5":  {InputPer1M: 1.00, OutputPer1M: 5.00},
	"claude-opus-4-1":   {InputPer1M: 15.00, OutputPer1M: 75.00},
	"claude-sonnet-4":   {InputPer1M: 3.00, OutputPer1M: 15.00},
	"claude-3-7-sonnet": {InputPer1M: 3.00, OutputPer1M: 15.00},
	"claude-3-5-sonnet": {InputPer1M: 3.00, OutputPer1M: 15.00},
	"claude-3-5-haiku":  {InputPer1M: 1.00, OutputPer1M: 5.00},
	"claude-3-opus":     {InputPer1M: 15.00, OutputPer1M: 75.00},
	"claude-3-sonnet":   {InputPer1M: 3.00, OutputPer1M: 15.00},
	"claude-3-haiku":    {InputPer1M: 0.25, OutputPer1M: 1.25},
	"claude-2.1":        {InputPer1M: 8.00, OutputPer1M: 24.00},
	"claude-2":          {InputPer1M: 8.00, OutputPer1M: 24.00},

	// ── OpenAI (platform.openai.com/pricing) ─────────────────────────
	"gpt-5":         {InputPer1M: 1.25, OutputPer1M: 10.00},
	"gpt-5-mini":    {InputPer1M: 0.25, OutputPer1M: 2.00},
	"gpt-5-nano":    {InputPer1M: 0.05, OutputPer1M: 0.40},
	"gpt-4.1":       {InputPer1M: 2.00, OutputPer1M: 8.00},
	"gpt-4.1-mini":  {InputPer1M: 0.40, OutputPer1M: 1.60},
	"gpt-4.1-nano":  {InputPer1M: 0.10, OutputPer1M: 0.40},
	"gpt-4o":        {InputPer1M: 2.50, OutputPer1M: 10.00},
	"gpt-4o-mini":   {InputPer1M: 0.15, OutputPer1M: 0.60},
	"gpt-4-turbo":   {InputPer1M: 10.00, OutputPer1M: 30.00},
	"gpt-4":         {InputPer1M: 30.00, OutputPer1M: 60.00},
	"gpt-3.5-turbo": {InputPer1M: 0.50, OutputPer1M: 1.50},
	"o3":            {InputPer1M: 2.00, OutputPer1M: 8.00},
	"o3-mini":       {InputPer1M: 1.10, OutputPer1M: 4.40},
	"o1":            {InputPer1M: 15.00, OutputPer1M: 60.00},
	"o1-mini":       {InputPer1M: 3.00, OutputPer1M: 12.00},

	// ── Google (ai.google.dev/gemini-api/docs/pricing, fetched 2026-06-22) ──
	// Gemini Pro families use long-context tier pricing: when a single
	// call's input_tokens exceeds 200,000, BOTH input and output get
	// billed at the higher rate. Flash families are flat-rate per
	// Google's current docs (no tier breakpoint). See Wave 0.6
	// closeout for the per-rate breakdown and source links.
	"gemini-2.5-pro": {
		InputPer1M:                1.25,
		OutputPer1M:               10.00,
		InputPer1MOverTier:        2.50,
		OutputPer1MOverTier:       15.00,
		TierBreakpointInputTokens: 200_000,
	},
	"gemini-3.1-pro-preview": {
		InputPer1M:                2.00,
		OutputPer1M:               12.00,
		InputPer1MOverTier:        4.00,
		OutputPer1MOverTier:       18.00,
		TierBreakpointInputTokens: 200_000,
	},
	"gemini-3.5-flash":       {InputPer1M: 1.50, OutputPer1M: 9.00},
	"gemini-3.1-flash-lite":  {InputPer1M: 0.25, OutputPer1M: 1.50},
	"gemini-2.5-flash":       {InputPer1M: 0.30, OutputPer1M: 2.50},
	// gemini-2.0-pro and gemini-1.5-pro retain the legacy flat-rate
	// shipped before Wave 0.6 (Google's current pricing page no longer
	// documents them; preserving customer-visible cost continuity
	// until a dedicated legacy-pricing-deprecation wave). These should
	// be revisited if Google publishes updated rates or formally
	// deprecates the families.
	"gemini-2.0-pro":   {InputPer1M: 1.25, OutputPer1M: 10.00},
	"gemini-1.5-pro":   {InputPer1M: 1.25, OutputPer1M: 5.00},
	"gemini-1.5-flash": {InputPer1M: 0.075, OutputPer1M: 0.30},
	// gemini-2.0-flash was deprecated and shut down by Google on
	// 2026-06-01. Pricing kept at $0/$0 so cost_velocity does not
	// double-bill for stale historical event records that may still
	// resolve via the prefix-match path; the model can no longer be
	// invoked successfully so prospective customer cost is zero.
	"gemini-2.0-flash": {InputPer1M: 0, OutputPer1M: 0},

	// ── Meta Llama (ai.meta.com/llama) ────────────────────────────────
	// Llama weights are free for self-hosted deployments and Mesedi
	// has no visibility into per-customer hosting agreements (Together,
	// Groq, Fireworks, Bedrock, self-hosted, etc.). Default to $0 so
	// the detector silently doesn't fire on Llama-only workloads.
	// Customers with paid Llama hosting can declare actual pricing via
	// per-tenant overrides (future work — see Decision 1c in audit).
	"llama-4-scout":    {InputPer1M: 0.00, OutputPer1M: 0.00},
	"llama-4-maverick": {InputPer1M: 0.00, OutputPer1M: 0.00},
	"llama-3.3-70b":    {InputPer1M: 0.00, OutputPer1M: 0.00},
	"llama-3.2-90b":    {InputPer1M: 0.00, OutputPer1M: 0.00},
	"llama-3.1-405b":   {InputPer1M: 0.00, OutputPer1M: 0.00},
	"llama-3.1-70b":    {InputPer1M: 0.00, OutputPer1M: 0.00},
	"llama-3.1-8b":     {InputPer1M: 0.00, OutputPer1M: 0.00},

	// ── Mistral (docs.mistral.ai/getting-started/models) ─────────────
	"mistral-large-2":    {InputPer1M: 2.00, OutputPer1M: 6.00},
	"mistral-large-2407": {InputPer1M: 2.00, OutputPer1M: 6.00},
	"mistral-medium":     {InputPer1M: 2.70, OutputPer1M: 8.10},
	"mistral-small-3":    {InputPer1M: 0.20, OutputPer1M: 0.60},
	"mistral-nemo":       {InputPer1M: 0.15, OutputPer1M: 0.15},
	"codestral":          {InputPer1M: 0.30, OutputPer1M: 0.90},

	// ── Ollama (local; no API cost) ──────────────────────────────────
	// Ollama is locally-hosted with zero per-token API cost. The
	// identifiers below match the canonical Llama / Mistral names; if
	// a customer is running an Ollama model via instrument_ollama with
	// a tag-style identifier (e.g. "llama3.2:3b" instead of
	// "llama-3.2-90b"), it will land in the unknown-model fallback
	// path. Wave 2.5.4 extends this table with Ollama-tag-style
	// identifiers as part of the instrument_ollama work.
	// (Entries above under "Meta Llama" already cover llama-*; this
	// block is a placeholder for future Ollama-specific tags so the
	// reviewer sees the intent.)
}

// ComputeLLMCost returns the estimated USD cost of a single LLM call
// given the model identifier and observed input/output token counts.
//
// Resolution: tries the model name as given (case-insensitive), then
// falls back to prefix matches against the table keys. Unknown models
// return 0.0 (no estimated cost) rather than guessing, the dashboard
// renders this as ", " naturally.
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
	// Tier-flip: when a per-call input_tokens count exceeds the
	// configured breakpoint, BOTH input and output get billed at the
	// over-tier rates. Matches Google's documented long-context
	// behavior for Gemini Pro models (>200k input flips the whole
	// call). For flat-rate models, TierBreakpointInputTokens is 0
	// and this branch never executes — the legacy single-rate path
	// applies.
	inputRate := p.InputPer1M
	outputRate := p.OutputPer1M
	if p.TierBreakpointInputTokens > 0 && inputTokens > p.TierBreakpointInputTokens {
		inputRate = p.InputPer1MOverTier
		outputRate = p.OutputPer1MOverTier
	}
	return (float64(inputTokens)/1_000_000.0)*inputRate +
		(float64(outputTokens)/1_000_000.0)*outputRate
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
//
// Deprecated: Use PricingTableVersion directly. LastUpdated remains for
// backward-compat with callers that expect a function.
func LastUpdated() string { return PricingTableVersion }

// SupportedModels returns the sorted list of model identifiers in the
// pricing table. Used by GET /me/pricing-info so customers can see
// exactly which models Mesedi prices server-side and which models will
// fall back to the SDK-shipped per-event cost.
func SupportedModels() []string {
	out := make([]string, 0, len(priceTable))
	for k := range priceTable {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

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
	// "claude-opus-4-1" with a row keyed "claude", we want the most
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
