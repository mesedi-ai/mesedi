package detectors

// Canonical provider-error vocabulary, backend side. Values + the
// provider-side filter set are sourced from
// spec/error_classes.yaml via scripts/codegen.py, which writes
// provider_error_classes_generated.go. Hand-written constants
// below re-export the generated values under the historical
// Go names so call sites stay unchanged.
//
// To add a new class: edit spec/error_classes.yaml and run
// `python scripts/codegen.py`. The CI staleness check runs codegen
// with --check to fail the build if generators are out of date.

// ErrorClass* constants re-export the generated wire-format values
// under the historical Go-style names. The generated file is the
// source of truth — these are bindings, not declarations.
var (
	ErrorClassRateLimited        = ErrorClassValues["RATE_LIMITED"]
	ErrorClassQuotaExhausted     = ErrorClassValues["QUOTA_EXHAUSTED"]
	ErrorClassInternalError      = ErrorClassValues["INTERNAL_ERROR"]
	ErrorClassServiceUnavailable = ErrorClassValues["SERVICE_UNAVAILABLE"]
	ErrorClassTimeout            = ErrorClassValues["TIMEOUT"]
	ErrorClassInvalidAPIKey      = ErrorClassValues["INVALID_API_KEY"]
	ErrorClassClientError        = ErrorClassValues["CLIENT_ERROR"]
	ErrorClassUnknown            = ErrorClassValues["UNKNOWN"]
)

// ProviderSideErrorClasses is the closed set of canonical error
// classes that trigger provider_incident cross-tenant aggregation.
// Sourced from the generated file so Python / TypeScript / Go all
// see the SAME membership without manual sync.
var ProviderSideErrorClasses = ProviderSideErrorClassValues

// IsProviderSideErrorClass reports whether the supplied error_class
// is in the cross-tenant-aggregation set. The provider_incident
// detector calls this to drop customer-side classes before counting
// distinct tenants.
func IsProviderSideErrorClass(class string) bool {
	_, ok := ProviderSideErrorClasses[class]
	return ok
}
