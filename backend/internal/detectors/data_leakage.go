package detectors

// data_leakage detector per-project tunables (data_leakage.G5 wave).
//
// Unlike most detectors in this package, data_leakage has no
// stand-alone scan function here — the heavy lifting lives in the
// dlp/ package (rule registry + scanner) and the store layer
// (FindFirstDLPSignal* methods select which hits promote to a
// failure_group). The per-project knob this file exposes controls
// WHICH severities promote: today's hardcoded `["critical", "high"]`
// becomes a per-project tunable so regulated-industry customers can
// add "medium" to fire on PII patterns the default skips.

// DataLeakageThresholds carries the per-project tunable knob for
// the data_leakage detector. AllowedSeverities is the closed set of
// severity strings whose dlp_scan_result hits promote to a
// failure_group on the execution-close path. Hits at severities NOT
// in this list are still recorded (the scanner runs the full rule
// set regardless and emits dlp_scan_result events for every hit) —
// they just don't create or update a failure_group.
//
// Default ["critical", "high"] matches the historical hardcoded
// behavior. Customers can tighten to ["critical"] for less noise or
// loosen to ["critical", "high", "medium"] for regulated workloads
// that want PII patterns to page.
type DataLeakageThresholds struct {
	AllowedSeverities []string
}

// DefaultDataLeakageThresholds returns the historical hardcoded
// default. Used by legacy call sites and tests.
func DefaultDataLeakageThresholds() DataLeakageThresholds {
	return DataLeakageThresholds{
		AllowedSeverities: []string{"critical", "high"},
	}
}

// EffectiveAllowedSeverities returns the validated severity slice —
// defensive against bad config that escaped the validators registry.
// Empty input or any value outside the closed set {"critical",
// "high", "medium"} reverts the whole slice to the default. We
// don't partial-validate (drop invalid entries, keep valid ones)
// because the customer's intent on a malformed config is ambiguous
// and the safe failure mode is "treat as default" rather than
// "silently use a subset they didn't ask for."
func (t DataLeakageThresholds) EffectiveAllowedSeverities() []string {
	if len(t.AllowedSeverities) == 0 {
		return DefaultDataLeakageThresholds().AllowedSeverities
	}
	allowed := map[string]struct{}{
		"critical": {},
		"high":     {},
		"medium":   {},
	}
	for _, sev := range t.AllowedSeverities {
		if _, ok := allowed[sev]; !ok {
			return DefaultDataLeakageThresholds().AllowedSeverities
		}
	}
	return t.AllowedSeverities
}
