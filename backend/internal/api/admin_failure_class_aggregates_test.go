// Tests for the period helpers used by the failure_class aggregate
// surface (#212).
//
// Coverage:
//   - validPeriod accepts well-formed YYYY-MM and rejects garbage.
//   - monthBounds returns the correct first-instant + first-instant-of-next-month.
//   - DefaultKAnonymityThreshold stays at 3 (constant we ship).
package api

import (
	"testing"
	"time"
)

func Test_validPeriod_AcceptsGood(t *testing.T) {
	good := []string{"2026-01", "2026-06", "2026-12", "2025-09", "1999-01"}
	for _, p := range good {
		if !validPeriod(p) {
			t.Errorf("validPeriod(%q) = false, want true", p)
		}
	}
}

func Test_validPeriod_RejectsBad(t *testing.T) {
	bad := []string{
		"",
		"2026",
		"2026-13",      // month out of range
		"2026-00",      // month out of range
		"2026/06",      // wrong delimiter
		"26-06",        // 2-digit year
		"2026-6",       // 1-digit month
		"abcd-ef",      // non-numeric
		"2026-06-15",   // too many parts
		" 2026-06 ",    // leading/trailing space
	}
	for _, p := range bad {
		if validPeriod(p) {
			t.Errorf("validPeriod(%q) = true, want false", p)
		}
	}
}

func Test_monthBounds_June2026(t *testing.T) {
	start, end, err := monthBounds("2026-06")
	if err != nil {
		t.Fatalf("monthBounds error: %v", err)
	}
	wantStart := time.Date(2026, 6, 1, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2026, 7, 1, 0, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) {
		t.Errorf("start = %v, want %v", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("end = %v, want %v", end, wantEnd)
	}
}

func Test_monthBounds_December2026(t *testing.T) {
	// December rolls over the year; this is the boundary worth
	// checking explicitly.
	start, end, err := monthBounds("2026-12")
	if err != nil {
		t.Fatalf("monthBounds error: %v", err)
	}
	wantStart := time.Date(2026, 12, 1, 0, 0, 0, 0, time.UTC)
	wantEnd := time.Date(2027, 1, 1, 0, 0, 0, 0, time.UTC)
	if !start.Equal(wantStart) {
		t.Errorf("start = %v, want %v", start, wantStart)
	}
	if !end.Equal(wantEnd) {
		t.Errorf("end = %v, want %v", end, wantEnd)
	}
}

func Test_DefaultKAnonymityThreshold(t *testing.T) {
	// This constant gets surfaced as the default ?k= value on the
	// admin endpoint. If we ever change it (e.g., raise to 5 after
	// the customer base grows), the change should be deliberate,
	// not accidental. Pin the current value.
	if DefaultKAnonymityThreshold != 3 {
		t.Errorf("DefaultKAnonymityThreshold = %d, want 3", DefaultKAnonymityThreshold)
	}
}
