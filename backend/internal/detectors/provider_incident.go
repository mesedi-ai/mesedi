// Provider-incident detector.
//
// LLM providers (Anthropic, OpenAI, Gemini, Bedrock, Mistral, etc.)
// have outages. When one happens, every tenant in a multi-tenant
// project sees errors against the same provider name at roughly
// the same time. Without a cross-tenant signal, each tenant looks
// like an independent caller-side bug; with one, the whole
// pattern collapses into a single "provider had an incident"
// failure_group and the operator can stop hunting through
// per-tenant traces.
//
// This detector consumes a single (provider, error_class,
// tenant_count) tuple computed by the handler from
// store.CountDistinctTenantsWithProviderError. It fires when
// tenant_count meets or exceeds the threshold (default 2 ,
// "at least two tenants experienced the same error in the same
// window"). The handler runs this only after observing the
// current execution emit at least one matching error, so the
// threshold compares apples to apples ("this execution + at
// least one other tenant" produces tenant_count >= 2).
//
// Signature shape: "provider_incident:<provider>:<error_class>"
// so a single outage clusters all affected runs into one group
// regardless of which tenant emitted them.
package detectors

import (
	"fmt"
)

// MinTenantsForProviderIncident is the default threshold the
// handler uses when calling DetectProviderIncident. Exported so
// the wiring and the detector stay in sync.
const MinTenantsForProviderIncident = 2

// DetectProviderIncident reports whether the supplied
// (provider, error_class, tenant_count) tuple meets the cross-
// tenant threshold for a provider-side outage. Returns ("",
// false) when below threshold or when either of the identifier
// strings is empty (which would produce a degenerate signature).
func DetectProviderIncident(provider, errorClass string, tenantCount, threshold int) (signature string, detected bool) {
	if provider == "" || errorClass == "" {
		return "", false
	}
	if threshold <= 0 {
		threshold = MinTenantsForProviderIncident
	}
	if tenantCount < threshold {
		return "", false
	}
	return fmt.Sprintf("provider_incident:%s:%s", provider, errorClass), true
}
