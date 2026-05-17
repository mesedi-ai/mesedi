# Step-count exceedance

Your agent emitted **more telemetry events in a single execution than Mesedi expects for healthy agent behavior**. The detector buckets executions by total event count:

- `step_count_10+` — 10 to 49 events
- `step_count_50+` — 50 to 99 events
- `step_count_100+` — 100 to 499 events
- `step_count_500+` — 500 to 4,999 events
- `step_count_5000+` — 5,000 or more events

Threshold is artificially low (10 events) in v0.0.1 for local-dev visibility. The production default is 50+, configurable per project once the projects table has per-project policy columns.

The class is `loops` because high step count is the most reliable structural symptom of a loop — an agent that's making real progress almost always converges in a small number of steps. The detector runs last in the chain, so an execution that crashed, took too long, AND emitted lots of events classifies as crashes (higher priority); this row exists only for the executions that didn't crash, didn't time out, and were nominally "successful" but spent way too many steps getting there.

## What's usually happening

The bucket tells you a lot:

- **10+ and 50+** are usually fine in production with a tuned threshold, but in local-dev mode (threshold 10) you'll see these from any non-trivial agent. Look at the `llm_call` count vs the `tool_call` count — if they're roughly balanced, the agent's doing real work. If `llm_call` >> `tool_call`, the agent is doing too much thinking and not enough acting.
- **100+** is the interesting bucket. Most well-designed agents complete in 15-40 steps. 100+ usually means either a planning loop that's not converging (the agent keeps re-deliberating instead of committing to a plan), or a tool-call retry path that's spinning.
- **500+** and **5000+** are almost always a structural bug. An agent emitting 500+ events per execution is either in a true infinite loop bounded only by `max_iterations`, or your event emission is double-firing (the SDK is being initialized twice in the same process, or your custom event emission is duplicating).

## How to find the bug

Open the execution's event timeline. Three diagnostics:

1. **Look at the event-type histogram.** If 80%+ of the events are `llm_call`, the agent is over-deliberating. If 80%+ are `tool_call`, the agent has a tool-retry loop. If 80%+ are a custom event type, you have an event-emission bug — that custom event is being fired in a tight loop.

2. **Look at the timestamp deltas.** Healthy agent events have varying gaps between them (some 100ms, some 5s). Pathological loops have suspiciously uniform gaps — usually means the loop body is doing the same thing each iteration.

3. **Look for `identical_call` or `similar_call` failure groups on the same execution.** If step-count fired AND one of the call-loop detectors fired, you have direct confirmation that the loop is in the LLM-call path. If step-count fired alone, the loop is somewhere else — tool calls or custom logic.

## How to fix

The remediation depends on what kind of step is dominant, but the universal patterns:

- **Add a hard step budget.** Every agent loop should have a `max_steps` parameter with an explicit error at the limit. 50 is a reasonable production default; 100 if your agent does genuinely complex multi-stage reasoning. Anything that needs more than 200 should be decomposed into separate executions.

- **Bound the planning loop.** If the agent's pattern is "plan → act → re-plan → act → re-plan → ...", cap the number of re-plans before forcing a commit-to-best-plan or escalating to a human. Common with ReAct-style agents that don't have an explicit "I've planned enough" signal.

- **Audit tool-call retries.** A tool that fails 5 times shouldn't retry 50 times. Wrap external tools with `tenacity` or equivalent, cap attempts at 3-5, use exponential backoff with jitter, fall back to a degraded path on persistent failure.

- **De-duplicate event emission.** Verify your SDK is only initialized once per process. Check that any custom event-emitting wrappers aren't accidentally being applied at both the function and decorator level.

## How to test the fix

After deploying, run the same workload. The relevant signals:

- The bucket should shift down. An execution that landed in `step_count_500+` should land in `step_count_100+` or `step_count_50+`.
- The total event count on the dashboard should drop proportionally — Mesedi's storage cost scales with event count, so this is also a quiet cost-control win.
- If you added a hard step budget, you'll see `halt:local_signal` crash signatures appearing in the dashboard at the budget threshold. That's the budget working as intended.

## Auto-fix in a future Mesedi release

The v2 roadmap includes Mesedi-side step-budget enforcement — the SDK accepts a `max_steps` parameter and emits a halt signal when it's crossed, the same way time-budget enforcement works. Opt-in per project, off by default.
