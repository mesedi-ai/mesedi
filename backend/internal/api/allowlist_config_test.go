package api

// Unit tests for the Allowlist.a validation helpers + handler
// guards. Covers:
//   - sanitizeAllowlistKey accepts valid input + trims whitespace.
//   - sanitizeAllowlistKey rejects empty / oversized / control-char.
//   - newAllowlistID format invariants.
//   - allowlistDetectors allow-list matches the 3 in-scope detectors.

import (
	"os"
	"strings"
	"testing"
)

func Test_SanitizeAllowlistKey_HappyPath(t *testing.T) {
	cases := []struct {
		in, want string
	}{
		{"ValueError", "ValueError"},
		{"  RuntimeError  ", "RuntimeError"}, // trims whitespace
		{"my_search_tool", "my_search_tool"},
		{"validator.schema_check", "validator.schema_check"},
	}
	for _, tc := range cases {
		got, msg := sanitizeAllowlistKey(tc.in)
		if msg != "" {
			t.Errorf("sanitizeAllowlistKey(%q) unexpected error: %q", tc.in, msg)
			continue
		}
		if got != tc.want {
			t.Errorf("sanitizeAllowlistKey(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func Test_SanitizeAllowlistKey_RejectsEmpty(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		if _, msg := sanitizeAllowlistKey(in); msg == "" {
			t.Errorf("sanitizeAllowlistKey(%q) should have rejected empty input", in)
		}
	}
}

func Test_SanitizeAllowlistKey_RejectsOversized(t *testing.T) {
	long := strings.Repeat("a", projectAllowlistKeyMaxLen+1)
	_, msg := sanitizeAllowlistKey(long)
	if msg == "" {
		t.Errorf("sanitizeAllowlistKey should have rejected %d-char input", len(long))
	}
	if !strings.Contains(msg, "200") {
		t.Errorf("error message should reference 200-char cap, got %q", msg)
	}
	// Exactly at the cap should pass.
	atCap := strings.Repeat("a", projectAllowlistKeyMaxLen)
	if _, msg := sanitizeAllowlistKey(atCap); msg != "" {
		t.Errorf("sanitizeAllowlistKey(%d-char) should have accepted at-cap input: %q",
			projectAllowlistKeyMaxLen, msg)
	}
}

func Test_SanitizeAllowlistKey_RejectsControlChars(t *testing.T) {
	cases := []string{
		"Value\x00Error",
		"tool\twith\ttabs",
		"name\nwith\nnewline",
	}
	for _, in := range cases {
		if _, msg := sanitizeAllowlistKey(in); msg == "" {
			t.Errorf("sanitizeAllowlistKey(%q) should have rejected control characters", in)
		}
	}
}

func Test_NewAllowlistID_Format(t *testing.T) {
	for i := 0; i < 20; i++ {
		id := newAllowlistID()
		if !strings.HasPrefix(id, "allow_") {
			t.Errorf("ID %q missing allow_ prefix", id)
		}
		if len(id) != len("allow_")+32 {
			t.Errorf("ID %q length = %d, want %d", id, len(id), len("allow_")+32)
		}
	}
}

func Test_NewAllowlistID_Uniqueness(t *testing.T) {
	// 100 IDs must all be unique (128 bits of entropy = vanishingly
	// small collision probability).
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		id := newAllowlistID()
		if seen[id] {
			t.Errorf("duplicate ID generated: %q", id)
		}
		seen[id] = true
	}
}

func Test_AllowlistDetectors_ExactSetExpected(t *testing.T) {
	want := []string{"crashes", "tool_failures", "validator_failures"}
	for _, d := range want {
		if !allowlistDetectors[d] {
			t.Errorf("allowlistDetectors missing expected detector %q", d)
		}
	}
	if got := len(allowlistDetectors); got != len(want) {
		t.Errorf("allowlistDetectors has %d entries, want %d", got, len(want))
	}
	// Negative cases, detectors that should NOT be in the allow-list.
	for _, d := range []string{
		"prompt_injection", "data_leakage", "sandbox_escape",
		"semantic_loop", "token_waste", "drift",
	} {
		if allowlistDetectors[d] {
			t.Errorf("allowlistDetectors should NOT include %q", d)
		}
	}
}

func Test_ProjectAllowlistMax_MatchesPatternConfig(t *testing.T) {
	// Sanity: cap should match projectPatternMax, same shape +
	// same rationale across the two -style primitives.
	if projectAllowlistMax != projectPatternMax {
		t.Errorf("projectAllowlistMax (%d) != projectPatternMax (%d); "+
			"should match for consistency", projectAllowlistMax, projectPatternMax)
	}
}

// Test_AllowlistHelperWiredFromAllDetectors is the Allowlist.d
// regression guard for the wiring shape, if a future refactor
// accidentally drops the checkAllowlistAndMaybeSkip call from one
// of the three consuming detector hot paths in handlers.go, this
// test fires.
//
// We rely on a source-level count rather than a runtime probe
// because the wiring is a hand-written call site rather than a
// registry, there's nothing to enumerate at runtime. Reading
// the source file at test time keeps the regression guard close
// to the file it's protecting (same package).
//
// At least 3 call sites are expected, one per consuming detector
// (crashes, tool_failures, validator_failures). A future fourth
// allowlist-supporting detector should add a fourth call site;
// the assertion is intentionally >= 3 rather than == 3 so the
// guard doesn't fire on intentional growth.
func Test_AllowlistHelperWiredFromAllDetectors(t *testing.T) {
	data, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatalf("read handlers.go: %v", err)
	}
	src := string(data)
	const callSite = "h.checkAllowlistAndMaybeSkip("
	count := strings.Count(src, callSite)
	if count < 3 {
		t.Errorf("%s wired in %d places in handlers.go, want >= 3 "+
			"(one per consuming detector: crashes, tool_failures, "+
			"validator_failures). A wiring may have been accidentally "+
			"dropped, see Allowlist.b.", callSite, count)
	}
}
