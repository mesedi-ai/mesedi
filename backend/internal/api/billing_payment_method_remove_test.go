package api

import "testing"

// TestClassifyRemovalSettlement pins the 3-way branch:
//   - pending * 100 rounds to <= 0 cents → rounded_to_zero
//   - 1 <= cents < stripeMinChargePaymentIntentCents → waived_below_stripe_min
//   - cents >= stripeMinChargePaymentIntentCents → charge
//
// The waived-below-min case is the fix for the F2 harness finding:
// a Team customer with 1-499 units of accumulated overage
// ($0.001-$0.499) previously could not remove their card because
// Stripe refuses PaymentIntents below $0.50 USD with the error code
// `amount_too_small`. classifyRemovalSettlement now routes those
// customers to the write-off branch so the detach still succeeds.
func TestClassifyRemovalSettlement(t *testing.T) {
	cases := []struct {
		name           string
		pendingUSD     float64
		wantCents      int64
		wantSettlement removalSettlement
	}{
		{
			name:           "zero pending → rounded_to_zero",
			pendingUSD:     0.0,
			wantCents:      0,
			wantSettlement: settleRoundedToZero,
		},
		{
			name:           "quarter-cent pending → rounded_to_zero",
			pendingUSD:     0.0025,
			wantCents:      0,
			wantSettlement: settleRoundedToZero,
		},
		{
			name:           "exactly one cent → waived (below $0.50 min)",
			pendingUSD:     0.01,
			wantCents:      1,
			wantSettlement: settleWaivedBelowMin,
		},
		{
			name:           "F2 fact pattern (5 Team overage × $0.001 = $0.005 → 1 cent) → waived",
			pendingUSD:     0.005,
			wantCents:      1,
			wantSettlement: settleWaivedBelowMin,
		},
		{
			name:           "49 cents (just below Stripe min) → waived",
			pendingUSD:     0.49,
			wantCents:      49,
			wantSettlement: settleWaivedBelowMin,
		},
		{
			name:           "exactly 50 cents (Stripe min) → charge",
			pendingUSD:     0.50,
			wantCents:      50,
			wantSettlement: settleCharge,
		},
		{
			name:           "$1.00 → charge",
			pendingUSD:     1.0,
			wantCents:      100,
			wantSettlement: settleCharge,
		},
		{
			name:           "large Hobby-scale overage → charge",
			pendingUSD:     47.83,
			wantCents:      4783,
			wantSettlement: settleCharge,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			gotCents, gotSettlement := classifyRemovalSettlement(tc.pendingUSD)
			if gotCents != tc.wantCents {
				t.Errorf("cents: want %d, got %d", tc.wantCents, gotCents)
			}
			if gotSettlement != tc.wantSettlement {
				t.Errorf("settlement: want %q, got %q", tc.wantSettlement, gotSettlement)
			}
		})
	}
}

// TestStripeMinChargeConstant pins the constant to Stripe's
// documented minimum for USD PaymentIntents. If Stripe ever changes
// the minimum (or if someone renames/removes the constant), this
// test fails loudly rather than the harness silently regressing.
func TestStripeMinChargeConstant(t *testing.T) {
	const wantCents = int64(50)
	if stripeMinChargePaymentIntentCents != wantCents {
		t.Fatalf("stripeMinChargePaymentIntentCents drifted from Stripe's documented $0.50 USD minimum (got %d cents, want %d)",
			stripeMinChargePaymentIntentCents, wantCents)
	}
}
