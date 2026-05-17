# Mesedi Python SDK

**Status:** Phase 2 alpha (v0.0.1). Local-only development; not yet on PyPI.

The Mesedi SDK observes autonomous AI agent runs and ships them to the Mesedi
backend for detection and analysis. Today's surface is intentionally tiny:

- `mesedi.configure(api_key=...)` — set up the module-level client
- `@mesedi.wrap` — decorate any function as an "agent execution"; the SDK
  records start, completion (or crash), wall-clock duration, and a stable
  crash signature suitable for grouping identical exceptions.

The `@tool` decorator, Anthropic SDK monkey-patch, async event buffer, and
PyPI release land in later sub-slices.

## Quickstart (local development)

Prerequisites: Mesedi backend running on `localhost:8080` with the bootstrap
dev project. See `../backend/README.md` if not yet running.

```bash
cd ~/mesedi/sdk-python
python3 -m pip install -e .

cd sandbox
python3 real_agent.py
```

Or use it programmatically:

```python
import mesedi

mesedi.configure(
    api_key="mesedi_sk_dev_local_only",
    base_url="http://localhost:8080",
)

@mesedi.wrap
def run_my_agent(query: str) -> str:
    # ... your agent logic here ...
    return "answer"

run_my_agent("hello")
```

## What lands in the backend

For each `@wrap`-decorated call:

- **On entry:** `POST /executions` with `execution_id`, `status="started"`,
  `sdk_language="python"`, `sdk_version="0.0.1"`.
- **On normal return:** `PATCH /executions/{id}` with `status="completed"`,
  `ended_at`, `duration_ms`.
- **On exception:** `PATCH /executions/{id}` with `status="crashed"`,
  `crash_signature` (SHA-256-derived stable hash of exception type + top
  of traceback), then the original exception is re-raised.

Network failures during observation NEVER block the wrapped function. The
SDK is fail-open: a Mesedi outage degrades to invisibility, not to broken
production code.

## Framework integrations

If your agent is built on LangChain or CrewAI, you don't have to wrap every
function with `@mesedi.tool` by hand. Adapter modules under
`mesedi.integrations.*` translate each framework's native callback or hook
surface into Mesedi telemetry. They're **optional**: importing `mesedi`
itself never requires any framework to be installed.

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
hand-written `mesedi.emit_llm_call()` + `@mesedi.tool` pair. Detectors —
drift, identical / similar-call loops, tool-failures, cost-velocity,
prompt-injection — see no difference.

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

1. Attaches a Mesedi `MesediCallbackHandler` to each agent's LLM — same
   LLM/tool telemetry as the LangChain integration above, because CrewAI
   uses LangChain under the hood.
2. Sets `crew.step_callback` to emit `crewai.agent_action` /
   `crewai.agent_finish` checkpoint events per agent step.
3. Sets `crew.task_callback` to emit `crewai.task_completed` checkpoint
   events per finished task.

Result: the dashboard timeline shows LLM/tool detail interleaved with
CrewAI's higher-level reasoning rhythm.

## Posture

This SDK ships from the same monorepo as the backend during the local-only
development window. PyPI publication via Trusted Publishing (PEP 740) is
deferred to the post-LOI sequence in `../docs/DEVELOPMENT_CHECKLIST.md`.
