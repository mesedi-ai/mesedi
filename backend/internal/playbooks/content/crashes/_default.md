# Crash

A `@wrap`-decorated execution terminated abnormally: an unhandled exception, an out-of-memory kill, a segfault, or a similar process-level failure. Mesedi's crash detector groups affected executions by **crash signature**, a stable SHA-256 hash derived from the exception type plus the top of the traceback, so semantically identical crashes cluster together even when the surrounding payloads differ.

This is the loudest of Mesedi's failure classes and usually the easiest to act on. The execution did not produce output; the agent did not silently degrade; the SDK captured the failure at the `@wrap` boundary and re-raised the original exception to the caller. The Mesedi cluster you are looking at is the count of times the same crash signature has fired across runs in this period.

## Why this matters more than it looks like it does

Crashes are obvious in unit tests and on a developer's laptop. They are easy to miss in production agents because the surrounding application often catches and logs them at a generic layer (an HTTP server's 500 handler, a worker queue's job-failed branch, a notebook's traceback cell) and life moves on. The crash is recorded as a generic infrastructure error rather than as agent-specific signal, so:

1. **The same bug recurs without anyone noticing.** A null-dereference in one code path keeps firing for thousands of runs because the application-level handler treats every 500 the same.

2. **The crash is the LEADING indicator for a broader pattern.** A spike in crashes is usually downstream of something that already broke: a deployment, a tool's auth credential rolling over, an upstream dependency rate-limiting you, a model swap. The crash count is a faster signal than checking each of those in isolation.

3. **The recovery path matters as much as the failure.** If your agent retries a crashed execution automatically, the retry burns budget and may itself crash; you are paying compute for a known-failing code path. The affected-executions count on this failure group is the size of the bill so far.

## How to find the bug

Open one of the affected executions in the timeline. Three places to look:

1. **The crash signature itself.** Mesedi exposes the captured `crash_signature` (exception type + top of stack frames) on the execution detail. Compare the signature across two or three affected executions. If they match exactly, the bug is structural and reproducible. If they differ but cluster into the same Mesedi group, the bug is data-dependent and triggered by something in the input.

2. **The last event before the crash.** Scroll to the bottom of the timeline. The event immediately preceding the crash usually tells you which step failed: an `llm_call` whose output the agent tried to parse as JSON and threw; a `tool_call` that returned a shape the agent's code did not handle; a `checkpoint` that recorded a state too large for downstream code.

3. **Inputs across affected executions.** Click into 2-3 affected executions. If the same input prefix appears, the bug is triggered by a specific request shape; that gives you a minimal repro you can drop into a test. If the inputs vary widely, the bug is environmental (a flaky dependency, a memory leak that surfaces under sustained load, a timing condition).

## How to fix

The remediation pattern depends on what kind of crash this is:

- **Unhandled exceptions on parsed model output.** The most common shape: the LLM returned text that the agent's code tried to `json.loads()` or `parseInt()` and the exception escaped the surrounding `try`. Wrap the parse in a typed validator (Pydantic, Zod, JSON Schema), feed the validation failure back into the prompt as a corrective signal, and re-emit a fresh `llm_call`. Do not retry the parse on the same output.

- **Crashes from missing or stale credentials.** A `401`, `403`, or `permission denied` that the agent does not catch will end the execution. Add a startup health-check that calls the relevant tool with a known-safe argument and refuses to start if it fails; that catches the credential bug at boot time instead of at request time. Rotate the credential, then redeploy.

- **Out-of-memory.** Usually means the agent accumulated context (chat history, retrieved chunks, intermediate results) without bounding it. Cap the size of every list-typed field that grows step-over-step, trim chat history with a windowed strategy, and add a `max_tokens_in` budget so Mesedi's hard-halt catches the runaway before the OOM does.

- **Unbounded recursion or stack overflow.** Almost always a tool-loop bug: the agent invokes a tool whose handler invokes the same agent. Add a depth counter to the recursive path, cap it at a small constant (5 to 10), and surface the depth limit as a structured error so the agent can pick a different strategy.

- **Crashes from a dependency you do not control.** A spike of crashes correlated with one provider, region, or version is an external incident, not a code bug. Check the `provider_incident` failure group for the same time window. The fix is operational: switch providers if you have a fallback, back off and retry with longer waits if you do not, and surface the degradation to the caller honestly rather than silently retrying.

If the crash signature is new this period, look at what shipped recently: dependency upgrades, model swaps, prompt changes, schema migrations. The first crash of a fresh signature is almost always the diff between the last green deploy and the current one.
