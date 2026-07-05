# v0.7.0 — LangChain.js callback handler adapter

Adds a first-class **LangChain.js** integration. Wire it into any
LangChain runnable and every LLM invocation and tool call flows into
Mesedi with no changes to your agent code beyond attaching the
handler.

## LangChain

- **`MesediLangChainCallbackHandler`** extends LangChain's
  `BaseCallbackHandler`. Attach an instance to any runnable's
  `callbacks:` slot; LangChain propagates it to every nested runnable
  automatically.
- Wrap your entry function with `mesedi.wrap()` (which owns the
  execution boundary) and the handler emits `llm_call` and
  `tool_call` events for the LLM and tool invocations inside.
- Extractors handle LangChain's `Serialized` + `LLMResult` shape
  churn across versions — `kwargs.model` → `id[]` → `name` → fallback
  chain for model identification; `text` vs `message.content` vs
  multi-modal blocks for response text; `tokenUsage` /
  `token_usage` / `usage` / newer `usage_metadata` for token counts.
- Truncation budgets match the Python langchain adapter and every
  other TS adapter, so backend detectors see one unified event
  stream regardless of source.
- Fail-open per-method try/catch; outside a `wrap()` execution the
  handler silently no-ops.
- Docs: https://mesedi.ai/docs/integrations/langchain

## Compatibility

- **Node 18+** minimum.
- New optional peer dependency: **`@langchain/core >= 0.3.0 < 0.5.0`**.
  Customers who never import `mesedi/integrations/langchain` never
  pay the install cost.
- Existing `wrap()` / `tool()` call sites and the LangGraph, OpenAI
  Agents, Vercel AI SDK, and Mastra adapters keep working without
  modification.
