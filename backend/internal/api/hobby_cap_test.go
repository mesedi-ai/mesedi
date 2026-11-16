package api

import "testing"

// A stored cap of 0 means the published $200 default, never no cap.
// Until 2026-11-16 the billing scheduler read 0 as uncapped (the outside review of 2026-11-15).
func TestEffectiveHobbyCapDefaultsTo200(t *testing.T) {
	cases := map[float64]float64{0: 200, -5: 200, 50: 50, 1000: 1000}
	for stored, want := range cases {
		if got := effectiveHobbyCapUSD(stored); got != want {
			t.Errorf("effectiveHobbyCapUSD(%v) = %v, want %v", stored, got, want)
		}
	}
	if HobbyDefaultBillingCapUSD != 200 {
		t.Errorf("HobbyDefaultBillingCapUSD = %v; the pricing page promises $200", HobbyDefaultBillingCapUSD)
	}
}
