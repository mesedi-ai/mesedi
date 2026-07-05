"""End-to-end smoke test for the CrewAI adapter.

Doesn't require ``crewai`` to be installed, uses duck-typed mocks
that mirror the Crew / Agent / LLM / AgentAction / AgentFinish /
TaskOutput shapes the real CrewAI exposes.

Verifies that ``instrument_crew``:
  1. Attaches a MesediCallbackHandler to each agent's LLM callbacks
  2. Sets crew.step_callback and crew.task_callback (since the mock
     crew doesn't have them pre-set)
  3. The step_callback emits the right Mesedi events (one
     checkpoint per agent step + one tool_call when an underlying
     llm_call would fire, we simulate the underlying LangChain
     handler dispatch directly)

Run after starting the backend on :8080:

    python3 sdk-python/sandbox/crewai_adapter_test.py

Verify on http://localhost:8080/ui/:
  - Execution 'run_crewai_smoke' is completed.
  - Timeline shows checkpoint events tagged 'crewai.agent_action',
    'crewai.agent_finish', 'crewai.task_completed' plus an llm_call
    and tool_call from the simulated LangChain dispatch.
"""

from __future__ import annotations

import uuid
from dataclasses import dataclass
from typing import Any, List, Optional

import mesedi
from mesedi.integrations.crewai import (
    instrument_crew,
    mesedi_step_callback,
    mesedi_task_callback,
)


# ─────────────────────────────────────────────────────────────────────
# Mock CrewAI-shaped types. Only the attributes the adapter reads
# are populated. Real CrewAI classes have many more fields.
# ─────────────────────────────────────────────────────────────────────


class _MockLLM:
    """Mimics a LangChain ChatModel, has a mutable ``callbacks`` list."""

    def __init__(self) -> None:
        self.callbacks: Optional[List[Any]] = None


@dataclass
class _MockAgent:
    role: str
    llm: _MockLLM


@dataclass
class _MockCrew:
    agents: List[_MockAgent]
    step_callback: Any = None
    task_callback: Any = None


@dataclass
class _MockAgentAction:
    tool: str
    tool_input: str
    log: str = ""


@dataclass
class _MockAgentFinish:
    return_values: dict
    log: str = ""


@dataclass
class _MockTaskOutput:
    description: str
    agent: str
    raw: str


@dataclass
class _MockChatGeneration:
    text: str = ""
    message: Any = None


class _MockAIMessage:
    def __init__(self, content: str) -> None:
        self.content = content
        self.usage_metadata = {"input_tokens": 50, "output_tokens": 8}


class _MockLLMResult:
    def __init__(self, content: str) -> None:
        self.generations = [[_MockChatGeneration(text="", message=_MockAIMessage(content))]]
        self.llm_output = {"model_name": "gpt-4o"}


# ─────────────────────────────────────────────────────────────────────
# Smoke driver
# ─────────────────────────────────────────────────────────────────────


@mesedi.wrap
def run_crewai_smoke() -> str:
    """Stand-in for a customer's @wrap'd CrewAI entry point. Builds a
    fake crew, instruments it, then simulates the dispatch that
    CrewAI + LangChain would do at runtime so we can verify the
    adapter end-to-end."""

    # Build a mock crew with one agent.
    researcher = _MockAgent(role="researcher", llm=_MockLLM())
    crew = _MockCrew(agents=[researcher])

    # Instrument it. After this:
    #   - researcher.llm.callbacks should contain a MesediCallbackHandler
    #   - crew.step_callback should be mesedi_step_callback
    #   - crew.task_callback should be mesedi_task_callback
    handler = instrument_crew(crew)
    assert handler in (researcher.llm.callbacks or []), (
        "handler should have been attached to researcher.llm.callbacks"
    )
    assert crew.step_callback is mesedi_step_callback, (
        "step_callback should have been set to mesedi_step_callback"
    )
    assert crew.task_callback is mesedi_task_callback, (
        "task_callback should have been set to mesedi_task_callback"
    )

    # ── Simulate the dispatch CrewAI + LangChain would do at runtime ──

    # 1. The agent's LLM fires (CrewAI calls underlying LangChain
    #    chat model; LangChain's CallbackManager would dispatch the
    #    Mesedi handler we just attached).
    run_id_llm = uuid.uuid4()
    handler.on_chat_model_start(
        serialized={
            "id": ["langchain", "chat_models", "openai", "ChatOpenAI"],
            "kwargs": {"model_name": "gpt-4o"},
            "name": "ChatOpenAI",
        },
        messages=[[]],
        run_id=run_id_llm,
    )
    handler.on_llm_end(_MockLLMResult("I should search for recent papers."), run_id=run_id_llm)

    # 2. CrewAI dispatches step_callback with an AgentAction (agent
    #    decided to invoke a tool).
    crew.step_callback(_MockAgentAction(
        tool="search_web",
        tool_input="recent papers on agent observability",
    ))

    # 3. The tool runs. LangChain's CallbackManager would emit
    #    on_tool_start / on_tool_end on the handler.
    run_id_tool = uuid.uuid4()
    handler.on_tool_start(
        serialized={"name": "search_web"},
        input_str="recent papers on agent observability",
        run_id=run_id_tool,
    )
    handler.on_tool_end(
        output="Found 3 relevant papers: ...",
        run_id=run_id_tool,
    )

    # 4. Agent finishes the step (final answer).
    crew.step_callback(_MockAgentFinish(
        return_values={"output": "Here are 3 papers..."},
    ))

    # 5. CrewAI dispatches task_callback when the task completes.
    crew.task_callback(_MockTaskOutput(
        description="Find recent papers on agent observability",
        agent="researcher",
        raw="Here are 3 papers...",
    ))

    return "ok"


def main() -> None:
    mesedi.configure(
        api_key="mesedi_sk_dev_local_only",
        base_url="http://localhost:8080",
    )
    result = run_crewai_smoke()
    print(f"smoke result: {result}")
    mesedi.flush(timeout=5.0)
    print("flushed; inspect http://localhost:8080/ui/")


if __name__ == "__main__":
    main()
