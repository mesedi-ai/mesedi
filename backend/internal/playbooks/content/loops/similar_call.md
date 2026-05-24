# Similar-call loop

Your agent made **three or more LLM calls whose `user_message` text was nearly identical**, same prompt structure, only minor field-level edits between calls. Mesedi's `similar_call` detector computes char-3-gram cosine distance between every pair of `llm_call` events in the execution and flags clusters where 3+ messages fall within a 0.20 distance of each other. The signature hash is derived from the cluster's dominant trigrams, so different stuck patterns across different executions aggregate as different failure groups.

## What this catches that `identical_call` misses

`identical_call` only fires on byte-exact repetition. `similar_call` fires on the much more common pattern where the call template is the same but one slot varies between iterations:

- **Timestamp substitution.** `"Fetch metrics at 2026-05-15T10:00:00Z"` → `"Fetch metrics at 2026-05-15T10:00:01Z"` → `"Fetch metrics at 2026-05-15T10:00:02Z"`. The agent is retrying the same conceptual call with a wall-clock that ticks.
- **ID iteration.** `"Look up customer cust-A1 in CRM"` → `"Look up customer cust-A2 in CRM"` → `"Look up customer cust-A3 in CRM"`. A pagination or enumeration loop that should have hit a terminator and didn't.
- **Schema variable that doesn't actually change behavior.** `"Generate report for NDA clause 4.1"` → `"...clause 4.2"` → `"...clause 4.3"`. The agent thinks it's making progress because the variable changed, but the underlying behavior is identical and it's just burning tokens.

## What this does NOT catch

True semantic paraphrases, `"Extract the date"` vs `"Find the date mentioned"` vs `"What date appears in this doc"`, usually share too few char-3-grams to cluster, even though a human reads them as the same intent. Semantic-paraphrase detection requires embedding similarity, which is on the roadmap once Mesedi has an embeddings substrate.

## How to find the bug

Open the execution's event timeline and scan the `user_message` field across the `llm_call` events. The cluster will be visually obvious: three or more rows where the text is 90%+ the same and only one field changes between them. The field that's changing IS the bug, that's the loop variable.

Three common shapes:

1. **A retry loop with a varying timestamp or correlation ID.** The retry path is re-issuing the same call, the only thing that varies is metadata the LLM doesn't actually use for routing its answer. The model returns roughly the same response each time, the downstream code rejects it the same way, and the loop continues.

2. **An iterator that never terminates.** A `while next_page:` or `for id in queue:` loop where the queue is being refilled by the loop body itself, or where the terminator condition is never met. Common with pagination over a list whose length the agent never bounded.

3. **A template variable that's changing but shouldn't affect routing.** The agent's planner is producing different prompts but they all flow into the same downstream behavior because the varying slot is decorative, not load-bearing.

## How to fix

The remediation depends on which shape it is, but the four standard patterns:

- **Bound the iteration.** If this is a pagination or enumeration loop, add an explicit upper bound (`max_iterations=10`) and an error path when it's exceeded. The terminator condition is broken in some way; the bound is the guardrail.

- **Hash the materially-meaningful prompt fields and short-circuit on repeat.** Build a small in-execution cache keyed on the SHA-256 of `(model + user_message_normalized)` where `user_message_normalized` strips out timestamps and correlation IDs. When the same hash recurs, you've detected the loop in your own agent code and can break out before Mesedi has to flag it after the fact.

- **Make the retried prompt include the previous response.** If the agent's retrying because the prior answer was unsatisfactory, the next prompt should include the previous response and ask for a refinement. Near-identical prompts with no feedback signal almost always produce near-identical responses.

- **Audit the loop's terminator.** Whatever condition is supposed to make the loop stop, log it on every iteration and watch what happens. Most "infinite loop" bugs are a single-character typo in the terminator condition or a variable that's being mutated in the wrong scope.

## Auto-fix in a future Mesedi release

The v2 roadmap (`docs/REPAIR_TIER_ROADMAP.md`) includes SDK-layer near-duplicate dedup as a Tier 3 capability: when the SDK observes `similar_call` clustering inside a live execution, it can either short-circuit the loop with an error or return the first call's response for the duplicates. Opt-in per project per class, never on by default, false positives on this would silently corrupt agent behavior.
