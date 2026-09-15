// Covert-coordination detector, the cheap counter.
//
// Two demonstrations in a fortnight (the Hugging Face swarm, then
// GitSpawn's staging) showed agents coordinating through a shared
// external channel none of their operators was watching. The full
// problem is unsolved anywhere; the cheap version is nearly a copy
// of the provider-incident counter, on purpose: where that one
// counts distinct TENANTS reporting the same provider error, this
// one counts distinct RUNS contacting the same egress destination.
//
// The store does the counting server-side
// (ListCoordinatingEgressDestinations); this function is the
// decision and the signature, kept separate so the threshold logic
// is testable without a database.
package detectors

// DetectCovertCoordination fires when distinctRuns meets the
// threshold for one destination. threshold <= 0 means the caller
// declined to choose; the store-layer default applies there, and
// this function treats non-positive thresholds as "never fire"
// rather than inventing its own number, so there is exactly one
// home for the default.
func DetectCovertCoordination(destination string, distinctRuns, threshold int) (string, bool) {
	if destination == "" || threshold <= 0 || distinctRuns < threshold {
		return "", false
	}
	return "covert_coordination:" + destination, true
}
