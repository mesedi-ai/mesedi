package api

// Unit tests for the pattern-config API surface.
// Covers (a) the validation helper that runs at POST/PATCH time
// and (b) the per-pattern-id generator's basic shape.
//
// Handler-level HTTP-roundtrip tests are out of scope for 2.1.a;
// they'll live in an integration test that exercises the full
// Store + auth + mux stack once the detector wiring lands in 2.1.b
// and there's something customer-visible to assert end-to-end.

import (
	"strings"
	"testing"
)

func TestValidatePatternFields_Happy(t *testing.T) {
	got := validatePatternFields(
		`(?i)ignore\s+(the\s+)?(previous|above)`,
		"medium",
		"Block prompt-injection ignore-previous patterns",
	)
	if got != "" {
		t.Errorf("expected no error for valid input; got: %q", got)
	}
}

func TestValidatePatternFields_EmptyPattern(t *testing.T) {
	got := validatePatternFields("", "high", "")
	if got != "pattern is required" {
		t.Errorf("expected 'pattern is required'; got %q", got)
	}
	got = validatePatternFields("   ", "high", "")
	if got != "pattern is required" {
		t.Errorf("whitespace-only pattern should be rejected; got %q", got)
	}
}

func TestValidatePatternFields_TooLong(t *testing.T) {
	// 1001 chars is one over the cap.
	long := strings.Repeat("a", projectPatternMaxLen+1)
	got := validatePatternFields(long, "low", "")
	if got != "pattern is too long (max 1000 chars)" {
		t.Errorf("expected pattern-too-long; got %q", got)
	}
}

func TestValidatePatternFields_InvalidRegex(t *testing.T) {
	// Unclosed character class: invalid RE2.
	got := validatePatternFields(`[abc`, "medium", "")
	if !strings.HasPrefix(got, "pattern is not a valid RE2 regex") {
		t.Errorf("expected RE2-invalid rejection; got %q", got)
	}
	// Catastrophic-backtracking PCRE features RE2 explicitly rejects.
	got = validatePatternFields(`(a+)+b`, "medium", "")
	if got != "" {
		// Note: (a+)+b IS valid RE2; the rejection is at the
		// "catastrophic backtracking" PCRE level. RE2 is
		// catastrophe-free by construction. So this pattern
		// COMPILES; we keep the assertion permissive — the test
		// documents that RE2 doesn't reject nested quantifiers
		// the way PCRE does.
		t.Logf("RE2 accepts (a+)+b — catastrophe-free by construction; "+
			"validator output: %q", got)
	}
}

func TestValidatePatternFields_BadSeverity(t *testing.T) {
	for _, bad := range []string{"", "critical", "LOW", "Medium", "INFO"} {
		got := validatePatternFields(`x`, bad, "")
		if got != "severity must be one of: low, medium, high" {
			t.Errorf("severity %q should be rejected; got %q", bad, got)
		}
	}
}

func TestValidatePatternFields_GoodSeverities(t *testing.T) {
	for _, sev := range []string{"low", "medium", "high"} {
		got := validatePatternFields(`x`, sev, "")
		if got != "" {
			t.Errorf("severity %q should be accepted; got %q", sev, got)
		}
	}
}

func TestValidatePatternFields_DescriptionTooLong(t *testing.T) {
	long := strings.Repeat("a", projectPatternMaxDescLen+1)
	got := validatePatternFields(`x`, "low", long)
	if got != "description is too long (max 500 chars)" {
		t.Errorf("expected description-too-long; got %q", got)
	}
}

func TestNewPatternID_ShapeAndUniqueness(t *testing.T) {
	id1 := newPatternID()
	id2 := newPatternID()
	if !strings.HasPrefix(id1, "ppat-") {
		t.Errorf("expected 'ppat-' prefix; got %q", id1)
	}
	// 12 bytes -> 24 hex chars + 5 chars prefix = 29 char ID.
	if len(id1) != 29 {
		t.Errorf("expected length 29; got %d for %q", len(id1), id1)
	}
	if id1 == id2 {
		t.Errorf("two consecutive newPatternID() calls returned the same id: %q", id1)
	}
}

func TestPatternConfigDetectors_AllowList(t *testing.T) {
	// Pin the allow-list — adding or removing a detector here is
	// a product decision and the test fails the build until the
	// reviewer reasons about the change.
	want := []string{"prompt_injection", "data_leakage", "sandbox_escape"}
	if len(patternConfigDetectors) != len(want) {
		t.Errorf("allow-list size changed: want %d, got %d",
			len(want), len(patternConfigDetectors))
	}
	for _, d := range want {
		if !patternConfigDetectors[d] {
			t.Errorf("detector %q missing from allow-list", d)
		}
	}
}
