// Unit tests for data_leakage detector's per-project severity-policy
// knob (Wave data_leakage.G5).
//
// The detector itself has no stand-alone scan function in this
// package — the dlp/ package handles scanning. data_leakage.go
// exposes only the per-project severity-policy knob that decides
// WHICH severities promote dlp_scan_result hits into failure_groups.
// These tests pin the validation + fallback behavior so accidental
// regressions to "accept any severity string" or "silently drop
// invalid entries" surface immediately.
package detectors

import (
	"reflect"
	"sort"
	"testing"
)

func Test_DefaultDataLeakageThresholds_LocksDocumentedDefault(t *testing.T) {
	d := DefaultDataLeakageThresholds()
	want := []string{"critical", "high"}
	if !reflect.DeepEqual(d.AllowedSeverities, want) {
		t.Errorf("DefaultDataLeakageThresholds.AllowedSeverities = %v, want %v", d.AllowedSeverities, want)
	}
}

func Test_EffectiveAllowedSeverities(t *testing.T) {
	defaults := DefaultDataLeakageThresholds().AllowedSeverities
	cases := []struct {
		name string
		in   []string
		want []string
	}{
		{"empty_slice_returns_default", []string{}, defaults},
		{"nil_returns_default", nil, defaults},
		{"valid_critical_only", []string{"critical"}, []string{"critical"}},
		{"valid_three_severities", []string{"critical", "high", "medium"}, []string{"critical", "high", "medium"}},
		{"unknown_severity_reverts_whole_slice", []string{"critical", "URGENT"}, defaults},
		{"typo_lowercase_severity_reverts", []string{"low"}, defaults}, // "low" is NOT in the closed set
		{"empty_string_entry_reverts", []string{"critical", ""}, defaults},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			thresh := DataLeakageThresholds{AllowedSeverities: tc.in}
			got := thresh.EffectiveAllowedSeverities()
			// Sort both before comparing for deterministic comparison
			// (the input slice may be returned by-value when valid).
			gotSorted := append([]string(nil), got...)
			wantSorted := append([]string(nil), tc.want...)
			sort.Strings(gotSorted)
			sort.Strings(wantSorted)
			if !reflect.DeepEqual(gotSorted, wantSorted) {
				t.Errorf("EffectiveAllowedSeverities(%v) = %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}

func Test_EffectiveAllowedSeverities_DoesNotPartialFilter(t *testing.T) {
	// The detector explicitly does NOT drop invalid entries and keep
	// valid ones — the customer's intent on a malformed config is
	// ambiguous, and the safe failure mode is to treat the whole
	// slice as default rather than a partial subset.
	in := []string{"critical", "high", "FAKE"}
	got := DataLeakageThresholds{AllowedSeverities: in}.EffectiveAllowedSeverities()
	want := DefaultDataLeakageThresholds().AllowedSeverities
	if !reflect.DeepEqual(got, want) {
		t.Errorf("partial-bad input should fall back to defaults, got %v, want %v", got, want)
	}
}
