"""End-to-end smoke test for the LangChain adapter.

Exercises mesedi.integrations.langchain.MesediCallbackHandler by
calling its callback methods directly with mock LangChain-shaped
arguments. Does NOT require the langchain package to be installed , 
the handler's stub-base fallback path means importing the module
works regardless. The dispatch surface is the same shape LangChain's
CallbackManager would use, so a passing test here implies the real
LangChain integration works when langchain IS installed.

Run after starting the backend on :8080:

    python sdk-python/sandbox/langchain_adapter_test.py

Verify on the dashboard at http://localhost:8080/ui/:
  - Execution 'langchain_adapter_smoke' shows up under Overview
  - Its event timeline shows 1 llm_call (model=claude-haiku-4-5,
    response_text='42'), 1 tool_call (tool_name=lookup, status=ok),
    1 tool_call (tool_name=lookup, status=failed).
"""

from __future__ import annotations

import uuid

import mesedi
from mesedi.integrations.langchain import MesediCallbackHandler


# ─────────────────────────────────────────────────────────────────────
# Mock LangChain-shaped types. Only the attributes the adapter reads
# are populated.
# ─────────────────────────────────────────────────────────────────────


class _MockHumanMessage:
    type = "human"

    def __init__(self, content: str) -> None:
        self.content = content


class _MockSystemMessage:
    type = "system"

    def __init__(self, content: str) -> None:
        self.content = content


class _MockChatGeneration:
    def __init__(self, content: str) -> None:
        self.text = ""
        self.message = _MockAIMessage(content)


class _MockAIMessage:
    def __init__(self, content: str) -> None:
        self.content = content
        self.usage_metadata = {"input_tokens": 12, "output_tokens": 1}


class _MockLLMResult:
    def __init__(self, content: str) -> None:
        self.generations = [[_MockChatGeneration(content)]]
        self.llm_output = {
            "token_usage": {"prompt_tokens": 12, "completion_tokens": 1},
            "model_name": "claude-haiku-4-5",
        }


# ─────────────────────────────────────────────────────────────────────
# Smoke driver
# ─────────────────────────────────────────────────────────────────────


@mesedi.wrap
def run_smoke() -> str:
    """Stand-in for a customer's @wrap'd agent entry point. Drives the
    LangChain callback handler directly to simulate what LangChain's
    CallbackManager would do at runtime."""
    handler = MesediCallbackHandler()

    # ── 1. A chat-model LLM call returning '42'. ──────────────────────
    run_id_1 = uuid.uuid4()
    handler.on_chat_model_start(
        serialized={
            "id": ["langchain", "chat_models", "anthropic", "ChatAnthropic"],
            "kwargs": {"model_name": "claude-haiku-4-5"},
            "name": "ChatAnthropic",
        },
        messages=[[
            _MockSystemMessage("You are a calculator. Reply with only the number."),
            _MockHumanMessage("What is 21 times 2?"),
        ]],
        run_id=run_id_1,
    )
    handler.on_llm_end(_MockLLMResult("42"), run_id=run_id_1)

    # ── 2. A successful tool call. ────────────────────────────────────
    run_id_2 = uuid.uuid4()
    handler.on_tool_start(
        serialized={"name": "lookup", "id": ["my_tools", "lookup"]},
        input_str="customer_id=42",
        run_id=run_id_2,
    )
    handler.on_tool_end(output="ACME Corp", run_id=run_id_2)

    # ── 3. A failed tool call (silent-degradation pattern, Mesedi
    #       classifies as tool_failures). ──────────────────────────────
    run_id_3 = uuid.uuid4()
    handler.on_tool_start(
        serialized={"name": "lookup", "id": ["my_tools", "lookup"]},
        input_str="customer_id=does_not_exist",
        run_id=run_id_3,
    )
    handler.on_tool_error(
        error=ValueError("customer not found"),
        run_id=run_id_3,
    )

    return "ok"


def main() -> None:
    mesedi.configure(
        api_key="mesedi_sk_dev_local_only",
        base_url="http://localhost:8080",
    )
    result = run_smoke()
    print(f"smoke result: {result}")
    mesedi.flush(timeout=5.0)
    print("flushed; inspect http://localhost:8080/ui/")


if __name__ == "__main__":
    main()
