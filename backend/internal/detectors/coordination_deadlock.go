// Coordination-deadlock detector (Mesedi #13).
//
// A coordination deadlock is the multi-agent pathology where two
// (or more) agents are waiting on each other and the system makes
// no forward progress. In Coffman's original framing the four
// conditions are: mutual exclusion, hold and wait, no preemption,
// and circular wait. In the agent-handoff setting the first three
// are implicit in the SDK semantics: one agent per role at a
// time, synchronous handoff = hold and wait, the SDK does not
// unwind a handoff = no preemption. What remains for detection is
// the "circular wait" condition: a cycle in the directed
// agent-handoff graph.
//
// This detector consumes a flat slice of HandoffEdge records
// (produced by store.ListHandoffEdgesInTopology) and looks for
// cycles in the agent-role graph induced by those edges.
//
// v1 detects 2-cycles only (A→B AND B→A appear in the same
// topology subtree). 2-cycles cover the most common real-world
// deadlock pattern (planner ↔ executor, supervisor ↔ worker) and
// are detectable with an O(E) scan + an O(E) cross-check; longer
// cycles require Tarjan's SCC algorithm and will be added in a
// follow-up iteration.
//
// Signature shape: "coordination_deadlock:<agent_a>:<agent_b>"
// where agent_a < agent_b lexicographically so A↔B and B↔A
// collapse to the same failure_group regardless of which side
// fires the handoff first.
package detectors

import (
	"fmt"
	"sort"

	"mesedi/backend/internal/store"
)

// DetectCoordinationDeadlock scans the supplied handoff edges and
// reports the first 2-cycle it finds. Returns ("", false) when no
// cycle is present.
//
// First-match priority is lexicographic over the canonical (a, b)
// agent-name pair so the signature is deterministic across re-runs
// and across event-emit ordering. The lexicographic-smallest pair
// wins so a topology with multiple deadlocks still produces a
// stable signature.
func DetectCoordinationDeadlock(edges []store.HandoffEdge) (signature string, detected bool) {
	if len(edges) < 2 {
		return "", false
	}
	// Build a set of directed agent-role edges. We collapse parallel
	// edges (same from→to from different executions) into one set
	// entry; the cycle detection only cares about graph topology,
	// not multiplicity.
	type edgeKey struct{ from, to string }
	directed := make(map[edgeKey]struct{}, len(edges))
	for _, e := range edges {
		directed[edgeKey{from: e.FromAgent, to: e.ToAgent}] = struct{}{}
	}
	// Collect all 2-cycles. A 2-cycle is a pair (a, b) where both
	// (a → b) and (b → a) appear in the directed-edge set.
	pairs := make(map[[2]string]struct{})
	for k := range directed {
		// Skip self-loops; an agent handing off to itself is not a
		// deadlock (it's a recursion or rebalance pattern).
		if k.from == k.to {
			continue
		}
		if _, ok := directed[edgeKey{from: k.to, to: k.from}]; !ok {
			continue
		}
		// Canonical ordering so A↔B and B↔A produce the same key.
		a, b := k.from, k.to
		if b < a {
			a, b = b, a
		}
		pairs[[2]string{a, b}] = struct{}{}
	}
	if len(pairs) == 0 {
		return "", false
	}
	// Pick the lexicographically-smallest cycle for a deterministic
	// signature when more than one deadlock exists in the same
	// subtree.
	keys := make([][2]string, 0, len(pairs))
	for p := range pairs {
		keys = append(keys, p)
	}
	sort.Slice(keys, func(i, j int) bool {
		if keys[i][0] != keys[j][0] {
			return keys[i][0] < keys[j][0]
		}
		return keys[i][1] < keys[j][1]
	})
	chosen := keys[0]
	return fmt.Sprintf("coordination_deadlock:%s:%s", chosen[0], chosen[1]), true
}
