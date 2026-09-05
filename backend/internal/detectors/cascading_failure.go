// Cascading-failure detector.
//
// A cascading failure is the multi-agent pathology where the parent
// hands off work to a sub-agent, the sub-agent crashes, and the
// parent either crashes in turn or carries the bad result forward.
// In single-agent systems this collapses into a regular exception;
// in multi-agent systems the same failure surfaces twice (once on
// each side of the handoff) and customers have no way to tell from
// the per-execution view that the two are the same logical bug.
//
// The detector consumes the [(handoff_event, child_terminal_status)]
// join produced by store.ListHandoffsWithChildStatus and fires on
// the first edge where the child reached a failure terminal state
// (anything in failureTerminalStatuses). The signature
// deterministically clusters per (from_agent, to_agent,
// child_status) so repeated cascades along the same agent edge
// (e.g. "planner → coder, child crashed") collapse into one
// failure_group instead of one-group-per-execution-pair.
//
// Future work tracked elsewhere:
//
//   - Cascade window. Right now we fire whenever the child terminated
//     in a failure state, regardless of how long after the handoff.
//     A future iteration should bound this to a configurable window
//     (default 5 minutes between handoff_emitted_at and
//     child_ended_at) so that long-lived spawn handoffs whose
//     children fail hours later do not get grouped as cascades.
//
//   - Spawn handoffs. Right now the detector treats all handoff
//     kinds identically. "spawn" handoffs are fire-and-forget and
//     a parent that terminates successfully while a spawn child
//     crashes later is arguably not a cascading failure but a
//     supervision gap. We will revisit once we have field data
//     on the kind distribution.
package detectors

import (
	"fmt"

	"mesedi/backend/internal/store"
)

// failureTerminalStatuses are the execution terminal states that
// indicate the child agent's work did not succeed. Anything not in
// this set (e.g. StatusCompleted) is treated as a benign handoff.
// The set is intentionally narrow: we do NOT include StatusHalted
// because halt is operator-driven, not a child-induced failure.
var failureTerminalStatuses = map[string]struct{}{
	"crashed":           {},
	"timeout":           {},
	"validation_failed": {},
}

// CascadingFailureThresholds carries the per-project tunable knobs
// for this detector (extensions wave, closes G2 + G3).
//
//   - CascadeWindowSeconds bounds the time between handoff_emitted_at
//     and child_ended_at; rows outside the window are excluded from
//     scoring. Default 86400 (24h) preserves the historical 'no
//     window' behavior; customers can tighten to e.g. 300 (5min)
//     to avoid grouping long-lived spawn handoffs whose children
//     fail hours later.
//
//   - ExcludeSpawnHandoffs when true skips rows where
//     HandoffKind == "spawn" before scoring. Spawn handoffs are
//     fire-and-forget; a parent that succeeded while a spawn child
//     failed later is arguably a supervision gap rather than a
//     cascade. Default false preserves the historical behavior.
type CascadingFailureThresholds struct {
	CascadeWindowSeconds int
	ExcludeSpawnHandoffs bool
}

// DefaultCascadingFailureThresholds returns the historical hardcoded
// behavior (24h window, include spawn handoffs).
func DefaultCascadingFailureThresholds() CascadingFailureThresholds {
	return CascadingFailureThresholds{
		CascadeWindowSeconds: 86400,
		ExcludeSpawnHandoffs: false,
	}
}

// DetectCascadingFailure scans the supplied (handoff, child-status)
// join rows and reports the first failing edge. Returns ("", false)
// when no handoff's child reached a failure terminal state, or when
// the handoff did not resolve to a child execution at all.
//
// First-match priority is sequence-order, matching the order the
// handoffs were emitted by the parent agent. The "first failing
// child wins" rule keeps the signature deterministic across re-runs.
//
// Preserved verbatim for backward compatibility with existing unit
// tests + non-handler call sites. The production execution-close
// path uses DetectCascadingFailureWithThresholds.
func DetectCascadingFailure(rows []store.HandoffWithChildStatus) (signature string, detected bool) {
	return DetectCascadingFailureWithThresholds(rows, DefaultCascadingFailureThresholds())
}

// DetectCascadingFailureWithThresholds is the per-project-aware
// variant. Applies the cascade-window filter and the spawn-handoff
// exclusion per t before running the legacy scoring logic. Closes
// cascading_failure.G2 + G3.
//
// Defensive: CascadeWindowSeconds outside [10, 86400] reverts to
// 86400 default (the validators registry rejects this at write time
// but we re-validate on read).
func DetectCascadingFailureWithThresholds(
	rows []store.HandoffWithChildStatus,
	t CascadingFailureThresholds,
) (signature string, detected bool) {
	if len(rows) == 0 {
		return "", false
	}
	windowSecs := t.CascadeWindowSeconds
	if windowSecs < 10 || windowSecs > 86400 {
		windowSecs = 86400
	}
	for _, r := range rows {
		if !r.ChildExists {
			continue // handoff did not resolve to a same-project child
		}
		if _, bad := failureTerminalStatuses[r.ChildStatus]; !bad {
			continue
		}
		// extensions wave, cascading_failure.G3. Skip
		// spawn handoffs when the customer has opted out of
		// treating them as cascades.
		if t.ExcludeSpawnHandoffs && r.HandoffKind == "spawn" {
			continue
		}
		// extensions wave, cascading_failure.G2. Skip
		// rows where child_ended_at is more than CascadeWindowSeconds
		// after handoff_emitted_at. ChildEndedAt is nullable; when
		// nil we conservatively keep the row (no timestamp = we
		// can't prove it's outside the window).
		if r.ChildEndedAt != nil {
			gap := r.ChildEndedAt.Sub(r.HandoffEmittedAt).Seconds()
			if gap > float64(windowSecs) {
				continue
			}
		}
		// Defensive: empty agent labels would produce a degenerate
		// signature like "cascading_failure:::crashed". Fall back
		// to literal "unknown" for missing identifiers so the
		// signature stays parseable in the dashboard.
		from := r.FromAgent
		if from == "" {
			from = "unknown"
		}
		to := r.ToAgent
		if to == "" {
			to = "unknown"
		}
		return fmt.Sprintf("cascading_failure:%s:%s:%s", from, to, r.ChildStatus), true
	}
	return "", false
}
