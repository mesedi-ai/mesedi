package detectors

// Canonical provider-error vocabulary, backend side. Mirrors the
// `ErrorClass` constants and `PROVIDER_SIDE_ERROR_CLASSES` set in
// the Python and TypeScript SDKs (mesedi/errors.py, src/errors.ts).
// The provider_incident detector only cross-tenant-aggregates
// error classes in :data:`ProviderSideErrorClasses` — customer-side
// classes (invalid_api_key, client_error, unknown) get stored in
// the event payload for observability but never trigger a
// provider_incident failure_group because they're not actually
// signals of provider-side outages.
//
// Adding a new canonical value here requires the same addition to
// BOTH SDK files and a coordinated release of all three. The
// integration suite at backend/test/integration/ exercises the
// SDK + backend pair end-to-end and is the canary for drift.

const (
	// ErrorClassRateLimited — provider says you're going too fast.
	ErrorClassRateLimited = "rate_limited"

	// ErrorClassQuotaExhausted — billing / quota cap hit.
	ErrorClassQuotaExhausted = "quota_exhausted"

	// ErrorClassInternalError — provider 5xx or malformed response.
	ErrorClassInternalError = "internal_error"

	// ErrorClassServiceUnavailable — provider unreachable /
	// overloaded / circuit-open.
	ErrorClassServiceUnavailable = "service_unavailable"

	// ErrorClassTimeout — provider exceeded its configured timeout.
	ErrorClassTimeout = "timeout"

	// ErrorClassInvalidAPIKey — auth rejection. Customer-side, not
	// a provider incident.
	ErrorClassInvalidAPIKey = "invalid_api_key"

	// ErrorClassClientError — 4xx request validation failure.
	// Customer-side, not a provider incident.
	ErrorClassClientError = "client_error"

	// ErrorClassUnknown — couldn't classify. Backend treats as
	// non-clusterable to avoid mislabeling.
	ErrorClassUnknown = "unknown"
)

// ProviderSideErrorClasses is the closed set of canonical error
// classes that trigger provider_incident cross-tenant aggregation.
// Must match `PROVIDER_SIDE_ERROR_CLASSES` in mesedi/errors.py and
// src/errors.ts exactly.
var ProviderSideErrorClasses = map[string]struct{}{
	ErrorClassRateLimited:        {},
	ErrorClassQuotaExhausted:     {},
	ErrorClassInternalError:      {},
	ErrorClassServiceUnavailable: {},
	ErrorClassTimeout:            {},
}

// IsProviderSideErrorClass reports whether the supplied error_class
// is in the cross-tenant-aggregation set. The provider_incident
// detector calls this to drop customer-side classes before counting
// distinct tenants.
func IsProviderSideErrorClass(class string) bool {
	_, ok := ProviderSideErrorClasses[class]
	return ok
}
