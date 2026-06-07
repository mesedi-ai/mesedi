# Context overflow

The cumulative input-token count on this execution crossed the model's configured context window. The signature is `context_overflow:<level>:<model>` where `<level>` is `warn` (at 90 percent of the window) or `fail` (at or above 100 percent).

When the input prompt exceeds the window, the provider almost certainly truncated the prompt. The model is responding to a prompt that is missing some of what you sent it, most often the early portion of the system instructions or the conversation history.

## What's usually happening

Three common causes, in rough order of frequency:

1. **Conversation history accumulation.** A chat-style agent appends every turn to the running history and re-sends the full history with each new turn. After enough turns the history alone exceeds the window. This is the single most common cause and applies to ReAct-style scratchpads as well as user-facing chat sessions.

2. **Retrieved-context bloat.** A RAG pipeline pulls back too many or too long documents and stuffs them all into the prompt without filtering or summarizing. The retrieval works correctly; the synthesis stage is poorly bounded.

3. **Unexpectedly small window for the model in use.** You switched to a model with a tighter window (smaller variant, cheaper tier, fallback model) and your prompt-builder did not adjust. Mesedi's model registry knows the window for Claude (200K), GPT-5 (400K), Gemini (2M), Llama-4-scout (10M), and Mistral variants; the signature includes the model name so you can confirm which one was in use.

## How to investigate

Open the execution and look at the `llm_call` events in order. The `input_tokens` value on each call shows how much you sent; cumulative input is the sum across calls in the same execution. When the cumulative sum approaches the window value for the model in the signature, you have your overflow.

Look at the largest single `input_tokens` value. If one call is responsible for most of the overflow, that single call is the place to optimize. If many calls are each medium-sized, you have accumulation rather than a single bloated prompt.

## How to fix

The remediation depends on which of the three causes:

- **History accumulation.** Cap the running history at the most recent N turns and summarize older turns into a single compact note. For Sonnet-class models, every 20 turns is a reasonable summarization cadence; for smaller models, every 10. Trade some semantic precision for bounded prompt length.

- **RAG bloat.** Cap the number of retrieved chunks and the length per chunk. Re-rank with a smaller model and discard low-scoring results. If full chunks are needed, summarize before injection.

- **Wrong-model assumption.** Either switch to a model with a larger window or tighten the prompt-builder to the actual window of the current model. Mesedi's model registry is the source of truth; consult it before assuming a value.

## How to test the fix

After deploying, look at the next executions in the same project. The `input_tokens` cumulative on each execution should stay well below the window value. Mesedi will continue to issue `context_overflow:warn` for runs that approach 90 percent; you can use the warn signal as a leading indicator before you start truncating.

## A note on what truncation looks like

Providers do not always announce truncation explicitly. The output may look coherent because the model is filling in plausible responses to a prompt missing its earlier sections. The customer-visible symptom is usually subtle quality drift rather than an outright error: the agent ignores instructions you thought you gave it, or forgets context from earlier in the session. If your agent's outputs degrade over a long conversation, context overflow is a likely culprit.
