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

## Posture

This SDK ships from the same monorepo as the backend during the local-only
development window. PyPI publication via Trusted Publishing (PEP 740) is
deferred to the post-LOI sequence in `../docs/DEVELOPMENT_CHECKLIST.md`.
