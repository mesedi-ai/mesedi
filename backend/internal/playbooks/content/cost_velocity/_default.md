# Cost velocity

An execution in this project incurred **more cost than the cost-velocity threshold** in a single run. Mesedi's cost-velocity detector buckets executions by order-of-magnitude USD cost:

- `cost_$0.001+` — between $0.001 and $0.01
- `cost_$0.01+` — between $0.01 and $0.10
- `cost_$0.10+` — between $0.10 and $1.00
- `cost_$1+` — between $1.00 and $10.00
- `cost_$10+` — $10.00 or more

The threshold is intentionally low ($0.001) in v0.0.1 so the detector is visible on local-dev traffic. Production deployments will either raise the absolute threshold OR move to baseline-relative detection (Phase 5+: "this execution cost N× the project's median," which is the more useful signal once enough traffic exists to compute a baseline).

The bucket on this failure group tells you, in dollars, what your agent is spending per execution. Use that as the triage signal — a `cost_$0.10+` group in a project where the average execution costs $0.001 means specific executions are 100× more expensive than the population, and those are where the spend leaks live.

## What's usually happening

The bucket tells you what to look for:

- **$0.001+ and $0.01+** are routine in production with a sane threshold. In local-dev with the threshold this low, almost every non-trivial execution lands here. Real signal starts at $0.10+.

- **$0.10+** usually means the agent made a lot of LLM calls OR used a high-context-window prompt. Combine this signal with the execution's `llm_call` count: many small calls vs few large calls have different root causes.

- **$1+** is almost always a structural problem. Either the agent is in a loop (cross-reference with `loops/*` failure groups for the same executions), the context window is growing unbounded across turns (scratchpad accumulation), or the agent is calling a much more expensive model than intended.

- **$10+** in a single execution is rare and indicates an emergency-class bug. An infinite loop, a corrupted retry path, a model upgrade that nobody noticed. Look at the affected execution immediately.

## How to find the bug

Open one of the affected executions. Three diagnostics, in order:

1. **The `llm_call` event count.** If the execution has 100 LLM calls and the bucket is `cost_$1+`, you're at ~$0.01 per call which is normal for mid-tier models — the loop is the bug, not the per-call cost. If the execution has 3 LLM calls and the bucket is `cost_$1+`, each call cost ~$0.33 — the prompt or response size is the bug, not the call count.

2. **The model field across calls.** Use the execution detail view to see distinct models. Unintended use of an expensive model is one of the most common cost-leak causes. If you see `claude-opus-4-6` in an execution that was supposed to use `claude-haiku-4-5`, that's an isolated config bug and easy to fix. If you see it mixed across many executions, you have a routing bug.

3. **Input/output token counts in the `llm_call` payload.** If `prompt_tokens` is growing across calls within the same execution, you have scratchpad accumulation — each turn re-sends a longer history. If `completion_tokens` is consistently maxed out, the model is producing verbose responses that should be capped.

## How to fix

The remediation depends on which of the three diagnostics flagged:

- **Too many calls (loop or over-deliberation).** Same fixes as the loops/* playbooks — cap iterations, hash-and-short-circuit duplicates, audit the planner's terminator condition. Cost velocity is often a downstream symptom of a loop; fixing the loop fixes the cost.

- **Wrong model.** Route deliberately. Use the cheapest model that works for each task. For routing decisions, gate them at the agent's entry point rather than letting individual code paths pick: that way the routing logic is in one auditable place.

- **Growing context (scratchpad accumulation).** Cap the prompt's transcript length and summarize older turns. For Sonnet-class models, summarize every 20 turns; for smaller models, every 10. Trade some context precision for bounded cost.

- **Verbose responses.** Set `max_tokens` explicitly. Most defaults are too high. Make the model's job specific enough that long responses aren't needed, and cap the upper bound.

## A cost-aware product pattern

For high-volume products, instrument cost per request as a first-class metric in your own observability (not just Mesedi). Set a per-request soft budget and a per-request hard limit at the application layer. When the soft budget is hit, log a warning; when the hard limit is hit, return an error before making the call. Mesedi will still classify the run, but you'll prevent the most expensive failures from completing.

## Auto-fix in a future Mesedi release

The v2 roadmap (`docs/REPAIR_TIER_ROADMAP.md`) includes per-project cost budgets enforced via the same halt mechanism as time and step budgets — the SDK can pre-flight estimated cost for each LLM call and halt the execution if the budget would be exceeded. Opt-in per project, off by default. The Tier 2 layer also includes "this execution cost N× the project median — here's a candidate fix" suggestions, which require having a baseline and so are deferred until the cost-history surface has enough data.
