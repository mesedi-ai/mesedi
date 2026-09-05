// Unit tests for the provider-incident detector.
//
// Detector contract: takes (provider, error_class, tenant_count,
// threshold) and reports whether cross-tenant signal is strong
// enough to fire. Signature: "provider_incident:<provider>:<class>".
// Default threshold MinTenantsForProviderIncident = 2 ("this exec
// + at least one other tenant"). Degenerate inputs (empty provider
// or class) return ("", false) to avoid the dashboard rendering a
// "provider_incident::" cluster.
package detectors

import "testing"

func Test_DetectProviderIncident_BelowThreshold(t *testing.T) {
	sig, detected := DetectProviderIncident("anthropic", "service_unavailable", 1, 2)
	if detected {
		t.Errorf("tenant_count=1, threshold=2 should not fire; got signature=%q", sig)
	}
}

func Test_DetectProviderIncident_AtThreshold(t *testing.T) {
	// >= comparison: tenant_count == threshold MUST fire.
	sig, detected := DetectProviderIncident("anthropic", "service_unavailable", 2, 2)
	if !detected {
		t.Fatal("tenant_count=2 with threshold=2 should fire (>=)")
	}
	if sig != "provider_incident:anthropic:service_unavailable" {
		t.Errorf("expected signature 'provider_incident:anthropic:service_unavailable', got %q", sig)
	}
}

func Test_DetectProviderIncident_AboveThreshold(t *testing.T) {
	sig, detected := DetectProviderIncident("openai", "rate_limited", 5, 2)
	if !detected {
		t.Fatal("tenant_count=5 should fire above threshold=2")
	}
	if sig != "provider_incident:openai:rate_limited" {
		t.Errorf("expected signature 'provider_incident:openai:rate_limited', got %q", sig)
	}
}

func Test_DetectProviderIncident_DegenerateInputs(t *testing.T) {
	// Empty provider OR empty error_class should NOT fire, would
	// produce a degenerate signature like "provider_incident::" that
	// the dashboard can't render usefully.
	cases := []struct {
		name        string
		provider    string
		errorClass  string
		tenantCount int
	}{
		{"empty_provider", "", "service_unavailable", 5},
		{"empty_error_class", "anthropic", "", 5},
		{"both_empty", "", "", 5},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig, detected := DetectProviderIncident(tc.provider, tc.errorClass, tc.tenantCount, 2)
			if detected {
				t.Errorf("degenerate input should not fire, got signature=%q", sig)
			}
			if sig != "" {
				t.Errorf("expected empty signature, got %q", sig)
			}
		})
	}
}

func Test_DetectProviderIncident_ZeroOrNegativeThresholdFallsBack(t *testing.T) {
	// threshold <= 0 should fall back to MinTenantsForProviderIncident
	// (= 2) so accidental misconfiguration doesn't silently disable
	// the detector.
	cases := []struct {
		name      string
		threshold int
	}{
		{"zero_threshold", 0},
		{"negative_threshold", -1},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// With tenant_count = 1, default threshold (2) means no fire
			sig, detected := DetectProviderIncident("anthropic", "service_unavailable", 1, tc.threshold)
			if detected {
				t.Errorf("default threshold should be 2, so tenant_count=1 should not fire; got %q", sig)
			}
			// With tenant_count = 2, default threshold (2) means fire
			sig2, detected2 := DetectProviderIncident("anthropic", "service_unavailable", 2, tc.threshold)
			if !detected2 {
				t.Errorf("default threshold should be 2, tenant_count=2 should fire; got detected=false")
			}
			if sig2 != "provider_incident:anthropic:service_unavailable" {
				t.Errorf("wrong signature on fallback-threshold fire: %q", sig2)
			}
		})
	}
}

func Test_MinTenantsForProviderIncident_LocksDocumentedValue(t *testing.T) {
	// The docstring + handler wire-up both depend on this constant
	// being 2. If someone changes it without updating the docs and
	// the threshold validator, this test fails the build.
	if MinTenantsForProviderIncident != 2 {
		t.Errorf("MinTenantsForProviderIncident = %d, want 2", MinTenantsForProviderIncident)
	}
}
