# Cost velocity

An execution in this project tripped one of Mesedi's two cost-velocity detectors. Both detectors fire under the same `cost_velocity` failure_class but answer different questions, and they can BOTH fire on the same execution.

## The two signals

**Absolute single-execution cost.** An individual execution's cost crossed the per-project `cost_velocity_threshold_usd` (default `$1.00`). Catches "this one run was unusually expensive." Signature buckets by order-of-magnitude USD:

- `cost_$0.001+`, between $0.001 and $0.01
- `cost_$0.01+`, between $0.01 and $0.10
- `cost_$0.10+`, between $0.10 and $1.00
- `cost_$1+`, between $1.00 and $10.00
- `cost_$10+`: $10.00 or more

**Sustained burn rate.** Mesedi sums execution costs over a rolling per-project window (default 5 minutes) and divides by the window. If the rate exceeds `cost_velocity_rate_threshold_usd_per_min` (default `$5/min`), the rate detector fires. Catches "you are sustaining burn above $X/minute": the production case where the per-execution cost is unremarkable but the volume of executions is. Signature buckets:

- `rate_$0.10+_per_min`, below $1/min
- `rate_$1+_per_min`, between $1 and $5/min
- `rate_$5+_per_min`, between $5 and $10/min
- `rate_$10+_per_min`, between $10 and $100/min
- `rate_$100+_per_min`, between $100 and $1000/min
- `rate_$1000+_per_min`, $1000/min or more

The bucket on this failure group tells you in dollars what your agent is spending. Use that as the triage signal, a `cost_$0.10+` group in a project where the median execution costs $0.001 means specific executions are 100× more expensive than the population, and those are where the spend leaks live.

## What's usually happening

Match the signature to the diagnosis:

- **`cost_$0.001+`, `cost_$0.01+`**: routine in production with the default threshold. If you see these regularly, lower your threshold to focus on what matters.

- **`cost_$0.10+`** usually means the agent made a lot of LLM calls OR used a high-context-window prompt. Combine this with the execution's `llm_call` count: many small calls vs few large calls have different root causes.

- **`cost_$1+`** is almost always a structural problem. Either the agent is in a loop (cross-reference with `loops/*` failure_groups on the same executions), the context window is growing unbounded across turns (scratchpad accumulation), or the agent is calling a much more expensive model than intended.

- **`cost_$10+`** in a single execution is rare and indicates an emergency-class bug. An infinite loop, a corrupted retry path, a model upgrade that nobody noticed. Look at the affected execution immediately.

- **`rate_*_per_min`** signatures point at volume, not magnitude. A `rate_$10+_per_min` group from a workload where individual executions cost cents means the call volume is the problem, not any one call. Cross-reference with the timeline of recent executions in the project: is there a spike, or is it sustained?

## How to find the bug

Open one of the affected executions. Three diagnostics, in order:

1. **The `llm_call` event count.** If the execution has 100 LLM calls and the bucket is `cost_$1+`, you're at ~$0.01 per call (normal for mid-tier models): the loop is the bug, not the per-call cost. If the execution has 3 LLM calls and the bucket is `cost_$1+`, each call cost ~$0.33: the prompt or response size is the bug, not the call count.

2. **The model field across calls.** Use the execution detail view to see distinct models. Unintended use of an expensive model is one of the most common cost-leak causes. If you see `claude-opus-*` in an execution that was supposed to use `claude-haiku-*`, that's an isolated config bug and easy to fix. If you see it mixed across many executions, you have a routing bug.

3. **Input/output token counts in the `llm_call` payload.** If `prompt_tokens` is growing across calls within the same execution, you have scratchpad accumulation: each turn re-sends a longer history. If `completion_tokens` is consistently maxed out, the model is producing verbose responses that should be capped.

## How to fix

The remediation depends on which of the three diagnostics flagged:

- **Too many calls (loop or over-deliberation).** Same fixes as the `loops/*` playbooks: cap iterations, hash-and-short-circuit duplicates, audit the planner's terminator condition. Cost velocity is often a downstream symptom of a loop; fixing the loop fixes the cost.

- **Wrong model.** Route deliberately. Use the cheapest model that works for each task. Gate routing decisions at the agent's entry point rather than letting individual code paths pick: that way the routing logic lives in one auditable place.

- **Growing context (scratchpad accumulation).** Cap the prompt's transcript length and summarize older turns. For Sonnet-class models, summarize every 20 turns; for smaller models, every 10. Trade some context precision for bounded cost.

- **Verbose responses.** Set `max_tokens` explicitly. Most defaults are too high. Make the model's job specific enough that long responses aren't needed, and cap the upper bound.

## Per-project tunables

Two dedicated endpoints, distinct from the generic detector_thresholds primitive (the cost-velocity wave shipped before that primitive existed):

- **`cost_velocity_threshold_usd`** (default `$1.00`; bounds [`$0.01`, `$10,000`]; no tier cap). Read/write via `GET/PUT /me/cost-velocity-config`. Lower it to catch cheaper anomalies; raise it on batch-tolerant workloads.
- **`cost_velocity_rate_threshold_usd_per_min`** + **`window_minutes`** (defaults `$5/min` over `5` minutes; threshold bounds [`$0.10`, `$10,000`] per minute; window bounds [`1`, `60`] minutes; no tier cap). Read/write via `GET/PUT /me/cost-velocity-rate-config`. The floor of `$0.10/min` is a storage-abuse guardrail: workloads spending less than that in total cannot trip the rate detector by design.

Both knobs defend against bad config by rejecting out-of-range values at write time with a 400. If the per-project read fails at execution close, the handler falls back to the package default and records a `config_fallback` system_event so persistent failures surface in the dashboard.

## How cost is computed

Mesedi computes cost server-side at execution close from per-event input/output token counts (shipped by the SDK) multiplied by a backend-maintained pricing table. Pricing changes ship with a backend deploy. No SDK release wait. The table version is exposed at `GET /me/pricing-info` so customers can verify which prices Mesedi is using. Models not in the backend pricing table fall back to the SDK-shipped per-event `estimated_cost_usd`; when that happens a `pricing_unknown_model` system_event surfaces the unrecognized identifier in the dashboard.

## A cost-aware product pattern

For high-volume products, instrument cost per request as a first-class metric in your own observability (not just Mesedi). Set a per-request soft budget and a per-request hard limit at the application layer. When the soft budget is hit, log a warning; when the hard limit is hit, return an error before making the call. Mesedi will still classify the run, but you'll prevent the most expensive failures from completing.
