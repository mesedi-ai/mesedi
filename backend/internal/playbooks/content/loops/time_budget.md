# Time-budget exceedance

Your agent ran for **longer than the time-budget threshold** before terminating. Mesedi's time-budget detector buckets executions by total wall-clock duration:

- `time_budget_1s+` — 1s to 10s
- `time_budget_10s+` — 10s to 1min
- `time_budget_60s+` — 1min to 10min
- `time_budget_10m+` — 10min to 1hr
- `time_budget_1h+` — 1hr or longer

The threshold is intentionally low (1 second) in local-dev so you see the detector working on your synthetic traffic. The production default is 60 seconds, and once per-project policy lands you'll be able to tune the threshold by class of agent — `quick_lookup` agents budget tight, `deep_research` agents budget loose.

The class is `loops` because slow executions almost always come from one of the loop patterns, not because Mesedi knows for certain this is a loop. Use the bucket as a triage signal — the wider the bucket, the more likely the agent is genuinely stuck rather than just doing real work.

## What's usually happening

The bucket tells you what to look for:

- **1s+** is mostly noise in local-dev (the threshold is tight). In production with a sane threshold this bucket would be empty; if you see it after raising the threshold, look at network latency in your LLM client and at sync I/O blocking the event loop.
- **10s+** and **60s+** are the interesting buckets. These usually mean the agent made more LLM calls than necessary — combine this signal with the execution's `llm_call` count to confirm. If the count is also high, look for `identical_call` or `similar_call` in the same execution's failure-group history.
- **10m+** and **1h+** mean something is genuinely wrong. Either the agent is in a tight infinite loop (check step-count too), or it's waiting on a remote resource that never responded (a tool call that didn't time out, a vector-store query against a non-existent index, an OAuth token refresh against a revoked credential).

## How to find the bug

Open the execution's event timeline. Three places to look:

1. **The gap between the last event and the terminal status.** If there's a 9-minute silence between the last `llm_call` and the `execution_completed`, the agent was waiting on something — probably a tool call that didn't have a timeout. Trace which tool was called last and check its client config.

2. **The density of `llm_call` events.** If you see 50 LLM calls in 10 minutes that's roughly 12 seconds per call, which is normal. If you see 5 LLM calls in 10 minutes, each LLM call took 2 minutes on average — that's a sign of a runaway streaming response or a context window that's grown so large the API is struggling to process it.

3. **The size of the `llm_call.payload.user_message` field as the execution progressed.** If the user_message is growing linearly with step count, you've got context-accumulation runaway — the agent is appending to a running transcript and the prompt has gotten so long that each call takes proportionally longer. Common with naive ReAct loops that don't summarize their scratchpad.

## How to fix

Three remediation patterns, in rough order of how often they apply:

- **Add a hard time budget to the agent.** A `with timeout(seconds=120):` or `asyncio.wait_for(coro, timeout=120)` around the agent's main loop. When the budget is exhausted, raise an explicit `TimeBudgetExceeded` exception that the caller can catch and degrade gracefully. Mesedi's hard-halt SDK helper does this for you in the same wire format the dashboard's Halt button uses; see the `halt_on_time_budget` parameter on `@wrap`.

- **Add per-tool timeouts.** Every external HTTP client should have an explicit timeout. The default in `requests` and `httpx` is no timeout — that's the wrong default for an agent. 30 seconds per tool call is a reasonable starting point; tune from telemetry.

- **Summarize the scratchpad.** If the agent's prompt is growing with each step, insert a summarization pass every N steps. The exact N depends on your model — for Sonnet, every 20 steps is plenty; for smaller models, every 10. Trade some semantic precision for bounded prompt length.

## How to test the fix

After deploying the fix, run the same workload and check the Mesedi dashboard a few minutes later. The relevant signals:

- The bucket should shift down. An execution that used to land in `time_budget_10m+` should now land in `time_budget_60s+` or below.
- The `failure_group` row's `execution_count` for the original bucket should plateau (no new entries).
- If you added a hard time budget, you'll see `halt:local_signal` `crashes/*` failure groups appearing — that's the timeout working as intended, and it's a much better failure mode than runaway duration.

## Auto-fix in a future Mesedi release

The v2 roadmap includes Mesedi sending the SDK a `halt` signal automatically when an execution exceeds a configured time budget, without the customer having to wire it in. This is the dashboard's Halt button on autopilot — same wire format, same SDK consumer side, just policy-driven instead of operator-driven. Opt-in per project, off by default.
