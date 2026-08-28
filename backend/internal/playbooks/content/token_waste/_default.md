# Token waste

A leading prompt prefix was sent to the model three or more times within a single execution. The agent is paying for the same tokens repeatedly. The signature is `token_waste:<prefix_hex8>` so distinct repeated prefixes cluster separately, and recurring patterns across executions surface clearly.

The detector runs a three-layer pipeline on the `user_message` field of each `llm_call` event:

1. **Variable-prefix strip.** Normalizes five known drifting prefix shapes that would otherwise fragment the cluster: ISO-8601 / RFC3339 timestamps, hex UUIDs (8-4-4-4-12 or naked 32-hex), `req_id` / `trace_id` / `correlation_id` key-value prefixes, leading numeric counters, and labeled counters like `Turn 12:` or `Step 3:`. Multi-layer prefixes (e.g. timestamp followed by request_id followed by turn counter) get stripped iteratively up to 8 rounds.
2. **Exact 2048-char SHA-256 hash** on the normalized text. When three or more calls share the same hash, fires under `token_waste:<hex8>`.
3. **Shingle-Jaccard near-duplicate fallback.** Runs ONLY when the exact-hash path found no match. Builds k=8 character shingles per normalized payload, computes pairwise Jaccard, and fires when three or more payloads share Jaccard ≥ 0.85 with each other. Catches the structurally-similar-but-lexically-distinct cases the strip can't reach (variable material mid-prefix, conversation-history drift inside the 2048-char window). Fires under a separate `token_waste:near_dup:<hex8>` signature so it doesn't pollute the exact-match clusters.

The detector emits **one signature per execution**, either an exact match or a near_dup, never both. The near_dup suffix is deterministic across re-runs: same payloads produce the same hex8.

## What's usually happening

Three common causes, in rough order of frequency:

1. **Conversation history re-sent on every turn.** A ReAct-style or chat-style agent rebuilds the full prompt on each step, including the system prompt and conversation so far. The first 2048 characters (system prompt plus the opening of the conversation) are byte-identical across steps. Every call after the first is paying again for tokens you have already paid for.

2. **Retry logic that resends the full prompt.** A wrapper retries failed calls by re-running the exact same input. If the retry happens within the same execution and the prompt prefix matches, this detector fires alongside the `identical_call` detector.

3. **A shared system prompt across distinct calls in the same agent step.** The agent makes several independent LLM calls during one step (planner, summarizer, response synthesizer) and each call gets the same long system prompt. The fix is to use prompt caching or to factor the shared portion out.

## How to investigate

Open the execution and read the `llm_call` events. Compare the `user_message` field across calls: that is the exact field the detector reads. (If you previously looked at `user_prompt`, that field name is not what the SDK emits; the SDK ships `user_message` and the detector reads `user_message`.) If the first 2048 characters look byte-identical across three or more calls, the exact-match path fired. If the calls look near-identical but differ in a UUID, timestamp, or customer ID mid-prefix, the near_dup path fired.

If the signature is `token_waste:near_dup:<hex8>`, the calls are structurally similar at the shingle level (≥ 0.85 Jaccard on k=8 character shingles) but didn't pass the exact-hash test. This is normal for prompts whose variable material lives mid-prefix rather than at the leading edge.

## How to fix

The remediation depends on which of the three causes:

- **History accumulation.** Use prompt caching. Anthropic's prompt caching and OpenAI's prompt caching both allow you to declare a stable prefix and pay for it once per cache lifetime instead of once per call. Anthropic charges roughly 10 percent of the normal input rate for cached tokens; OpenAI is similar. The savings are large when the prefix is large.

- **Retry resends.** The `identical_call` playbook applies. Stop re-running the same prompt and either fix the underlying cause of the failure or fall back to a degraded path.

- **Shared system prompt across multiple agent calls.** Factor the system prompt out, declare it as the cache-able prefix, and let provider-side caching avoid the repeated billing.

## How to test the fix

After deploying, look at the next few executions in the same project. The `token_waste` failure_group's affected_executions count should plateau (no new entries). The cumulative `input_tokens` on each execution should drop noticeably as well, and so should your bill.

## Per-project tunables

Two thresholds are configurable per-project via the detector_thresholds primitive:

- **`prefix_window_chars`** (default 2048; hard bounds [64, 65536]; tier-capped: Hobby 4 KB, Team 16 KB, Enterprise 64 KB). Bigger window means hashing more text per event AND a larger shingle set on the near-duplicate fallback, both real CPU vectors on the detector hot path, hence the tier discrimination.
- **`min_repeats`** (default 3; bounds [2, 100]). Pure alerting sensitivity, no tier cap. Lower it to catch shorter loops; raise it to ignore agents that legitimately retry twice before succeeding.

Both thresholds defend against bad config that escapes the validators registry: values below the lower bound revert to the historical default rather than erroring or no-op'ing.

## A note on cost vs latency

Prompt caching saves cost, but it also reduces time-to-first-token because the provider has already processed the prefix. The fix is doubly worthwhile if your application is latency-sensitive (chat, voice, real-time agents). For batch workflows the cost saving alone justifies it.
