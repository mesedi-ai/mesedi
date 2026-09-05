// Package otel holds configuration shared by future OpenTelemetry
// emission paths. For now, the package's only surface
// is honoring OTEL_SEMCONV_STABILITY_OPT_IN so that
// enterprise pipelines can opt into the experimental Generative AI
// semantic conventions without forcing a hard-cut migration on
// existing customers.
//
// Background: OpenTelemetry semantic conventions live in two tracks:
// stable (rarely-revised, breaking-change-protected) and incubating
// (often-revised, breaking-change-allowed). GenAI conventions are
// currently incubating. Customers running a strict OTel Collector
// reject incubating-attribute names by default; enterprises with
// regulatory pipelines need a deliberate opt-in.
//
// The env var OTEL_SEMCONV_STABILITY_OPT_IN can hold a
// comma-separated list of opt-in tokens. We support:
//
//   - "gen_ai/dup", emit BOTH stable and incubating names
//   - "gen_ai"    , emit only the incubating names
//   - any other value or empty, emit only stable names
//
// This package is callable from any other package; importing it has
// no side effects beyond cheap env reads on first use.
package otel

import (
	"os"
	"strings"
	"sync"
)

// SemConvMode encodes the customer's chosen tradeoff between
// breakage and forward compatibility. The zero value (ModeStable)
// is what every customer gets without setting the env var.
type SemConvMode int

const (
	// ModeStable emits only OpenTelemetry stable-tier attribute
	// names. Default; safe for strict OTel Collector deployments.
	ModeStable SemConvMode = iota
	// ModeGenAIDup emits both stable and incubating GenAI names
	// for every attribute. Used during migration windows so
	// downstream consumers can switch over without timing.
	ModeGenAIDup
	// ModeGenAI emits only the incubating GenAI names. Use when
	// the downstream pipeline already accepts the new names.
	ModeGenAI
)

const stabilityEnvVar = "OTEL_SEMCONV_STABILITY_OPT_IN"

var (
	cachedMode SemConvMode
	cacheOnce  sync.Once
)

// SemConv returns the current effective semconv mode based on the
// process environment. The lookup is memoized; in practice this
// function is called from emit-hot paths and should not re-read the
// env on each call.
//
// To pick up a runtime change to the env var, call ResetCache (test
// helpers only).
func SemConv() SemConvMode {
	cacheOnce.Do(func() {
		cachedMode = parseSemConvOptIn(os.Getenv(stabilityEnvVar))
	})
	return cachedMode
}

// ResetCache clears the memoized SemConv result so subsequent calls
// re-read the env var. Intended for tests that mutate the
// environment; production code should never need this.
func ResetCache() {
	cacheOnce = sync.Once{}
}

// parseSemConvOptIn turns the raw env-var value into the typed mode.
// Accepts comma-separated tokens (any whitespace) so customers can
// also opt in to non-GenAI stability tracks if/when OTel adds them.
// "gen_ai/dup" is checked first because it implies "gen_ai" too.
func parseSemConvOptIn(raw string) SemConvMode {
	if raw == "" {
		return ModeStable
	}
	tokens := map[string]bool{}
	for _, t := range strings.Split(raw, ",") {
		t = strings.TrimSpace(t)
		if t != "" {
			tokens[t] = true
		}
	}
	switch {
	case tokens["gen_ai/dup"]:
		return ModeGenAIDup
	case tokens["gen_ai"]:
		return ModeGenAI
	default:
		return ModeStable
	}
}

// EmitIncubating returns true when the current mode requires
// emitting incubating attribute names. Convenience predicate so
// hot-path callers don't have to compare mode values manually.
func (m SemConvMode) EmitIncubating() bool {
	return m == ModeGenAI || m == ModeGenAIDup
}

// EmitStable returns true when the current mode requires emitting
// stable attribute names.
func (m SemConvMode) EmitStable() bool {
	return m == ModeStable || m == ModeGenAIDup
}
