# HITL timeout

A human-in-the-loop request on this execution either explicitly timed out (the host application gave up waiting before a human responded) or breached the customer-declared SLA (a human responded, but the wait exceeded `sla_seconds`). Both cases indicate that the human side of your HITL loop is dropping or stalling requests.

The signature is `hitl_timeout:explicit` for explicit timeouts and `hitl_timeout:sla_exceeded` for SLA breaches. **Both signatures can fire on the same execution.** When an execution has multiple `human_intervention` events and one fires explicit timeout while another breaches SLA, the detector promotes both signals to distinct failure_groups so neither cluster is suppressed by the other. Result ordering is deterministic: explicit appears before sla_exceeded when both fire.

## What's usually happening

The signature tells you which kind:

- **`explicit`** means the `response_kind` field on the human_intervention event was literally `"timeout"`. The host application called `handle.complete(response_kind="timeout", ...)` because some other code (a queue worker, a polling task, a UI timer) decided to give up. Mesedi sees the host's decision; the actual root cause lives in the host code.

- **`sla_exceeded`** means a human responded with approved, rejected, edited, or cancelled, but the elapsed `wait_duration_ms` was longer than `sla_seconds * 1000`. The agent proceeded with whatever the human said; the SLA was breached in passing. Only fires when the customer-declared SLA is positive (`sla_seconds > 0`); without a customer-declared SLA the detector cannot detect a breach.

## How to investigate

Open the execution and read the `human_intervention` event payload. Three fields tell most of the story:

1. **`wait_duration_ms`** is the actual time the request waited from `requested_at` to `decided_at`. The detector compared this to `sla_seconds * 1000`.

2. **`response_kind`** is what closed the request. `"timeout"` is the explicit signal; the other values mean a human did eventually respond.

3. **`decided_by`** identifies who responded (when supplied). Empty values for `decided_by` on timeout events often indicate the request never reached a human at all; an operator field with a real value but a long wait means the human was overloaded.

Cross-reference with other recent `hitl_timeout` failure_groups in this project. A single execution is an outlier; many executions across a window is a systemic capacity or routing issue.

## How to fix

The remediation depends on which kind fired:

- **Explicit timeout.** Find the code that called `handle.complete(response_kind="timeout")`. Three common causes:

  1. **Queue worker giving up too fast.** Your background worker that drains the HITL queue has a timeout shorter than the SLA. Raise it.

  2. **Notification channel broken.** The Slack message, email, or webhook that was supposed to alert the on-call human never fired. Test the channel end-to-end and add a health-check job that pings it periodically.

  3. **No one available.** The on-call rotation has gaps, or the people on call were not on duty when the request landed. Fix the rotation; add a secondary on-call.

- **SLA exceeded.** Either the SLA is too tight or the human response process is too slow. Triage:

  1. Look at the distribution of `wait_duration_ms` across recent HITL events. If the median is close to the SLA, raise the SLA. If the median is much lower than the SLA but the long tail spikes, focus on reducing the tail (better routing, faster paging, batched UI).

  2. Investigate the slow responders. If most response delay comes from a small group of decisions or a specific time-of-day window, the fix is structural (more capacity, async batching) rather than per-decision.

## How to test the fix

After deploying, watch new HITL executions in the same project. The hitl_timeout failure_group should plateau. The leading indicator that the queue-worker fix worked is that explicit timeout events stop landing. The leading indicator that the SLA fix worked is that `wait_duration_ms` percentiles tighten below the new SLA.

## Per-project tunables

One configurable knob via the detector_thresholds primitive:

- **`fire_modes`** (default `["explicit", "sla_exceeded"]`; closed set — only those two strings are valid). Controls which firing modes promote to failure_groups. Restrict to `["explicit"]` to mute SLA-exceeded clusters when SLA tracking lives in a different system. Restrict to `["sla_exceeded"]` to mute explicit-timeout clusters (rare — usually treated as control flow rather than an alert). Empty input or any value outside the closed set reverts the whole slice to the default (defensive fallback against bad config that escapes the validators registry).

The `sla_seconds` and `wait_duration_ms` themselves are not per-project tunables — they're per-handle on the customer's own SDK code via `request_human_intervention(sla_seconds=...)`.

## A note on what wait_duration_ms measures

`wait_duration_ms` is computed by the SDK at the moment `handle.complete(...)` is called: `decided_at` minus `requested_at`. It includes HTTP round-trip overhead from the original PATCH that opened the pause. The backend's `total_paused_ms` (visible on the execution detail page) is the backend's own measurement of the pause cycle and may differ slightly from `wait_duration_ms` by the round-trip latency. For SLA purposes, the SDK measurement is what the detector uses.
