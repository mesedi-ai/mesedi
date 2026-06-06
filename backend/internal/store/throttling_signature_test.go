// Tests for ThrottlingSignature, the pure function that assembles a
// cluster signature for an infrastructure_throttled failure_group
// from the (reason, provider, dimension, circuit_state) tuple
// captured on an InfrastructureEventPayload.
//
// Coverage targets:
//   - Each documented reason ("rate_limit", "circuit_breaker",
//     "quota_exhausted") emits the expected stable shape.
//   - Empty provider falls back to "unknown" so signatures stay
//     groupable instead of exploding one-per-execution.
//   - Empty optional fields collapse correctly (rate_limit without a
//     dimension is still a valid signature, circuit_breaker without
//     a circuit_state defaults to "open").
//   - Unknown reasons cluster by reason+provider so we get safe
//     forward-compat behavior when new sub-cases land in the SDK
//     before the detector knows about them.
package store

import "testing"

func Test_ThrottlingSignature_RateLimit(t *testing.T) {
	cases := []struct {
		name      string
		provider  string
		dimension string
		want      string
	}{
		{
			name:      "with dimension",
			provider:  "anthropic",
			dimension: "tokens_per_minute",
			want:      "rate_limit:anthropic:tokens_per_minute",
		},
		{
			name:      "without dimension",
			provider:  "openai",
			dimension: "",
			want:      "rate_limit:openai",
		},
		{
			name:      "empty provider falls back to unknown",
			provider:  "",
			dimension: "requests_per_second",
			want:      "rate_limit:unknown:requests_per_second",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ThrottlingSignature("rate_limit", tc.provider, tc.dimension, "")
			if got != tc.want {
				t.Errorf("ThrottlingSignature(rate_limit, %q, %q, \"\") = %q, want %q",
					tc.provider, tc.dimension, got, tc.want)
			}
		})
	}
}

func Test_ThrottlingSignature_CircuitBreaker(t *testing.T) {
	cases := []struct {
		name         string
		provider     string
		circuitState string
		want         string
	}{
		{
			name:         "open trip",
			provider:     "anthropic",
			circuitState: "open",
			want:         "circuit_breaker:anthropic:open",
		},
		{
			name:         "half_open re-test",
			provider:     "openai",
			circuitState: "half_open",
			want:         "circuit_breaker:openai:half_open",
		},
		{
			name:         "empty circuit state defaults to open",
			provider:     "google",
			circuitState: "",
			want:         "circuit_breaker:google:open",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := ThrottlingSignature("circuit_breaker", tc.provider, "", tc.circuitState)
			if got != tc.want {
				t.Errorf("ThrottlingSignature(circuit_breaker, %q, \"\", %q) = %q, want %q",
					tc.provider, tc.circuitState, got, tc.want)
			}
		})
	}
}

func Test_ThrottlingSignature_QuotaExhausted(t *testing.T) {
	got := ThrottlingSignature("quota_exhausted", "anthropic", "tokens_per_month", "")
	want := "quota_exhausted:anthropic"
	if got != want {
		t.Errorf("ThrottlingSignature(quota_exhausted, anthropic, ...) = %q, want %q (dimension should be ignored)", got, want)
	}
}

func Test_ThrottlingSignature_UnknownReason(t *testing.T) {
	// Forward-compat: an SDK that emits a new reason should still
	// produce a stable, groupable signature rather than crash or
	// explode-one-per-execution.
	got := ThrottlingSignature("new_provider_specific_thing", "anthropic", "", "")
	want := "new_provider_specific_thing:anthropic"
	if got != want {
		t.Errorf("ThrottlingSignature(new..., anthropic, ...) = %q, want %q", got, want)
	}
}

func Test_ThrottlingSignature_StabilityAcrossCalls(t *testing.T) {
	// Same inputs must always produce the same output. Failure here
	// would mean failure_groups proliferate one-per-execution instead
	// of clustering. Deliberately call the function 100 times and
	// confirm.
	const want = "rate_limit:anthropic:tokens_per_minute"
	for i := 0; i < 100; i++ {
		got := ThrottlingSignature("rate_limit", "anthropic", "tokens_per_minute", "open")
		if got != want {
			t.Fatalf("iteration %d: got %q, want %q (signature is not deterministic)", i, got, want)
		}
	}
}
