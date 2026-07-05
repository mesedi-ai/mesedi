package models

// Unit tests for normalizeModelID + the routed-identifier
// resolution behavior of ContextWindow / Provider / IsKnown.
// Closes context_overflow.G2.

import "testing"

func Test_NormalizeModelID_BedrockAnthropic(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		// Modern Bedrock Anthropic identifiers.
		{"anthropic.claude-3-5-sonnet-20240620-v1:0", "claude-3-5-sonnet"},
		{"anthropic.claude-3-5-haiku-20241022-v1:0", "claude-3-5-haiku"},
		{"anthropic.claude-3-opus-20240229-v1:0", "claude-3-opus"},
		// Bedrock Cohere identifier — strips prefix + suffix.
		// (Will only register in registry once the no-suffix
		// canonical matches an existing entry.)
		{"cohere.command-r-plus-20240801-v1:0", "command-r-plus"},
	}
	for _, tc := range cases {
		got := normalizeModelID(tc.in)
		if got != tc.want {
			t.Errorf("normalizeModelID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func Test_NormalizeModelID_VertexGemini(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"gemini-1.5-pro-001", "gemini-1.5-pro"},
		{"gemini-1.5-flash-002", "gemini-1.5-flash"},
		{"gemini-2.5-pro-001", "gemini-2.5-pro"},
	}
	for _, tc := range cases {
		got := normalizeModelID(tc.in)
		if got != tc.want {
			t.Errorf("normalizeModelID(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func Test_NormalizeModelID_CanonicalPassthrough(t *testing.T) {
	// Canonical model IDs already in the registry must pass
	// through unchanged.
	canonicals := []string{
		"claude-3-5-sonnet",
		"gpt-4o",
		"gemini-1.5-pro",
		"command-r-plus",
		"llama-3.3-70b",
		"mistral-large-2",
	}
	for _, m := range canonicals {
		got := normalizeModelID(m)
		if got != m {
			t.Errorf("normalizeModelID(%q) = %q; canonical should pass through unchanged",
				m, got)
		}
	}
}

func Test_NormalizeModelID_AzureDeploymentUnchanged(t *testing.T) {
	// Azure deployment names are customer-chosen and not
	// deterministic; normalization is impossible without a
	// customer-supplied mapping. The function must leave them
	// alone so the registry lookup correctly returns ok=false
	// and context_overflow silently skips (correct behavior —
	// G3 per-project model-window override is the right fix).
	azures := []string{
		"my-prod-gpt4",
		"acme-chatbot-v2",
		"customer123_deployment",
	}
	for _, m := range azures {
		got := normalizeModelID(m)
		if got != m {
			t.Errorf("normalizeModelID(%q) = %q; Azure deployment names must pass through",
				m, got)
		}
	}
}

func Test_NormalizeModelID_EmptyInput(t *testing.T) {
	if got := normalizeModelID(""); got != "" {
		t.Errorf("normalizeModelID(\"\") = %q, want empty", got)
	}
}

func Test_ContextWindow_BedrockResolvesToCanonical(t *testing.T) {
	// End-to-end: Bedrock-routed Anthropic IDs must return the
	// same window as the canonical model. Closes the silent-
	// skip gap context_overflow.G2 was about.
	bedrockID := "anthropic.claude-3-5-sonnet-20240620-v1:0"
	got, ok := ContextWindow(bedrockID)
	if !ok {
		t.Fatalf("ContextWindow(%q) returned ok=false; expected to resolve via normalizer", bedrockID)
	}
	canonicalWindow, _ := ContextWindow("claude-3-5-sonnet")
	if got != canonicalWindow {
		t.Errorf("Bedrock window %d != canonical window %d", got, canonicalWindow)
	}
}

func Test_ContextWindow_VertexResolvesToCanonical(t *testing.T) {
	vertexID := "gemini-1.5-pro-001"
	got, ok := ContextWindow(vertexID)
	if !ok {
		t.Fatalf("ContextWindow(%q) returned ok=false; expected to resolve via normalizer", vertexID)
	}
	canonicalWindow, _ := ContextWindow("gemini-1.5-pro")
	if got != canonicalWindow {
		t.Errorf("Vertex window %d != canonical window %d", got, canonicalWindow)
	}
}

func Test_ContextWindow_CohereModelsRegistered(t *testing.T) {
	// Closes context_overflow.G1: Cohere chat models must be in
	// the registry so context_overflow can fire on Cohere
	// executions.
	cases := []struct {
		model  string
		window int
	}{
		{"command-r", 128_000},
		{"command-r-plus", 128_000},
		{"command-r-08-2024", 128_000},
		{"command-r-plus-08-2024", 128_000},
		{"command-light", 4_096},
		{"command", 4_096},
	}
	for _, tc := range cases {
		got, ok := ContextWindow(tc.model)
		if !ok {
			t.Errorf("ContextWindow(%q) returned ok=false; Cohere model missing from registry", tc.model)
			continue
		}
		if got != tc.window {
			t.Errorf("ContextWindow(%q) = %d, want %d", tc.model, got, tc.window)
		}
	}
}

func Test_Provider_CohereModelsCorrectlyTagged(t *testing.T) {
	cohereModels := []string{
		"command-r",
		"command-r-plus",
		"command-r-08-2024",
		"command-r-plus-08-2024",
		"command-light",
		"command",
	}
	for _, m := range cohereModels {
		if got := Provider(m); got != "cohere" {
			t.Errorf("Provider(%q) = %q, want %q", m, got, "cohere")
		}
	}
}

func Test_Provider_BedrockAnthropicResolvesProvider(t *testing.T) {
	// Bedrock-routed Anthropic must report "anthropic" — the
	// provider_incident detector keys on Provider() to cluster
	// cross-tenant signals.
	if got := Provider("anthropic.claude-3-5-sonnet-20240620-v1:0"); got != "anthropic" {
		t.Errorf("Provider(bedrock-anthropic) = %q, want %q", got, "anthropic")
	}
	if got := Provider("gemini-1.5-pro-001"); got != "google" {
		t.Errorf("Provider(vertex-gemini) = %q, want %q", got, "google")
	}
}
