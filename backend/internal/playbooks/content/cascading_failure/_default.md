# Cascading failure

The parent agent on this execution handed off work to a sub-agent, and the sub-agent reached a failure terminal state (`crashed`, `timeout`, or `validation_failed`). Mesedi noticed both halves and collapsed them into one logical failure_group attributed to the specific agent edge that broke, rather than producing two separate failure groups for the same logical bug.

The signature is `cascading_failure:<from_agent>:<to_agent>:<child_status>` so repeated cascades along the same edge dedupe into one group regardless of the specific execution_id pair involved.

## What's usually happening

The signature tells you which edge broke. From the parent's perspective, the work was delegated and the child did not return a useful result. From the child's perspective, the work crashed or timed out for its own reasons. The cascading_failure clustering says these are two views of the same incident.

Three common shapes:

1. **The child's bug is the real bug.** The parent correctly delegated work that the child was supposed to handle, and the child has a defect. The cascade is just a delivery vehicle for the underlying child crash.

2. **The parent gave the child a bad task.** The parent handed off a malformed prompt, an invalid input, or a task outside the child's competence. The child failed because the input was unworkable, not because the child is broken.

3. **A timeout race.** The parent expected the child to return within some window; the child took longer than the parent waited. The child's eventual completion may even be correct, but the parent gave up first.

## How to investigate

Open the parent execution. The topology view at the top of the page shows the parent + the failing child. Click into the child execution to see its own failure detail. The child will usually have its own failure_group as well (a `crashes` group if it crashed, a `time_budget` group if it timed out, a `validator_failures` group if its own validator rejected).

Read the `agent_handoff` event on the parent that points to the failing child. The `task_summary` field shows what the parent asked the child to do; cross-reference against the child's actual input to see if anything was lost in translation.

## How to fix

The remediation depends on the root cause:

- **Child bug.** Fix the child. The cascade alert is correct but the underlying issue lives in the child's code. Once the child stops failing, the cascade clears automatically.

- **Bad task from parent.** Validate the handoff payload in the parent before sending. If the parent is delegating malformed inputs, the parent's planner is the bug; tighten the prompt that produces the handoff or add a schema check on the task before invocation.

- **Timeout race.** Either raise the parent's wait window or make the handoff asynchronous. For "delegate" handoffs (synchronous, expect-a-return), the wait should be sized for the worst-case child latency plus a safety margin. For "spawn" handoffs (fire-and-forget), the parent should not block at all.

## How to test the fix

After deploying, look at the parent execution again or run an equivalent workload. The cascading_failure group should plateau. If the child fix worked, the child's own failure_group will also stop accumulating. If the cascade persists but the child's group does not, you may have fixed the symptom on the child side while still sending a bad task from the parent.

## Per-project tunables

Two knobs via the detector_thresholds primitive:

- **`cascade_window_seconds`** (default 86400 = 24 h; bounds [10, 86400]; no tier cap). Rows where `child_ended_at - handoff_emitted_at` exceeds this window are excluded from scoring. The default preserves the historical "no window" posture; lower it (e.g. 300 for 5 minutes) when long-lived spawn handoffs whose children fail hours later are showing up as false-positive cascades.
- **`exclude_spawn_handoffs`** (default `false`; boolean; no tier cap). When true, rows whose `handoff_kind = "spawn"` are skipped before scoring. Fire-and-forget spawns are arguably a supervision gap rather than a cascade: turn this on if your topology uses spawn semantics and the parent's success shouldn't depend on the spawn-child's outcome.

Both defaults preserve the original detector behavior so customers who don't tune see no change. Defensive: `cascade_window_seconds` outside [10, 86400] reverts to default at read time.

## A note on the topology view

The topology graph (visible on every execution detail page that participates in a multi-agent run) is the fastest way to diagnose cascades. The seed execution is highlighted in accent orange; failing ancestors and descendants get a red "flagged" chip linking to their own failure_group. Climbing the chain visually is faster than reading event timelines in isolation.
