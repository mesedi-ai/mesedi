// Tests for the OTEL_SEMCONV_STABILITY_OPT_IN parser. Coverage:
// every documented opt-in token resolves to the right mode, the
// EmitIncubating / EmitStable predicates match the mode, and unknown
// tokens fall back to the safe default (stable-only).
package otel

import "testing"

func Test_ParseSemConvOptIn(t *testing.T) {
	cases := []struct {
		raw  string
		want SemConvMode
	}{
		{"", ModeStable},
		{"   ", ModeStable},
		{"gen_ai", ModeGenAI},
		{"gen_ai/dup", ModeGenAIDup},
		{"gen_ai/dup,gen_ai", ModeGenAIDup}, // dup wins
		{"gen_ai , other_track", ModeGenAI}, // whitespace tolerated
		{"unknown_token", ModeStable},
	}
	for _, tc := range cases {
		t.Run(tc.raw, func(t *testing.T) {
			if got := parseSemConvOptIn(tc.raw); got != tc.want {
				t.Errorf("parseSemConvOptIn(%q) = %d, want %d", tc.raw, got, tc.want)
			}
		})
	}
}

func Test_SemConvMode_EmitPredicates(t *testing.T) {
	cases := []struct {
		mode           SemConvMode
		wantStable     bool
		wantIncubating bool
	}{
		{ModeStable, true, false},
		{ModeGenAI, false, true},
		{ModeGenAIDup, true, true},
	}
	for _, tc := range cases {
		if got := tc.mode.EmitStable(); got != tc.wantStable {
			t.Errorf("mode=%d EmitStable()=%v want %v", tc.mode, got, tc.wantStable)
		}
		if got := tc.mode.EmitIncubating(); got != tc.wantIncubating {
			t.Errorf("mode=%d EmitIncubating()=%v want %v", tc.mode, got, tc.wantIncubating)
		}
	}
}
