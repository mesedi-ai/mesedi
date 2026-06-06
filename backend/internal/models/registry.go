// Package models is the registry of foundation-model capability
// metadata that downstream detectors consult to evaluate against
// per-model thresholds. Today the only attribute tracked is the
// maximum context window in tokens, but the package is structured to
// grow per-model metadata (provider, pricing dimensions, rate limits)
// without churning callers.
//
// Detectors that consume this registry:
//
//   - context_overflow (Mesedi item #3): compares cumulative tokens
//     across an execution against the configured model's context
//     window, fires at WARN @ 90% and FAIL @ 100%.
//
//   - token_waste (Mesedi item #4): uses the context window as one
//     dimension of the "repeated context prefix" calculation
//     (the larger the window, the more wasted bytes per repeat).
//
// Design notes:
//
//  1. The registry is a static map keyed by exact model identifier.
//     Lookups are case-sensitive because providers ship variants of
//     the same family with different windows (e.g. claude-haiku-4-5
//     vs claude-haiku-4-5-20251001 may differ).
//
//  2. For unknown models the lookup returns ok=false. Detectors that
//     consult the registry must handle this gracefully (the explicit
//     contract is "skip per-model thresholds when we don't know the
//     model" rather than "guess and over-page"). This matters because
//     customers add new models constantly and we'd rather under-fire
//     than false-fire.
//
//  3. The registry is intentionally NOT loaded from a database in
//     this first pass. Model windows change at human cadence (every
//     few months as new generations ship) and version control is the
//     right primary source. A future pass can layer a per-tenant DB
//     override on top when customers want to declare e.g. "we run a
//     fine-tuned Sonnet capped at 100k for cost control" without
//     waiting on a redeploy.
//
//  4. Values are sourced from each provider's official documentation
//     at the time the model shipped. They are conservative: when a
//     provider advertises a window that requires a special opt-in
//     tier (e.g. Claude's 1M context extended-context beta), the
//     registry tracks the default-tier window, not the upper bound.
package models

// ContextWindow returns the maximum input token capacity for the
// given model identifier. The second return value is false when the
// model isn't in the registry. Callers (notably the context_overflow
// detector) MUST skip per-model checks when ok=false rather than
// substituting a default, to avoid false positives on models we
// haven't characterized yet.
func ContextWindow(modelID string) (tokens int, ok bool) {
	w, ok := windowByModel[modelID]
	return w, ok
}

// Provider returns the provider hint ("anthropic", "openai", etc.)
// for the given model identifier. Useful for cross-tenant signals
// like provider_incident (#16) and for routing rate-limit data to
// the right backoff strategy. Returns "" when the model isn't known.
func Provider(modelID string) string {
	return providerByModel[modelID]
}

// IsKnown is a cheap predicate for callers that don't need the
// window or provider value, just whether the registry recognizes
// the model. Equivalent to checking ContextWindow's ok return.
func IsKnown(modelID string) bool {
	_, ok := windowByModel[modelID]
	return ok
}

// windowByModel maps each known model identifier to its conservative
// default-tier context window in tokens. Keep entries alphabetized
// within each provider block, and prefer specific-dated identifiers
// over family aliases so detectors evaluate against the exact build
// the SDK reported.
//
// Sources (all official docs current as of the last update to this
// file):
//
//   - Anthropic:    https://docs.anthropic.com/claude/docs/models-overview
//   - OpenAI:       https://platform.openai.com/docs/models
//   - Google:       https://ai.google.dev/gemini-api/docs/models
//   - Meta:         https://ai.meta.com/llama/
//   - Mistral:      https://docs.mistral.ai/getting-started/models/
//
// When adding a new model, also add an entry to providerByModel so
// the Provider() lookup stays consistent. The Test_RegistryParity
// unit test in registry_test.go fails the build if these go out of
// sync.
var windowByModel = map[string]int{
	// ── Anthropic ─────────────────────────────────────────────────
	"claude-opus-4-6":           200_000,
	"claude-sonnet-4-6":         200_000,
	"claude-haiku-4-5":          200_000,
	"claude-haiku-4-5-20251001": 200_000,
	"claude-3-7-sonnet":         200_000,
	"claude-3-5-sonnet":         200_000,
	"claude-3-5-haiku":          200_000,
	"claude-3-opus":             200_000,
	"claude-3-sonnet":           200_000,
	"claude-3-haiku":            200_000,
	"claude-2.1":                200_000,
	"claude-2":                  100_000,

	// ── OpenAI ────────────────────────────────────────────────────
	"gpt-5":         400_000,
	"gpt-5-mini":    400_000,
	"gpt-5-nano":    400_000,
	"gpt-4.1":       1_000_000,
	"gpt-4.1-mini":  1_000_000,
	"gpt-4.1-nano":  1_000_000,
	"gpt-4o":        128_000,
	"gpt-4o-mini":   128_000,
	"gpt-4-turbo":   128_000,
	"gpt-4":         8_192,
	"gpt-3.5-turbo": 16_385,
	"o3":            200_000,
	"o3-mini":       200_000,
	"o1":            200_000,
	"o1-mini":       128_000,

	// ── Google ────────────────────────────────────────────────────
	"gemini-2.5-pro":   2_000_000,
	"gemini-2.5-flash": 1_000_000,
	"gemini-2.0-pro":   2_000_000,
	"gemini-2.0-flash": 1_000_000,
	"gemini-1.5-pro":   2_000_000,
	"gemini-1.5-flash": 1_000_000,

	// ── Meta / Llama ──────────────────────────────────────────────
	"llama-4-scout":    10_000_000,
	"llama-4-maverick": 1_000_000,
	"llama-3.3-70b":    128_000,
	"llama-3.2-90b":    128_000,
	"llama-3.1-405b":   128_000,
	"llama-3.1-70b":    128_000,
	"llama-3.1-8b":     128_000,

	// ── Mistral ───────────────────────────────────────────────────
	"mistral-large-2":    128_000,
	"mistral-large-2407": 128_000,
	"mistral-medium":     32_000,
	"mistral-small-3":    32_000,
	"mistral-nemo":       128_000,
	"codestral":          256_000,
}

// providerByModel groups each model under its vendor for cross-tenant
// signals and rate-limit routing. Keep in sync with windowByModel; the
// Test_RegistryParity test enforces this.
var providerByModel = map[string]string{
	// Anthropic
	"claude-opus-4-6":           "anthropic",
	"claude-sonnet-4-6":         "anthropic",
	"claude-haiku-4-5":          "anthropic",
	"claude-haiku-4-5-20251001": "anthropic",
	"claude-3-7-sonnet":         "anthropic",
	"claude-3-5-sonnet":         "anthropic",
	"claude-3-5-haiku":          "anthropic",
	"claude-3-opus":             "anthropic",
	"claude-3-sonnet":           "anthropic",
	"claude-3-haiku":            "anthropic",
	"claude-2.1":                "anthropic",
	"claude-2":                  "anthropic",
	// OpenAI
	"gpt-5":         "openai",
	"gpt-5-mini":    "openai",
	"gpt-5-nano":    "openai",
	"gpt-4.1":       "openai",
	"gpt-4.1-mini":  "openai",
	"gpt-4.1-nano":  "openai",
	"gpt-4o":        "openai",
	"gpt-4o-mini":   "openai",
	"gpt-4-turbo":   "openai",
	"gpt-4":         "openai",
	"gpt-3.5-turbo": "openai",
	"o3":            "openai",
	"o3-mini":       "openai",
	"o1":            "openai",
	"o1-mini":       "openai",
	// Google
	"gemini-2.5-pro":   "google",
	"gemini-2.5-flash": "google",
	"gemini-2.0-pro":   "google",
	"gemini-2.0-flash": "google",
	"gemini-1.5-pro":   "google",
	"gemini-1.5-flash": "google",
	// Meta
	"llama-4-scout":    "meta",
	"llama-4-maverick": "meta",
	"llama-3.3-70b":    "meta",
	"llama-3.2-90b":    "meta",
	"llama-3.1-405b":   "meta",
	"llama-3.1-70b":    "meta",
	"llama-3.1-8b":     "meta",
	// Mistral
	"mistral-large-2":    "mistral",
	"mistral-large-2407": "mistral",
	"mistral-medium":     "mistral",
	"mistral-small-3":    "mistral",
	"mistral-nemo":       "mistral",
	"codestral":          "mistral",
}
