# Identical-call loop

Your agent made the **same LLM call (same model, exact same `user_message` text) three or more times** in a single execution. Mesedi's `identical_call` detector hashes `(model + user_message)` per `llm_call` event and flags executions where the same hash recurs 3+ times.

## What's usually happening

Three common causes, in rough order of frequency:

1. **Retry logic without backoff or jitter.** Your agent code or your LLM client wrapper is retrying a failed call by re-running the exact same prompt. If the failure is deterministic (model returns the same answer, your downstream code rejects it the same way), you get an infinite identical loop until something else stops it.

2. **Caching key collision.** Your agent's caching layer thinks two distinct requests are the same and serves the same prompt repeatedly because the cache key doesn't distinguish them. Common when the cache key is built from request metadata that's identical across user requests but the user-specific context lives in a separate field the key ignores.

3. **State-machine bug.** Your agent's planning loop is supposed to advance through different prompts but the state-advance condition is broken, so it keeps re-running the first prompt without progressing.

## How to find the bug

Open the affected execution's event timeline (link in the Affected executions table below) and look at the `llm_call` events. You'll see the same `user_message` text three or more times in a row. The first place to look is your retry path, somewhere in your agent code, an LLM call is being invoked from inside a loop with a condition that doesn't change between iterations.

A useful debugging hack: temporarily log the call site for every LLM invocation. If all the duplicate calls come from the same stack frame, you've found a retry bug. If they come from different stack frames, you've found a state-machine bug.

## How to fix

The remediation depends on the cause, but four standard patterns:

- **Add exponential backoff with jitter** to your retry path. If you're using `tenacity` or similar, that's `wait_exponential_jitter(initial=1, max=30)`. The jitter prevents thundering-herd retries from multiple concurrent agents.

- **Cap retry attempts at 3 or 5.** Unlimited retries are almost never the right answer. After N attempts return an error or fall back to a degraded path.

- **Make the retried prompt actually different.** If your agent is retrying because the LLM's first response was unsatisfactory, the next prompt should include the previous response and ask for a refinement. Identical retries on identical inputs almost always give identical outputs, the agent is wasting tokens.

- **Fix the cache key.** If caching is the culprit, the key must include every field that materially distinguishes one request from another, user_id, session_id, the full text of the prompt, model name, temperature, system prompt hash.

## Auto-fix in a future Mesedi release

The Mesedi v2 roadmap (see `docs/REPAIR_TIER_ROADMAP.md`) includes SDK-layer dedup for this pattern: when the same `(model, user_message)` hash recurs within an execution, the SDK can transparently return the cached first-call response instead of re-calling the model. That saves tokens and breaks the loop simultaneously, without changing your agent code. Opt-in per project per class, never on by default.
