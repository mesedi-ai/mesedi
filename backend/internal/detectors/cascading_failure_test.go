package detectors

// Unit tests for cascading_failure WithThresholds variants
// (Theme B extensions wave — closes G2 + G3). Covers:
//   - Default thresholds preserve historical behavior.
//   - Cascade-window filter excludes child-failures whose
//     ChildEndedAt is more than CascadeWindowSeconds after
//     HandoffEmittedAt.
//   - Spawn-handoff exclusion skips rows where HandoffKind=="spawn"
//     when ExcludeSpawnHandoffs is true.
//   - Legacy DetectCascadingFailure wrapper produces byte-identical
//     behavior to DetectCascadingFailureWithThresholds(rows,
//     defaults) — backward compat.
//   - Bad config (window out of bounds) falls back to default.

import (
	"testing"
	"time"

	"mesedi/backend/internal/store"
)

func makeHandoffRow(from, to, status, kind string, gap time.Duration) store.HandoffWithChildStatus {
	emitted := time.Now().UTC().Add(-gap - 1*time.Minute)
	ended := emitted.Add(gap)
	return store.HandoffWithChildStatus{
		FromAgent:        from,
		ToAgent:          to,
		HandoffKind:      kind,
		ChildExists:      true,
		ChildStatus:      status,
		ChildEndedAt:     &ended,
		HandoffEmittedAt: emitted,
	}
}

func Test_CascadingFailure_LegacyWrapperUsesDefaults(t *testing.T) {
	// Legacy DetectCascadingFailure must produce byte-identical
	// output to DetectCascadingFailureWithThresholds(rows, defaults).
	rows := []store.HandoffWithChildStatus{
		makeHandoffRow("planner", "coder", "crashed", "delegate", 10*time.Second),
	}
	legacySig, legacyFired := DetectCascadingFailure(rows)
	newSig, newFired := DetectCascadingFailureWithThresholds(
		rows, DefaultCascadingFailureThresholds(),
	)
	if legacyFired != newFired {
		t.Errorf("legacy fired=%v vs WithThresholds default fired=%v — backward compat broken",
			legacyFired, newFired)
	}
	if legacySig != newSig {
		t.Errorf("legacy sig=%q vs WithThresholds default sig=%q — backward compat broken",
			legacySig, newSig)
	}
}

func Test_CascadingFailure_WindowExcludesLateFailures(t *testing.T) {
	// One handoff whose child failed 10 MINUTES after emission.
	// Default window (86400s = 24h) includes it; tightened window
	// (300s = 5min) excludes it.
	rows := []store.HandoffWithChildStatus{
		makeHandoffRow("planner", "coder", "crashed", "delegate", 10*time.Minute),
	}
	if _, fired := DetectCascadingFailureWithThresholds(rows, DefaultCascadingFailureThresholds()); !fired {
		t.Errorf("24h default window should include a 10min-gap failure")
	}
	tight := DefaultCascadingFailureThresholds()
	tight.CascadeWindowSeconds = 300
	if _, fired := DetectCascadingFailureWithThresholds(rows, tight); fired {
		t.Errorf("tightened 5min window should exclude a 10min-gap failure")
	}
}

func Test_CascadingFailure_SpawnExclusion(t *testing.T) {
	// Spawn handoff with failed child. Default treatment includes
	// it; ExcludeSpawnHandoffs=true excludes it (no other rows so
	// no cascade fires).
	rows := []store.HandoffWithChildStatus{
		makeHandoffRow("planner", "worker", "crashed", "spawn", 5*time.Second),
	}
	if _, fired := DetectCascadingFailureWithThresholds(rows, DefaultCascadingFailureThresholds()); !fired {
		t.Errorf("default treatment should fire on spawn handoff with crashed child")
	}
	exclude := DefaultCascadingFailureThresholds()
	exclude.ExcludeSpawnHandoffs = true
	if _, fired := DetectCascadingFailureWithThresholds(rows, exclude); fired {
		t.Errorf("ExcludeSpawnHandoffs=true should skip spawn handoffs; no cascade should fire")
	}
}

func Test_CascadingFailure_BadWindowFallsBackToDefault(t *testing.T) {
	// CascadeWindowSeconds outside [10, 86400] should fall back to
	// 86400. With 10min gap, default would include it.
	rows := []store.HandoffWithChildStatus{
		makeHandoffRow("planner", "coder", "crashed", "delegate", 10*time.Minute),
	}
	bad := CascadingFailureThresholds{CascadeWindowSeconds: 0}
	if _, fired := DetectCascadingFailureWithThresholds(rows, bad); !fired {
		t.Errorf("bad window (0) should fall back to default 86400 and fire")
	}
}
