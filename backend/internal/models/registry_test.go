// Tests for the static model registry. The most important test in
// this file is Test_RegistryParity: it fails the build whenever
// windowByModel and providerByModel get out of sync (i.e. someone
// added a new model to one map but forgot the other). The cost of
// this slip in production is silent: ContextWindow returns ok=true
// but Provider() returns "", which breaks the provider_incident
// cross-tenant correlation logic.
package models

import (
	"sort"
	"strings"
	"testing"
)

func Test_ContextWindow_Known(t *testing.T) {
	cases := []struct {
		model string
		want  int
	}{
		{"claude-sonnet-4-6", 200_000},
		{"claude-haiku-4-5-20251001", 200_000},
		{"gpt-5", 400_000},
		{"gpt-4.1", 1_000_000},
		{"gemini-2.5-pro", 2_000_000},
		{"llama-4-scout", 10_000_000},
		{"mistral-large-2", 128_000},
	}
	for _, tc := range cases {
		t.Run(tc.model, func(t *testing.T) {
			got, ok := ContextWindow(tc.model)
			if !ok {
				t.Fatalf("ContextWindow(%q): ok=false, want true", tc.model)
			}
			if got != tc.want {
				t.Errorf("ContextWindow(%q) = %d, want %d", tc.model, got, tc.want)
			}
		})
	}
}

func Test_ContextWindow_Unknown(t *testing.T) {
	// Unknown / hypothetical / typo'd model IDs must return ok=false
	// so detectors skip per-model checks rather than over-fire.
	cases := []string{
		"",
		"claude-3-opus-typo",
		"gpt-99",
		"gemini-3.0-ultra",
		"some-internal-finetune-v1",
	}
	for _, model := range cases {
		t.Run(model, func(t *testing.T) {
			if _, ok := ContextWindow(model); ok {
				t.Errorf("ContextWindow(%q): ok=true, want false (unknown model should not match)", model)
			}
		})
	}
}

func Test_Provider_Known(t *testing.T) {
	cases := map[string]string{
		"claude-sonnet-4-6": "anthropic",
		"gpt-5":             "openai",
		"gemini-2.5-pro":    "google",
		"llama-4-scout":     "meta",
		"mistral-large-2":   "mistral",
	}
	for model, want := range cases {
		if got := Provider(model); got != want {
			t.Errorf("Provider(%q) = %q, want %q", model, got, want)
		}
	}
}

func Test_Provider_Unknown(t *testing.T) {
	if got := Provider("definitely-not-a-real-model"); got != "" {
		t.Errorf("Provider(unknown) = %q, want \"\"", got)
	}
}

func Test_IsKnown(t *testing.T) {
	if !IsKnown("claude-opus-4-6") {
		t.Errorf("IsKnown(claude-opus-4-6) = false, want true")
	}
	if IsKnown("not-a-model") {
		t.Errorf("IsKnown(not-a-model) = true, want false")
	}
}

// Test_RegistryParity is the most important test in this file: every
// entry in windowByModel MUST have a matching entry in providerByModel
// (and vice-versa). Without this guard, it's trivially easy to add a
// new model to one map and forget the other, silently breaking the
// provider_incident detector which relies on Provider() returning
// non-empty for all known models.
func Test_RegistryParity(t *testing.T) {
	var missingProvider []string
	for model := range windowByModel {
		if _, ok := providerByModel[model]; !ok {
			missingProvider = append(missingProvider, model)
		}
	}
	if len(missingProvider) > 0 {
		sort.Strings(missingProvider)
		t.Errorf(
			"%d model(s) in windowByModel have no providerByModel entry:\n  - %s",
			len(missingProvider),
			strings.Join(missingProvider, "\n  - "),
		)
	}

	var missingWindow []string
	for model := range providerByModel {
		if _, ok := windowByModel[model]; !ok {
			missingWindow = append(missingWindow, model)
		}
	}
	if len(missingWindow) > 0 {
		sort.Strings(missingWindow)
		t.Errorf(
			"%d model(s) in providerByModel have no windowByModel entry:\n  - %s",
			len(missingWindow),
			strings.Join(missingWindow, "\n  - "),
		)
	}
}

// Test_RegistryValuesSane catches obvious typos: every window must be
// a positive integer, and every provider must be a lowercase token
// (since downstream code may use them as labels / map keys without
// further normalization).
func Test_RegistryValuesSane(t *testing.T) {
	for model, w := range windowByModel {
		if w <= 0 {
			t.Errorf("windowByModel[%q] = %d, want > 0", model, w)
		}
	}
	for model, p := range providerByModel {
		if p == "" {
			t.Errorf("providerByModel[%q] is empty", model)
		}
		if p != strings.ToLower(p) {
			t.Errorf("providerByModel[%q] = %q, want lowercase", model, p)
		}
	}
}
