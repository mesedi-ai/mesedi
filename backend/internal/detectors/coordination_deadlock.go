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
// Two-layer cycle detection:
//
//  1. Fast 2-cycle scan (preserved verbatim from v1). A 2-cycle is
//     the most common real-world deadlock pattern (planner ↔
//     executor, supervisor ↔ worker) and is detectable with an
//     O(E) scan + an O(E) cross-check. Signature shape:
//     "coordination_deadlock:<a>:<b>" with a < b lexicographically.
//     Existing customer dashboards filtered on specific 2-cycle
//     signatures keep working unchanged.
//
//  2. Tarjan's SCC pass for N >= 3 cycles. Runs ONLY when the
//     fast 2-cycle scan finds no match — preserves signature
//     stability and skips Tarjan cost on the ~90% of executions
//     that either have no deadlock or have a 2-cycle. Tarjan runs
//     in O(V+E); on realistic topologies (typically <100 unique
//     agent roles) this is sub-millisecond. When multiple SCCs of
//     size >= 3 exist, the lexicographically-smallest member-list
//     wins (same tie-break as the 2-cycle path). Signature shape:
//     "coordination_deadlock:<m1>:<m2>:<m3>..." with all members
//     sorted alphabetically — same prefix as 2-cycle, so cycle
//     length is recoverable by counting colons.
//
// Why 2-cycle priority over larger cycles when both exist: in
// real production a 2-cycle co-existing with a 3+-cycle usually
// means the 2-cycle is the simpler / more-actionable signal (a
// hot loop between two specific agents inside a larger topology).
// Demoting the 2-cycle in favor of the larger cycle would also
// silently change alerting behavior for live customers — the
// existing 2-cycle dashboard filters must keep firing on the
// exact same executions they fire on today.
package detectors

import (
	"fmt"
	"sort"
	"strings"

	"mesedi/backend/internal/store"
)

// DetectCoordinationDeadlock scans the supplied handoff edges and
// reports the first cycle it finds. The 2-cycle fast path runs
// first; only when no 2-cycle exists does the Tarjan SCC pass run
// for cycles of length >= 3.
//
// Returns ("", false) when no cycle is present.
//
// First-match priority is lexicographic for both paths so the
// signature is deterministic across re-runs and across event-emit
// ordering.
func DetectCoordinationDeadlock(edges []store.HandoffEdge) (signature string, detected bool) {
	if len(edges) < 2 {
		return "", false
	}
	// Build the directed edge set. Collapse parallel edges (same
	// from→to from different executions) into one set entry; the
	// cycle detection only cares about graph topology, not
	// multiplicity. Skip self-loops; an agent handing off to itself
	// is not a deadlock (it's a recursion or rebalance pattern).
	directed := make(map[directedEdge]struct{}, len(edges))
	for _, e := range edges {
		if e.FromAgent == e.ToAgent {
			continue
		}
		directed[directedEdge{from: e.FromAgent, to: e.ToAgent}] = struct{}{}
	}
	if len(directed) < 2 {
		return "", false
	}
	// Layer 1: 2-cycle fast path. Same behavior as v1; preserves
	// signature stability for existing dashboards.
	if sig, ok := detectTwoCycle(directed); ok {
		return sig, true
	}
	// Layer 2: Tarjan SCC fallback. Runs only when no 2-cycle was
	// found; catches 3+ cycles (planner → researcher → executor →
	// planner and similar multi-agent topology pathologies).
	return detectNCycleSCC(directed)
}

// directedEdge is the canonical map key for the directed
// agent-handoff edge set. Hoisted to package scope so detectors
// can share the type without local-struct equality footguns.
type directedEdge struct {
	from, to string
}

// detectTwoCycle scans the directed-edge set for the
// lexicographically-smallest 2-cycle. Identical to v1 behavior;
// extracted so the public entry point can express the layered
// chain cleanly.
func detectTwoCycle(directed map[directedEdge]struct{}) (signature string, detected bool) {
	pairs := make(map[[2]string]struct{})
	for k := range directed {
		if _, ok := directed[directedEdge{from: k.to, to: k.from}]; !ok {
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

// detectNCycleSCC runs Tarjan's strongly-connected-components
// algorithm on the directed edge set, picks the
// lexicographically-smallest SCC of size >= 3, and builds the
// signature from its sorted members.
//
// Determinism: nodes are iterated in sorted order, adjacency lists
// are also sorted before recursion. Tarjan returns SCCs in
// reverse-topological order; we sort all qualifying SCCs by their
// sorted member-list and pick the smallest. Two layers of
// determinism so the signature is identical across re-runs even
// when Go's map iteration order changes between runs.
//
// Returns ("", false) when no SCC of size >= 3 exists. (Size-2
// SCCs are 2-cycles already handled by the fast path; size-1 SCCs
// are individual nodes with no cycle.)
func detectNCycleSCC(directed map[directedEdge]struct{}) (signature string, detected bool) {
	// Build adjacency list keyed by source node. Collect the unique
	// node set in the process for deterministic iteration.
	adj := map[string][]string{}
	nodeSet := map[string]struct{}{}
	for k := range directed {
		adj[k.from] = append(adj[k.from], k.to)
		nodeSet[k.from] = struct{}{}
		nodeSet[k.to] = struct{}{}
	}
	// Sort each adjacency list for deterministic DFS ordering.
	for n := range adj {
		sort.Strings(adj[n])
	}
	nodes := make([]string, 0, len(nodeSet))
	for n := range nodeSet {
		nodes = append(nodes, n)
	}
	sort.Strings(nodes)

	state := &tarjanState{
		adj:     adj,
		index:   map[string]int{},
		lowlink: map[string]int{},
		onStack: map[string]bool{},
		nextIdx: 0,
		stack:   nil,
		sccsBig: nil,
	}
	for _, n := range nodes {
		if _, visited := state.index[n]; !visited {
			state.strongconnect(n)
		}
	}
	if len(state.sccsBig) == 0 {
		return "", false
	}
	// Sort each SCC's members alphabetically (in place), then sort
	// the SCC list itself by member-list lexicographic order. Pick
	// the smallest.
	for i := range state.sccsBig {
		sort.Strings(state.sccsBig[i])
	}
	sort.Slice(state.sccsBig, func(i, j int) bool {
		a, b := state.sccsBig[i], state.sccsBig[j]
		minLen := len(a)
		if len(b) < minLen {
			minLen = len(b)
		}
		for k := 0; k < minLen; k++ {
			if a[k] != b[k] {
				return a[k] < b[k]
			}
		}
		return len(a) < len(b)
	})
	chosen := state.sccsBig[0]
	return fmt.Sprintf(
		"coordination_deadlock:%s",
		strings.Join(chosen, ":"),
	), true
}

// tarjanState carries the working memory of Tarjan's SCC
// algorithm. Operates on the sorted adjacency list built by
// detectNCycleSCC; collects SCCs of size >= 3 into sccsBig and
// silently drops size-1 (no cycle) and size-2 (already handled by
// the 2-cycle fast path) components.
type tarjanState struct {
	adj     map[string][]string
	index   map[string]int
	lowlink map[string]int
	onStack map[string]bool
	nextIdx int
	stack   []string
	sccsBig [][]string
}

func (s *tarjanState) strongconnect(v string) {
	s.index[v] = s.nextIdx
	s.lowlink[v] = s.nextIdx
	s.nextIdx++
	s.stack = append(s.stack, v)
	s.onStack[v] = true

	for _, w := range s.adj[v] {
		if _, visited := s.index[w]; !visited {
			s.strongconnect(w)
			if s.lowlink[w] < s.lowlink[v] {
				s.lowlink[v] = s.lowlink[w]
			}
		} else if s.onStack[w] {
			if s.index[w] < s.lowlink[v] {
				s.lowlink[v] = s.index[w]
			}
		}
	}

	if s.lowlink[v] == s.index[v] {
		// Root of an SCC; pop everything down to v.
		var scc []string
		for {
			top := s.stack[len(s.stack)-1]
			s.stack = s.stack[:len(s.stack)-1]
			s.onStack[top] = false
			scc = append(scc, top)
			if top == v {
				break
			}
		}
		// Only retain SCCs of size >= 3; smaller components either
		// have no cycle (size 1) or are 2-cycles already covered by
		// the fast path (size 2).
		if len(scc) >= 3 {
			s.sccsBig = append(s.sccsBig, scc)
		}
	}
}
