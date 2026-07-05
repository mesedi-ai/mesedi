package api

import (
	"strings"
	"testing"
)

func Test_SanitizeSignaturePart_HappyPath(t *testing.T) {
	cases := []struct{ in, want string }{
		{"RuntimeError", "RuntimeError"},
		{"  ConnectionError  ", "ConnectionError"}, // trims whitespace
		{"schema_check", "schema_check"},
		{"validator.foo", "validator.foo"},
	}
	for _, tc := range cases {
		got := sanitizeSignaturePart(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeSignaturePart(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func Test_SanitizeSignaturePart_StripsControlChars(t *testing.T) {
	cases := []struct{ in, want string }{
		{"Foo\x00Bar", "FooBar"},
		{"with\ttab", "withtab"},
		{"with\nnewline", "withnewline"},
	}
	for _, tc := range cases {
		got := sanitizeSignaturePart(tc.in)
		if got != tc.want {
			t.Errorf("sanitizeSignaturePart(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func Test_SanitizeSignaturePart_EmptyInputs(t *testing.T) {
	for _, in := range []string{"", "   ", "\t\n"} {
		if got := sanitizeSignaturePart(in); got != "" {
			t.Errorf("sanitizeSignaturePart(%q) = %q, want \"\"", in, got)
		}
	}
}

func Test_SanitizeSignaturePart_CapsLength(t *testing.T) {
	long := strings.Repeat("a", signaturePartMaxLen+50)
	got := sanitizeSignaturePart(long)
	if len(got) != signaturePartMaxLen {
		t.Errorf("sanitizeSignaturePart cap broken: len=%d, want %d",
			len(got), signaturePartMaxLen)
	}
}

func Test_ToolFailureSignature_BackwardCompat(t *testing.T) {
	// Legacy tool_call event with no exception_type captured → bare
	// tool name (preserves existing failure_group clusters).
	if got := toolFailureSignature("my_search_tool", ""); got != "my_search_tool" {
		t.Errorf("bare-name fallback broken: got %q, want %q",
			got, "my_search_tool")
	}
}

func Test_ToolFailureSignature_Granular(t *testing.T) {
	cases := []struct{ tool, exc, want string }{
		{"my_search_tool", "ConnectionError", "my_search_tool:ConnectionError"},
		{"flaky_api", "RuntimeError", "flaky_api:RuntimeError"},
		{"db_tool", "  ValueError  ", "db_tool:ValueError"}, // qualifier trimmed
	}
	for _, tc := range cases {
		got := toolFailureSignature(tc.tool, tc.exc)
		if got != tc.want {
			t.Errorf("toolFailureSignature(%q, %q) = %q, want %q",
				tc.tool, tc.exc, got, tc.want)
		}
	}
}

func Test_ToolFailureSignature_EmptyToolName(t *testing.T) {
	// Defensive: empty tool name returns empty signature (handler
	// gates on "" before calling GroupToolFailure, so this is
	// double-defense).
	if got := toolFailureSignature("", "RuntimeError"); got != "" {
		t.Errorf("empty-tool path broken: got %q, want \"\"", got)
	}
}

func Test_ValidatorFailureSignature_BackwardCompat(t *testing.T) {
	// Customer not supplying category → bare validator name.
	// Critical for backward compat: existing customers landing
	// failures under "<name>" must keep doing so.
	if got := validatorFailureSignature("quality_check", ""); got != "quality_check" {
		t.Errorf("bare-name fallback broken: got %q, want %q",
			got, "quality_check")
	}
}

func Test_ValidatorFailureSignature_Granular(t *testing.T) {
	cases := []struct{ name, cat, want string }{
		{"quality_check", "schema_mismatch", "quality_check:schema_mismatch"},
		{"factuality", "hallucination", "factuality:hallucination"},
		{"output_schema", "  missing_field  ", "output_schema:missing_field"},
	}
	for _, tc := range cases {
		got := validatorFailureSignature(tc.name, tc.cat)
		if got != tc.want {
			t.Errorf("validatorFailureSignature(%q, %q) = %q, want %q",
				tc.name, tc.cat, got, tc.want)
		}
	}
}

func Test_ValidatorFailureSignature_ControlCharsInCategory(t *testing.T) {
	// A category with a control character should be sanitized OR
	// the qualifier should be empty enough to drop. Either way
	// the composite signature should not contain the control char.
	got := validatorFailureSignature("quality_check", "schema\x00mismatch")
	want := "quality_check:schemamismatch"
	if got != want {
		t.Errorf("control-char-stripping broken: got %q, want %q", got, want)
	}
}
