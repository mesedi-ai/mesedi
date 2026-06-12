// Tests for the Anthropic Admin Cost Report client (#198).
//
// Coverage:
//   - parseCostUSD handles the documented shape (cents-as-string)
//     correctly, the legacy float-dollars fallback, and zero/empty.
//   - AdminClient.Configured returns false on empty key, true on
//     non-empty key.
//   - GetCostReport returns ErrAdminDisabled when no key is set.
package anthropic

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

func Test_parseCostUSD_CentsString(t *testing.T) {
	cases := []struct {
		name      string
		amount    string
		amountStr string
		costFloat float64
		wantUSD   float64
	}{
		{
			name:    "150 cents = $1.50",
			amount:  "150",
			wantUSD: 1.50,
		},
		{
			name:    "1 cent = $0.01",
			amount:  "1",
			wantUSD: 0.01,
		},
		{
			name:    "0 = $0.00",
			amount:  "0",
			wantUSD: 0.0,
		},
		{
			name:    "decimal cents 12.5 = $0.125",
			amount:  "12.5",
			wantUSD: 0.125,
		},
		{
			name:      "amount_str fallback when amount empty",
			amount:    "",
			amountStr: "299",
			wantUSD:   2.99,
		},
		{
			name:      "legacy cost float (already dollars)",
			amount:    "",
			amountStr: "",
			costFloat: 4.20,
			wantUSD:   4.20,
		},
		{
			name:    "all-empty defaults to zero",
			amount:  "",
			wantUSD: 0.0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseCostUSD(tc.amount, tc.amountStr, tc.costFloat)
			if err != nil {
				t.Fatalf("parseCostUSD: unexpected error: %v", err)
			}
			if math.Abs(got-tc.wantUSD) > 1e-9 {
				t.Errorf("got %.6f, want %.6f", got, tc.wantUSD)
			}
		})
	}
}

func Test_parseCostUSD_InvalidString(t *testing.T) {
	_, err := parseCostUSD("not-a-number", "", 0)
	if err == nil {
		t.Fatal("expected error for invalid amount string")
	}
}

func Test_AdminClient_Configured(t *testing.T) {
	empty := NewAdminClient("", nil)
	if empty.Configured() {
		t.Error("empty key should report Configured()=false")
	}
	withKey := NewAdminClient("sk-ant-admin-fake-test-only", nil)
	if !withKey.Configured() {
		t.Error("non-empty key should report Configured()=true")
	}
}

func Test_GetCostReport_DisabledWithoutKey(t *testing.T) {
	c := NewAdminClient("", nil)
	now := time.Now().UTC()
	_, err := c.GetCostReport(context.Background(), now.AddDate(0, 0, -7), now)
	if !errors.Is(err, ErrAdminDisabled) {
		t.Errorf("expected ErrAdminDisabled, got %v", err)
	}
}

func Test_GetCostReport_RejectsEndBeforeStart(t *testing.T) {
	c := NewAdminClient("sk-ant-admin-fake-test-only", nil)
	now := time.Now().UTC()
	// Intentionally swapped: end before start.
	_, err := c.GetCostReport(context.Background(), now, now.AddDate(0, 0, -1))
	if err == nil {
		t.Fatal("expected error for ending_at before starting_at")
	}
}
