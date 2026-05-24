# Tool failure

A `tool_call` event in this execution recorded `status=failed`. Mesedi's tool-failure detector groups affected executions by **tool name**, the signature on this failure group IS the name of the tool that failed. That's deliberate: tool failures cluster by the tool they happen to, not by the exception text or the agent that invoked them, because the fix almost always lives in the tool's client code or its upstream dependency.

This is different from a `crashes/*` classification. Crashes happen when an exception escapes the `@wrap` boundary and the execution terminates abnormally. Tool failures here are the **silent-degradation** pattern: the tool call raised, the agent caught the exception (or the SDK swallowed it), and the execution ran to completion producing degraded output. Crashes are loud; tool failures are quiet, which is exactly why a separate failure class exists for them.

## Why this matters more than it looks like it does

When an agent recovers from a tool failure and keeps going, three bad things happen at once:

1. **The output is wrong but no one notices.** The agent has produced an answer based on whatever fallback path it took, no API result, stale cache, made-up data. The end user gets a confident-sounding response that's missing the data that should have shaped it.

2. **The failure mode is invisible from the outside.** Application logs show "execution completed successfully." The Mesedi tool-failure classification is often the only place this surfaces.

3. **The pattern repeats silently across many executions before anyone catches it.** Look at the affected-executions count on this failure group, that's how many runs produced degraded output before you found it.

## How to find the bug

Open one of the affected executions in the timeline. Three places to look:

1. **The `tool_call` event itself.** Its payload includes the tool name, the arguments, the failure type, and (often) an error message. The error message is your first hint at whether this is a transient or permanent failure, `connection refused` and `503 Service Unavailable` are usually transient; `401 Unauthorized`, `permission denied`, and `schema validation failed` are usually permanent.

2. **What the agent did NEXT.** Scroll past the failed `tool_call`. The agent's next action tells you which fallback path fired. If the next event is another `tool_call` to the same tool, retry-without-fixing is the bug. If the next event is an `llm_call` that asks the model to "answer without the data," you have silent-degradation explicitly coded into your agent. If there's no next event and the execution just ended, the agent gave up, better than degrading, worse than recovering.

3. **The cross-execution pattern.** Click into 2-3 different affected executions. If the failure happens at the same step in every run, the bug is structural, the tool is always broken in this code path. If the failure happens at different steps with different arguments, the bug is in the tool's client (timeout, retry, error-handling) and only manifests under specific conditions.

## How to fix

The remediation pattern depends on what kind of failure this tool is producing:

- **Transient failures (network, rate limit, 5xx).** Wrap the tool client with retry + exponential backoff + jitter. Cap attempts at 3-5. If retries are exhausted, surface a structured error to the agent so it can either pick a different tool or escalate to a human, rather than silently degrading.

- **Permanent failures (auth, schema, permission).** Don't retry. Fix the underlying config, rotate the credential, update the schema, fix the permission. Add a startup health-check that calls the tool with a known-safe argument and refuses to start if it fails; that catches the config bug at boot time instead of at request time.

- **Argument-construction failures.** If the tool is failing because the agent is passing bad arguments, the bug is upstream of the tool, in the prompt that asks the LLM to generate the arguments, or in the schema validation between the LLM output and the tool call. Tighten the validator and feed validation failures back into the prompt as a corrective signal.

- **Silent-degradation by design.** If your agent's intentional behavior is "if this tool fails, answer without it," that's a product decision, not a bug, but it should be EXPLICIT. Emit a custom event (`degraded_response` or similar) so Mesedi can group these separately from accidental tool failures, and so your downstream consumers can flag the response as "this answer was produced without [tool]."

## Auto-fix in a future Mesedi release

The v2 roadmap (`docs/REPAIR_TIER_ROADMAP.md`) includes per-tool playbook overrides, when your agent's `slack.post_message` tool fails routinely with `channel_not_found`, the playbook can be Slack-specific instead of generic. The pattern table in `playbooks.go` already supports prefix-based overrides; the gating constraint is authoring the per-tool content. The current `_default.md` (this file) covers the general pattern; tool-specific entries get added as the customer base reveals which tools fail most.
