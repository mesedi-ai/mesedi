# Mesedi Python SDK

**Status:** v0.2.0. Live on PyPI.

The Mesedi SDK observes autonomous AI agent runs and ships them to the Mesedi
backend for failure-class detection and analysis. The v1 surface:

- `mesedi.configure(api_key=...)`: set up the module-level client
- `@mesedi.wrap`: decorate any function as an "agent execution". The SDK
  records start, completion (or crash), wall-clock duration, and a stable
  crash signature suitable for grouping identical exceptions.
- `@mesedi.tool`: decorate any function as an observed tool call. Emits
  `tool_call` events into the surrounding execution context, including
  the function's docstring (see "Tool descriptions" below).
- Framework adapters for LangChain, LangGraph, OpenAI Agents SDK, and CrewAI (see below).

## Install

```bash
pip install mesedi
```

## Quickstart

```python
import mesedi

mesedi.configure(api_key="mesedi_sk_...")

@mesedi.wrap
def run_my_agent(query: str) -> str:
    # ... your agent logic here ...
    return "answer"

run_my_agent("hello")
```

For local backend development against `localhost:8080`, pass an explicit
`base_url=`. Otherwise the SDK posts to the Mesedi production backend.

## What lands in the backend

For each `@wrap`-decorated call:

- **On entry:** `POST /executions` with `execution_id`, `status="started"`,
  `sdk_language="python"`, `sdk_version="0.2.0"`.
- **On normal return:** `PATCH /executions/{id}` with `status="completed"`,
  `ended_at`, `duration_ms`.
- **On exception:** `PATCH /executions/{id}` with `status="crashed"`,
  `crash_signature` (SHA-256-derived stable hash of exception type + top
  of traceback), then the original exception is re-raised.

Network failures during observation NEVER block the wrapped function. The
SDK is fail-open: a Mesedi outage degrades to invisibility, not to broken
production code.

## Tool descriptions

`@mesedi.tool` reads the decorated function's docstring and sends it as
`tool_description` on each `tool_call` event. Nothing to configure:

```python
@mesedi.tool
def lookup_docs(library: str) -> dict:
    """Look up documentation for a library. Returns the doc snippet."""
    return {"library": library, "snippet": "..."}
```

**Why this exists.** A tool's contract has two halves: the shape it
returns, and the description the model reads when deciding whether and
how to call it. Mesedi's `tool_schema_drift` detector watches both.
When a description changes away from a stable baseline you get a
failure group with a signature like `lookup_docs:desc:1a2b3c4d`,
distinct from a return-shape change so you can tell which half moved.

That matters most under MCP, where descriptions come from a
third-party server and are not sanitised. It is the mechanism behind
CVE-2026-75130 (Context7 MCP server, published 2026-08-18): a
compromised server puts instructions in what reads to the model as
help text, the agent follows them, and the tool's return shape never
changes. Without the description, nothing about that call looks
unusual.

Two details worth knowing:

- The docstring is read **at call time**, not when the decorator runs.
  A description swapped at runtime, which is exactly what a
  compromised MCP server does, is therefore visible.
- A tool with no docstring omits the field entirely rather than
  sending an empty string, and description drift never fires for it.
  Nothing else changes.

Descriptions are truncated at 2000 characters with an inline
`...[truncated]` marker, and are only ever hashed for comparison.

## Optional: hard-halt with local budgets

Cap a single execution across four axes — input tokens, output tokens,
wall-clock seconds, and step count. Pass any subset; unset fields impose
no limit on that axis. When any budget is exceeded, the SDK raises
`MesediHalt` at the next safe boundary (between LLM calls, tool calls,
or explicit `checkpoint()`s), never mid-call, so `try`/`finally` cleanup
runs and open resources release.

```python
from mesedi import wrap, Budget

@wrap(budget=Budget(
    max_wall_clock_seconds=600,   # 10 min real time
    max_steps=30,                  # 30 tool/LLM/checkpoint boundaries
    max_tokens_in=200_000,
    max_tokens_out=50_000,
))
def my_agent(query: str):
    ...
```

When a budget is supplied, the SDK also opens an SSE subscription to
`GET /executions/{id}/halt-stream`. Operators can halt a running
execution from the dashboard. If the SSE connection fails (backend
unreachable, 4xx/5xx, network partition), the reader logs and returns
— the wrapped agent keeps running with local budgets still enforced
client-side. Mesedi never decides to halt on its own; operator intent
or your own budget rules are the only triggers. `MesediHalt` inherits
from `BaseException` (not `Exception`), so broad `except Exception`
handlers do not swallow it.

## Framework integrations

If your agent is built on LangChain, LangGraph, the OpenAI Agents SDK, or
CrewAI, you don't have to wrap every function with `@mesedi.tool` by hand.
Adapter modules under `mesedi.integrations.*` translate each framework's
native callback or hook surface into Mesedi telemetry. They're **optional**:
importing `mesedi` itself never requires any framework to be installed.

The pattern is the same across frameworks: your function gets `@mesedi.wrap`
for the execution boundary, and a one-line adapter does the in-execution
event emission.

### LangChain

```bash
pip install mesedi[langchain]
```

```python
import mesedi
from mesedi.integrations.langchain import MesediCallbackHandler

@mesedi.wrap
def run_agent(question: str) -> str:
    chain = build_chain()
    result = chain.invoke(
        {"input": question},
        config={"callbacks": [MesediCallbackHandler()]},
    )
    return result["output"]
```

The callback handler subscribes to LangChain's standard `on_llm_start` /
`on_llm_end` / `on_tool_start` / `on_tool_end` (etc.) hooks and emits
`llm_call` and `tool_call` events with the same wire format as a
hand-written `mesedi.emit_llm_call()` + `@mesedi.tool` pair. Detectors
(drift, identical/similar-call loops, tool-failures, cost-velocity,
prompt-injection) see no difference.

### LangGraph

```bash
pip install mesedi[langgraph]
```

```python
import mesedi
from mesedi.integrations.langgraph import instrument_graph

@mesedi.wrap
def run_my_graph(question: str) -> str:
    graph = build_graph()
    instrument_graph(graph)
    result = graph.invoke({"input": question})
    return result["output"]
```

`instrument_graph` attaches Mesedi telemetry to each node in the graph,
emits `llm_call` and `tool_call` events for the LLM-backed nodes, and
labels each event with the node name so the dashboard timeline shows the
graph's flow alongside the per-step detail.

### OpenAI Agents SDK

```bash
pip install mesedi[openai-agents]
```

```python
import mesedi
from mesedi.integrations.openai_agents import instrument_agent

@mesedi.wrap
def run_my_agent(question: str) -> str:
    agent = build_agent()
    instrument_agent(agent)
    return agent.run(question)
```

`instrument_agent` subscribes to the OpenAI Agents SDK's lifecycle hooks
and emits `llm_call` + `tool_call` events with the same wire format as
the LangChain and LangGraph adapters, so detectors see no difference.

### CrewAI

```bash
pip install mesedi[crewai]
```

```python
import mesedi
from mesedi.integrations.crewai import instrument_crew

@mesedi.wrap
def run_my_crew(question: str) -> str:
    crew = build_crew()
    instrument_crew(crew)
    return str(crew.kickoff(inputs={"question": question}))
```

`instrument_crew` is one line that does three things, all idempotent:

1. Attaches a Mesedi `MesediCallbackHandler` to each agent's LLM. Same
   LLM/tool telemetry as the LangChain integration above, because CrewAI
   uses LangChain under the hood.
2. Sets `crew.step_callback` to emit `crewai.agent_action` /
   `crewai.agent_finish` checkpoint events per agent step.
3. Sets `crew.task_callback` to emit `crewai.task_completed` checkpoint
   events per finished task.

Result: the dashboard timeline shows LLM/tool detail interleaved with
CrewAI's higher-level reasoning rhythm.

## Releases

This SDK is published to PyPI via OIDC Trusted Publishing from the
`release-sdk-python.yml` GitHub Actions workflow, with no long-lived
PYPI_TOKEN secret. Every release carries the PyPI "verified" provenance
badge linking it to a specific commit in `mesedi-ai/mesedi`.

To cut a new release, bump `version` in `pyproject.toml`, commit, then:

```bash
git tag -a sdk-python-v0.X.Y -m "Release sdk-python v0.X.Y"
git push origin sdk-python-v0.X.Y
```

The workflow type-checks, builds, validates with `twine`, and publishes.
