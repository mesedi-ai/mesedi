// Unit tests for the coordination_deadlock detector.
//
// Two coverage groups:
//   - 2-cycle fast path (preserved verbatim from v1; new tests pin
//     the existing behavior so future refactors can't regress it).
//   - Tarjan SCC fallback for N >= 3 cycles (the wave that closes
//     coordination_deadlock.G1).
package detectors

import (
	"strings"
	"testing"

	"mesedi/backend/internal/store"
)

// edge is a tiny test helper that builds a store.HandoffEdge with
// only the two fields the detector reads.
func edge(from, to string) store.HandoffEdge {
	return store.HandoffEdge{FromAgent: from, ToAgent: to}
}

// ─────────────────────────────────────────────────────────────────
// 2-cycle fast path (regression pins on existing behavior).
// ─────────────────────────────────────────────────────────────────

func Test_CoordinationDeadlock_TwoCycle_BasicFires(t *testing.T) {
	edges := []store.HandoffEdge{
		edge("planner", "reviewer"),
		edge("reviewer", "planner"),
	}
	sig, detected := DetectCoordinationDeadlock(edges)
	if !detected {
		t.Fatalf("expected 2-cycle detection, got none")
	}
	if sig != "coordination_deadlock:planner:reviewer" {
		t.Errorf("expected alphabetized 2-cycle signature, got %q", sig)
	}
}

func Test_CoordinationDeadlock_TwoCycle_OrderInvariant(t *testing.T) {
	// Same cycle emitted with reversed edge order — must produce
	// the same signature.
	edges := []store.HandoffEdge{
		edge("reviewer", "planner"),
		edge("planner", "reviewer"),
	}
	sig, detected := DetectCoordinationDeadlock(edges)
	if !detected || sig != "coordination_deadlock:planner:reviewer" {
		t.Errorf("expected order-invariant 2-cycle signature, got sig=%q detected=%v", sig, detected)
	}
}

func Test_CoordinationDeadlock_TwoCycle_MultipleDeadlocks(t *testing.T) {
	// Two distinct 2-cycles in the same topology. The
	// lexicographically-smallest pair wins (alice:bob beats
	// planner:reviewer).
	edges := []store.HandoffEdge{
		edge("planner", "reviewer"),
		edge("reviewer", "planner"),
		edge("alice", "bob"),
		edge("bob", "alice"),
	}
	sig, _ := DetectCoordinationDeadlock(edges)
	if sig != "coordination_deadlock:alice:bob" {
		t.Errorf("expected lex-smallest 2-cycle, got %q", sig)
	}
}

func Test_CoordinationDeadlock_TwoCyclePriorityOverThreeCycle(t *testing.T) {
	// Topology has BOTH a 2-cycle and a 3-cycle. Q4-approved
	// behavior: 2-cycle wins (signature stability for existing
	// dashboards).
	edges := []store.HandoffEdge{
		// 2-cycle: alice ↔ bob
		edge("alice", "bob"),
		edge("bob", "alice"),
		// 3-cycle: planner → researcher → executor → planner
		edge("planner", "researcher"),
		edge("researcher", "executor"),
		edge("executor", "planner"),
	}
	sig, detected := DetectCoordinationDeadlock(edges)
	if !detected {
		t.Fatalf("expected detection, got none")
	}
	if sig != "coordination_deadlock:alice:bob" {
		t.Errorf("expected 2-cycle to win over 3-cycle, got %q", sig)
	}
}

func Test_CoordinationDeadlock_NoCycle(t *testing.T) {
	// Acyclic chain: planner → researcher → executor (no back-edge).
	edges := []store.HandoffEdge{
		edge("planner", "researcher"),
		edge("researcher", "executor"),
	}
	if sig, detected := DetectCoordinationDeadlock(edges); detected {
		t.Errorf("expected no detection on acyclic chain, got %q", sig)
	}
}

func Test_CoordinationDeadlock_SelfLoopIgnored(t *testing.T) {
	// An agent handing off to itself is a recursion / rebalance
	// pattern, not a deadlock. Plus a non-cycle handoff for context.
	edges := []store.HandoffEdge{
		edge("planner", "planner"),
		edge("planner", "executor"),
	}
	if sig, detected := DetectCoordinationDeadlock(edges); detected {
		t.Errorf("expected self-loop ignored, got %q", sig)
	}
}

func Test_CoordinationDeadlock_BelowMinEdges(t *testing.T) {
	// Single edge — cannot form a cycle.
	edges := []store.HandoffEdge{
		edge("planner", "executor"),
	}
	if _, detected := DetectCoordinationDeadlock(edges); detected {
		t.Errorf("expected no detection on single edge")
	}
}

// ─────────────────────────────────────────────────────────────────
// Tarjan SCC fallback — closes coordination_deadlock.G1.
// ─────────────────────────────────────────────────────────────────

func Test_CoordinationDeadlock_ThreeCycleFires(t *testing.T) {
	// Classic multi-agent topology pathology: planner →
	// researcher → executor → planner. No 2-cycle present;
	// Tarjan fallback must catch it.
	edges := []store.HandoffEdge{
		edge("planner", "researcher"),
		edge("researcher", "executor"),
		edge("executor", "planner"),
	}
	sig, detected := DetectCoordinationDeadlock(edges)
	if !detected {
		t.Fatalf("expected 3-cycle detection, got none")
	}
	expected := "coordination_deadlock:executor:planner:researcher"
	if sig != expected {
		t.Errorf("expected alphabetized 3-cycle signature %q, got %q", expected, sig)
	}
}

func Test_CoordinationDeadlock_ThreeCycleOrderInvariant(t *testing.T) {
	// Same 3-cycle with edges supplied in different order — must
	// produce identical signature.
	edges := []store.HandoffEdge{
		edge("executor", "planner"),
		edge("planner", "researcher"),
		edge("researcher", "executor"),
	}
	sig, _ := DetectCoordinationDeadlock(edges)
	if sig != "coordination_deadlock:executor:planner:researcher" {
		t.Errorf("expected order-invariant 3-cycle signature, got %q", sig)
	}
}

func Test_CoordinationDeadlock_FourCycleFires(t *testing.T) {
	// 4-cycle: planner → researcher → executor → critic → planner.
	edges := []store.HandoffEdge{
		edge("planner", "researcher"),
		edge("researcher", "executor"),
		edge("executor", "critic"),
		edge("critic", "planner"),
	}
	sig, detected := DetectCoordinationDeadlock(edges)
	if !detected {
		t.Fatalf("expected 4-cycle detection, got none")
	}
	expected := "coordination_deadlock:critic:executor:planner:researcher"
	if sig != expected {
		t.Errorf("expected 4-member signature %q, got %q", expected, sig)
	}
	// Cycle length is recoverable by counting colons + 1.
	if got := strings.Count(sig, ":") + 1; got != 5 {
		// 5 = "coordination_deadlock" + 4 agents.
		t.Errorf("expected 5 colon-separated tokens including class, got %d (sig=%q)", got, sig)
	}
}

func Test_CoordinationDeadlock_MultipleDisjointSCCsLexSmallestWins(t *testing.T) {
	// Two disconnected 3-cycles. Both are valid SCCs of size 3;
	// the lexicographically-smallest member-list wins.
	edges := []store.HandoffEdge{
		// SCC 1: planner / researcher / executor
		edge("planner", "researcher"),
		edge("researcher", "executor"),
		edge("executor", "planner"),
		// SCC 2: alice / bob / charlie
		edge("alice", "bob"),
		edge("bob", "charlie"),
		edge("charlie", "alice"),
	}
	sig, detected := DetectCoordinationDeadlock(edges)
	if !detected {
		t.Fatalf("expected detection, got none")
	}
	// alice:bob:charlie sorts before executor:planner:researcher.
	expected := "coordination_deadlock:alice:bob:charlie"
	if sig != expected {
		t.Errorf("expected lex-smallest SCC %q, got %q", expected, sig)
	}
}

func Test_CoordinationDeadlock_AcyclicDoesNotFire(t *testing.T) {
	// Long acyclic chain — Tarjan must find no SCC >= 3.
	edges := []store.HandoffEdge{
		edge("a", "b"),
		edge("b", "c"),
		edge("c", "d"),
		edge("d", "e"),
		edge("e", "f"),
	}
	if sig, detected := DetectCoordinationDeadlock(edges); detected {
		t.Errorf("expected no detection on acyclic chain, got %q", sig)
	}
}

func Test_CoordinationDeadlock_BranchingNoCycle(t *testing.T) {
	// Tree topology: planner branches to researcher and executor;
	// each has its own children. No back-edges.
	edges := []store.HandoffEdge{
		edge("planner", "researcher"),
		edge("planner", "executor"),
		edge("researcher", "junior_research_a"),
		edge("researcher", "junior_research_b"),
		edge("executor", "tool_caller"),
	}
	if sig, detected := DetectCoordinationDeadlock(edges); detected {
		t.Errorf("expected no detection on branching tree, got %q", sig)
	}
}

func Test_CoordinationDeadlock_DeterministicAcrossRuns(t *testing.T) {
	// Run the same 3-cycle topology 50 times; signature must be
	// identical every time. This pins the determinism guarantee
	// against Go's map iteration order randomness.
	edges := []store.HandoffEdge{
		edge("planner", "researcher"),
		edge("researcher", "executor"),
		edge("executor", "planner"),
	}
	first, _ := DetectCoordinationDeadlock(edges)
	for i := 0; i < 50; i++ {
		got, _ := DetectCoordinationDeadlock(edges)
		if got != first {
			t.Fatalf("signature drift on iteration %d: %q vs %q", i, got, first)
		}
	}
}

func Test_CoordinationDeadlock_ThreeCycleWithBranchingTail(t *testing.T) {
	// 3-cycle plus a non-cyclic tail off one of its members. The
	// SCC must be exactly the 3 cyclic members; the tail node
	// must NOT appear in the signature.
	edges := []store.HandoffEdge{
		// 3-cycle
		edge("planner", "researcher"),
		edge("researcher", "executor"),
		edge("executor", "planner"),
		// Tail off executor — not part of any cycle
		edge("executor", "tool_caller"),
	}
	sig, detected := DetectCoordinationDeadlock(edges)
	if !detected {
		t.Fatalf("expected detection, got none")
	}
	expected := "coordination_deadlock:executor:planner:researcher"
	if sig != expected {
		t.Errorf("expected cycle-only signature %q, got %q", expected, sig)
	}
	if strings.Contains(sig, "tool_caller") {
		t.Errorf("non-cyclic tail leaked into signature: %q", sig)
	}
}
