# Coordination deadlock

Mesedi detected a cycle in the agent_handoff graph within this execution's topology subtree. Two or more agents are waiting on each other to release control — Coffman's circular-wait condition expressed in agent-role terms. The SDK semantics make the other three Coffman conditions (mutual exclusion, hold-and-wait, no-preemption) implicit, so the detector's job is to find the fourth.

## How the detector finds cycles

Two-layer detection:

1. **2-cycle fast path.** When the directed handoff graph contains A→B and B→A within the same execution chain, fires under `coordination_deadlock:<A>:<B>` with members sorted alphabetically. This is the most common real-world deadlock pattern (planner ↔ executor, supervisor ↔ worker) and is detected with a linear scan.

2. **Tarjan SCC fallback.** When no 2-cycle exists, the detector runs Tarjan's strongly-connected-components algorithm on the directed graph and fires on the lexicographically-smallest SCC of size ≥ 3. Signature shape: `coordination_deadlock:<m1>:<m2>:<m3>...` with members sorted alphabetically. Cycle length is recoverable by counting colons.

When both a 2-cycle and an N≥3 cycle exist in the same topology, the 2-cycle wins. The 2-cycle is the simpler and more actionable signal, and demoting it would silently change behavior for existing customer dashboards that filter on specific 2-cycle signatures.

Self-loops (an agent handing off to itself) are excluded — that's not a deadlock, that's a single-agent recursion pattern caught by other detectors.

## What's usually happening

Coordination deadlocks always have an unwritten assumption about which role owns the in-flight work. A planner agent thinks the executor is supposed to make a decision; the executor thinks the planner is supposed to refine the plan first. Both roles politely defer, and the system makes no progress.

Three common shapes:

1. **Symmetric "I'll wait for you" roles.** Both agents have prompts that include "if you are unsure, hand off to the other agent." When both are unsure, both hand off, neither owns the decision. The roles are defined in a way that makes circular wait possible.

2. **Missing ownership at a specific decision point.** Most of the time the roles are clear, but for one class of decision (an edge case the prompt authors did not anticipate), neither role is supposed to handle it. Both delegate; the cycle forms.

3. **State the agents cannot see.** Agent A is waiting on B to produce X; agent B is waiting on A to produce Y. Neither agent knows the other is also waiting. The deadlock is implicit in the data flow, not in the prompt.

## How to investigate

Open the execution and look at the topology view. The agents that form the cycle are visible in the parent/child graph; for an N≥3 cycle the signature lists every member alphabetically so you can identify the participating roles directly. Read the `agent_handoff` events on each side — the `task_summary` field on each handoff usually reveals what was being asked for and why the recipient deferred.

Cross-reference with other recent `coordination_deadlock` failure_groups in this project. A single execution is investigation; many executions hitting the same agent pair (or SCC) is a systemic prompt-or-routing problem rather than a one-off bug.

## How to fix

The remediation depends on which shape:

- **Symmetric roles.** Pick a tie-breaker. Make one of the two roles the unambiguous owner for the decision class that triggered the deadlock. The prompt for the owning role should not include "hand off if unsure" for this decision; it should include "make a best-effort decision and document your uncertainty."

- **Missing ownership at a decision point.** Add an explicit branch to the prompts. Whichever role is invoked first for this decision class becomes the owner; the other role's prompt rejects this decision class outright. The branch may be ugly but it is determinate.

- **Implicit data dependency.** Make the dependency explicit and break the cycle. One side must produce its half before invoking the other. This usually requires re-thinking the agent decomposition, not just the prompts.

- **Customer-side guardrails (not detector features).** Some operators add a hop-count cap (refuse to hand off after N total handoffs in one execution) or a bounded N-pass loop (the orchestrator forces a termination after N rounds). Those guardrails live in customer code; they're worth considering as a safety net when prompt-level fixes don't fully eliminate the cycle.

## How to test the fix

After deploying, the deadlock failure_group should plateau. A subtler signal: average end-to-end latency for multi-agent runs in this project should drop, because executions that previously deadlocked were timing out, and the time-budget-exceeded executions were inflating the latency distribution.

## A note on detection scope

The detector consumes a flat slice of `HandoffEdge` records produced from `agent_handoff` events. It does not consume `checkpoint` events, so emitting per-handoff checkpoints will not change what the detector sees. Checkpoints are still useful for human debugging (they make the implicit handoff reasoning explicit when you read the execution timeline) but they are not part of the detector's input surface.

## Related detectors

- **`cascading_failure`** is the parallel multi-agent signal — instead of two agents waiting on each other (deadlock), one agent crashes and the parent inherits the failure (cascade). Both detectors consume the same `agent_handoff` events but answer different questions. If your topology has multiple handoffs and you're seeing one of these groups, check the other: a deadlock that times out can also trigger a cascading_failure when the timing-out side counts as a child terminal failure. Open both failure_groups for the same execution side-by-side to determine the root cause (deadlock first, cascade second is the usual reading).
- **`hitl_timeout`** is the human-in-the-loop sibling. When the cycle includes a step that asks a human reviewer to break the tie and the human never responds, you may see both a coordination_deadlock and an hitl_timeout. The hitl_timeout is usually the root cause and the deadlock the symptom.
