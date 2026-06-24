# Coordination deadlock

Mesedi detected a 2-cycle in the agent_handoff graph within this execution's topology subtree. Agent A handed off to agent B, and agent B handed off back to agent A, in the same chain. Both roles are suspended waiting on each other to release control. This is the Coffman circular-wait condition expressed in agent-role terms.

The signature is `coordination_deadlock:<agent_a>:<agent_b>` with agents alphabetized so A↔B and B↔A collapse to the same failure_group regardless of which side fired the first handoff.

## What's usually happening

Coordination deadlocks always have an unwritten assumption about which role owns the in-flight work. A planner agent thinks the executor is supposed to make a decision; the executor thinks the planner is supposed to refine the plan first. Both roles politely defer, and the system makes no progress.

Three common shapes:

1. **Symmetric "I'll wait for you" roles.** Both agents have prompts that include "if you are unsure, hand off to the other agent." When both are unsure, both hand off, neither owns the decision. The roles are defined in a way that makes circular wait possible.

2. **Missing ownership at a specific decision point.** Most of the time the roles are clear, but for one class of decision (an edge case the prompt authors did not anticipate), neither role is supposed to handle it. Both delegate; the cycle forms.

3. **State the agents cannot see.** Agent A is waiting on B to produce X; agent B is waiting on A to produce Y. Neither agent knows the other is also waiting. The deadlock is implicit in the data flow, not in the prompt.

## How to investigate

Open the execution and look at the topology view. The two agents that form the cycle will be visible in the parent/child graph. Read the `agent_handoff` events on each side; the `task_summary` field on each handoff usually reveals what was being asked for and why the recipient deferred.

A useful diagnostic: emit a `checkpoint` event at every handoff decision point that records what the agent considered and why it chose to delegate. The checkpoints make the implicit reasoning explicit, and reading them in sequence usually pinpoints where the responsibility should have been claimed.

## How to fix

The remediation depends on which shape:

- **Symmetric roles.** Pick a tie-breaker. Make one of the two roles the unambiguous owner for the decision class that triggered the deadlock. The prompt for the owning role should not include "hand off if unsure" for this decision; it should include "make a best-effort decision and document your uncertainty."

- **Missing ownership at a decision point.** Add an explicit branch to the prompts. Whichever role is invoked first for this decision class becomes the owner; the other role's prompt rejects this decision class outright. The branch may be ugly but it is determinate.

- **Implicit data dependency.** Make the dependency explicit and break the cycle. One side must produce its half before invoking the other. This usually requires re-thinking the agent decomposition, not just the prompts.

## How to test the fix

After deploying, the deadlock failure_group should plateau. A subtler signal that the fix worked: average end-to-end latency for multi-agent runs in this project should drop, because executions that previously deadlocked were timing out, and the time-budget-exceeded executions were inflating the latency distribution.

## A note on detection scope

The detector finds 2-cycles only in v1 (A→B and B→A). Longer cycles (A→B→C→A) are possible in principle but rarer in practice and require Tarjan's algorithm to detect; v1 does not implement this. Most deadlocks customers see in real multi-agent systems are 2-cycles, so v1 covers the common case. If you suspect a longer cycle and the detector did not fire, open an issue with the topology graph as a starting point.

## Related detectors

- **`cascading_failure`** is the parallel multi-agent signal — instead of two agents waiting on each other (deadlock), one agent crashes and the parent inherits the failure (cascade). Both detectors consume the same `agent_handoff` events but answer different questions. If your topology has multiple handoffs and you're seeing one of these groups, check the other: a deadlock that times out can also trigger a cascading_failure when the timing-out side counts as a child terminal failure. Open both failure_groups for the same execution side-by-side to determine the root cause (deadlock first, cascade second is the usual reading).
- **`hitl_timeout`** is the human-in-the-loop sibling. When the cycle includes a step that asks a human reviewer to break the tie and the human never responds, you may see both a coordination_deadlock and an hitl_timeout. The hitl_timeout is usually the root cause and the deadlock the symptom.
