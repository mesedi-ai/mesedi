// Unit tests for the canonical-error-class bindings.
//
// provider_error_classes.go re-exports auto-generated values from
// provider_error_classes_generated.go under the historical Go names
// (ErrorClassRateLimited, etc.) and exposes IsProviderSideErrorClass
// for the provider_incident detector's customer-vs-provider filter.
// These tests catch:
//
//   - Constant resolution failures (codegen output out of sync)
//   - Membership drift in ProviderSideErrorClasses (a class moving
//     from provider-side to customer-side without the cross-tenant
//     aggregation contract being updated)
//   - Edge cases (empty string, unknown class)
//
// The drift test for codegen vs spec staleness lives in the SDK
// (sdk-python/tests/test_mapping_staleness.py, Wave ).
package detectors

import "testing"

func Test_ErrorClassConstants_ResolveToNonEmpty(t *testing.T) {
	// Every re-exported constant must resolve to a non-empty
	// wire-format string. A blank value means the generator output
	// drifted away from the spec key the binding expects, which
	// would silently break every detector that compares against it.
	cases := map[string]string{
		"ErrorClassRateLimited":        ErrorClassRateLimited,
		"ErrorClassQuotaExhausted":     ErrorClassQuotaExhausted,
		"ErrorClassInternalError":      ErrorClassInternalError,
		"ErrorClassServiceUnavailable": ErrorClassServiceUnavailable,
		"ErrorClassTimeout":            ErrorClassTimeout,
		"ErrorClassInvalidAPIKey":      ErrorClassInvalidAPIKey,
		"ErrorClassClientError":        ErrorClassClientError,
		"ErrorClassUnknown":            ErrorClassUnknown,
	}
	for name, value := range cases {
		t.Run(name, func(t *testing.T) {
			if value == "" {
				t.Errorf("%s resolved to empty string; codegen output out of sync with bindings", name)
			}
		})
	}
}

func Test_ProviderSideErrorClasses_NonEmpty(t *testing.T) {
	// The provider_incident detector treats ProviderSideErrorClasses
	// as its membership filter. An empty set would silently disable
	// cross-tenant aggregation.
	if len(ProviderSideErrorClasses) == 0 {
		t.Fatal("ProviderSideErrorClasses is empty; provider_incident detector would silently never fire")
	}
}

func Test_IsProviderSideErrorClass_ProviderSide(t *testing.T) {
	// Classes the provider is responsible for — MUST trigger
	// cross-tenant aggregation.
	cases := []string{
		ErrorClassRateLimited,
		ErrorClassQuotaExhausted,
		ErrorClassInternalError,
		ErrorClassServiceUnavailable,
	}
	for _, class := range cases {
		t.Run(class, func(t *testing.T) {
			if !IsProviderSideErrorClass(class) {
				t.Errorf("%q should be classified provider-side (membership in ProviderSideErrorClasses)", class)
			}
		})
	}
}

func Test_IsProviderSideErrorClass_CustomerSide(t *testing.T) {
	// Classes the customer is responsible for — MUST NOT trigger
	// cross-tenant aggregation (would generate noise grouping
	// unrelated customer-side errors together).
	cases := []string{
		ErrorClassInvalidAPIKey,
		ErrorClassClientError,
	}
	for _, class := range cases {
		t.Run(class, func(t *testing.T) {
			if IsProviderSideErrorClass(class) {
				t.Errorf("%q should NOT be classified provider-side (would cluster customer-side errors as a provider incident)", class)
			}
		})
	}
}

func Test_IsProviderSideErrorClass_EdgeCases(t *testing.T) {
	cases := []struct {
		name  string
		class string
		want  bool
	}{
		{"empty_string", "", false},
		{"unknown_class", "not_a_real_class", false},
		{"case_sensitive", "RATE_LIMITED", false}, // uppercase form is the spec KEY, not the wire VALUE
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := IsProviderSideErrorClass(tc.class); got != tc.want {
				t.Errorf("IsProviderSideErrorClass(%q) = %v, want %v", tc.class, got, tc.want)
			}
		})
	}
}
