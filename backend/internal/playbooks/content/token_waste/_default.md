# Token waste

The same leading prompt prefix (the first 2048 characters) was sent three or more times within a single execution. The agent is paying for the same tokens repeatedly. The signature is `token_waste:<prefix_hex8>` so distinct repeated prefixes cluster separately, and recurring patterns across executions surface clearly.

## What's usually happening

Three common causes, in rough order of frequency:

1. **Conversation history re-sent on every turn.** A ReAct-style or chat-style agent rebuilds the full prompt on each step, including the system prompt and conversation so far. The first 2048 characters (system prompt plus the opening of the conversation) are byte-identical across steps. Every call after the first is paying again for tokens you have already paid for.

2. **Retry logic that resends the full prompt.** A wrapper retries failed calls by re-running the exact same input. If the retry happens within the same execution and the prompt prefix matches, this detector fires alongside the `identical_call` detector.

3. **A shared system prompt across distinct calls in the same agent step.** The agent makes several independent LLM calls during one step (planner, summarizer, response synthesizer) and each call gets the same long system prompt. The fix is to use prompt caching or to factor the shared portion out.

## How to investigate

Open the execution and read the `llm_call` events. Compare the first 2048 characters of each `user_prompt` (or the combined system_prompt + user_prompt depending on your wire shape). If the prefix is byte-identical across three or more calls, the detector fired correctly. If the prefixes look different but the detector still fired, you may have whitespace, formatting, or invisible-character variance; tighten the prompt builder to ensure the prefix is canonical.

## How to fix

The remediation depends on which of the three causes:

- **History accumulation.** Use prompt caching. Anthropic's prompt caching and OpenAI's prompt caching both allow you to declare a stable prefix and pay for it once per cache lifetime instead of once per call. Anthropic charges roughly 10 percent of the normal input rate for cached tokens; OpenAI is similar. The savings are large when the prefix is large.

- **Retry resends.** The `identical_call` playbook applies. Stop re-running the same prompt and either fix the underlying cause of the failure or fall back to a degraded path.

- **Shared system prompt across multiple agent calls.** Factor the system prompt out, declare it as the cache-able prefix, and let provider-side caching avoid the repeated billing.

## How to test the fix

After deploying, look at the next few executions in the same project. The `token_waste` failure_group's affected_executions count should plateau (no new entries). The cumulative `input_tokens` on each execution should drop noticeably as well, and so should your bill.

## A note on cost vs latency

Prompt caching saves cost, but it also reduces time-to-first-token because the provider has already processed the prefix. The fix is doubly worthwhile if your application is latency-sensitive (chat, voice, real-time agents). For batch workflows the cost saving alone justifies it.
