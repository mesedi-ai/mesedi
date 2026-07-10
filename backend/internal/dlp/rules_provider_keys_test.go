package dlp

// Unit tests for the provider-API-key rules added to close
// data_leakage.G2: anthropic_api_key and gemini_api_key.
//
// The Cohere built-in is intentionally NOT included (40-char
// bare-alphanumeric format would false-positive on SHA-1 hex,
// git SHAs, Stripe IDs, etc). Cohere customers use the
// custom-pattern editor to add a context-aware regex.

import (
	"testing"
)

func newTestScanner(t *testing.T) *Scanner {
	t.Helper()
	s, err := NewScanner(BuiltinRules())
	if err != nil {
		t.Fatalf("NewScanner failed: %v", err)
	}
	return s
}

func Test_AnthropicAPIKey_ModernFormat(t *testing.T) {
	// Modern project-scoped sk-ant-api03- form. ~100 chars total.
	input := "leaked: sk-ant-api03-abcdef1234567890ABCDEFGHIJklmnopqrstuvwxyz0123456789ABCDEFGH-other text"
	hits := newTestScanner(t).Scan(input)
	if len(hits) == 0 {
		t.Fatalf("expected at least one hit for modern Anthropic key")
	}
	foundAnthropic := false
	for _, h := range hits {
		if h.RuleID == "anthropic_api_key" {
			foundAnthropic = true
			if h.Severity != SeverityCritical {
				t.Errorf("anthropic_api_key severity = %v, want Critical", h.Severity)
			}
		}
	}
	if !foundAnthropic {
		t.Errorf("anthropic_api_key did not fire; got hits = %+v", hits)
	}
}

func Test_AnthropicAPIKey_LegacyFormat(t *testing.T) {
	// Legacy sk-ant- form without the api03- segment.
	input := "old key: sk-ant-abcdef1234567890ABCDEFGH-more"
	hits := newTestScanner(t).Scan(input)
	foundAnthropic := false
	for _, h := range hits {
		if h.RuleID == "anthropic_api_key" {
			foundAnthropic = true
		}
	}
	if !foundAnthropic {
		t.Errorf("anthropic_api_key did not fire on legacy form; got hits = %+v", hits)
	}
}

func Test_AnthropicAPIKey_WinsOverOpenAIOnSameMatch(t *testing.T) {
	// The OpenAI pattern `\bsk-(?:proj-)?[A-Za-z0-9_-]{20,}\b`
	// would ALSO match an Anthropic key. mergeOverlapping must
	// keep the more-specific anthropic_api_key match.
	input := "sk-ant-api03-abcdef1234567890ABCDEFGHIJklmnopqrstuvwxyz"
	hits := newTestScanner(t).Scan(input)
	if len(hits) != 1 {
		t.Fatalf("expected 1 merged hit; got %d: %+v", len(hits), hits)
	}
	if hits[0].RuleID != "anthropic_api_key" {
		t.Errorf("merged hit rule = %q, want %q (anthropic must win over openai on overlap)",
			hits[0].RuleID, "anthropic_api_key")
	}
}

func Test_GeminiAPIKey_StandardFormat(t *testing.T) {
	// Google AI Studio / Gemini API keys: AIza + exactly 35 chars
	// = 39 chars total. The string below has exactly 35 chars
	// after the AIza prefix (counted: SyABCDEFGHIJKLMNOPQRSTUVWXYZ0123456 = 35).
	input := "key=AIzaSyABCDEFGHIJKLMNOPQRSTUVWXYZ0123456 more text"
	hits := newTestScanner(t).Scan(input)
	foundGemini := false
	for _, h := range hits {
		if h.RuleID == "gemini_api_key" {
			foundGemini = true
			if h.Severity != SeverityCritical {
				t.Errorf("gemini_api_key severity = %v, want Critical", h.Severity)
			}
		}
	}
	if !foundGemini {
		t.Errorf("gemini_api_key did not fire; got hits = %+v", hits)
	}
}

func Test_GeminiAPIKey_DoesNotFireOnShortAIza(t *testing.T) {
	// AIza with fewer than 35 trailing chars must NOT trip.
	input := "AIzaShortString12345"
	hits := newTestScanner(t).Scan(input)
	for _, h := range hits {
		if h.RuleID == "gemini_api_key" {
			t.Errorf("gemini_api_key false-positive on short string: %q", h.Match)
		}
	}
}

func Test_AnthropicAPIKey_DoesNotFireOnOpenAIKey(t *testing.T) {
	// An OpenAI key (sk- without ant-) must NOT match the
	// Anthropic rule.
	input := "sk-abcdef1234567890ABCDEFGHIJklmnopqrstuvwxyz"
	hits := newTestScanner(t).Scan(input)
	for _, h := range hits {
		if h.RuleID == "anthropic_api_key" {
			t.Errorf("anthropic_api_key false-positive on OpenAI key: %q", h.Match)
		}
	}
}

func Test_AnthropicAPIKey_DoesNotFireOnUnrelatedSkAnt(t *testing.T) {
	// "sk-ant-" must be a word-boundary prefix. Embedded inside
	// a longer alphanumeric token shouldn't fire.
	input := "xsk-ant-shortstring"
	hits := newTestScanner(t).Scan(input)
	for _, h := range hits {
		if h.RuleID == "anthropic_api_key" {
			t.Errorf("anthropic_api_key false-positive on non-word-boundary: %q", h.Match)
		}
	}
}
