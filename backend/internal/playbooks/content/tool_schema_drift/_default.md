# Tool schema drift

A tool that this project has called many times returned a result whose shape differs from the historical baseline. Mesedi fingerprints each successful `tool_call` return value (sorted keys plus value types only, never the values themselves) and tracks the majority fingerprint per tool over the most recent baseline window. When a return shape diverges, this detector fires.

The signature is `tool_schema_drift:<tool_name>:<shape_hex8>` so each unique (tool, new shape) pair clusters separately. A tool that drifts to shape A today and shape B tomorrow produces two distinct failure_groups.

## How the fingerprint is computed

The shape walk preserves structure and drops values:

- **Primitives** become their type name: `null`, `bool`, `string`, `number`. Integers and floats both render as `number`: the shape doesn't distinguish numeric subtypes.
- **Arrays** render as `[<element-shape>]` using ONLY the first element's shape. Heterogeneous arrays still hash deterministically, but a tool returning `[int, string]` then `[string, int]` would NOT cluster together because the first element differs.
- **Objects** sort keys alphabetically and emit `{key:shape, key:shape, ...}`. ALL keys participate: there is no optional-field tolerance. A field that appears in some responses but not others produces a different shape.
- **Typed sentinels** (`{"__type__": "datetime", "value": "..."}` style): when an object carries a string `__type__` marker, the shape emits `<typed:<typename>>` (e.g. `<typed:datetime>`) or `<typed:<typename>:<class>>` (e.g. `<typed:object:User>`) instead of treating the sentinel as a plain object. This honors the SDK's convention for non-JSON-native values (datetimes, custom objects) shipped via `{"__type__": ..., "value": ...}`.

## When the detector fires

Three conditions all need to hold:

1. **Enough history.** The project must have at least `min_history_calls` (default 10) prior successful tool_call events for this tool. Brand-new tools sit in a priming state and don't fire.
2. **Stable baseline.** A single shape must dominate the history, at least 2/3 of the historical calls must share the same fingerprint. Tools whose return shape legitimately varies (e.g. a generic web fetcher) never establish a stable baseline and never fire drift.
3. **Current shape differs.** The execution's current return shape must differ from the dominant baseline.

## What's usually happening

The single most common cause is a third-party API version change that the provider rolled out without coordinating with you. The provider added a field, renamed a field, changed a value type from string to object, or restructured the response. Your agent is now parsing a response shape it has not been calibrated for and is probably misreading it. The agent's output may look plausible but be subtly wrong because the parser is now interpreting the wrong field.

Other less-common causes:

- The tool wrapper itself changed (you updated the wrapper code and the return shape changed as a side effect).
- The provider returns different shapes for different request paths and your agent took a path it had not previously hit.
- A feature flag or A/B test on the provider's side flipped between request and response.

## How to investigate

Open the affected execution. On the `tool_call` event, the return_value shape will be the new shape. Cross-reference with prior `tool_call` events in this project for the same tool name to see the historical shape. The diff will tell you exactly what changed.

Three places to verify the cause:

1. **The provider's changelog or release notes.** Many provider changes are announced; check the version in the response headers if available.
2. **The tool's request payload.** If your agent is sending a different request than it used to (different parameters, different endpoint), the response shape may be different by design.
3. **Local replay.** Re-run the same tool call from a clean environment. If the new shape reproduces, the provider changed; if not, look at the request path.

## How to fix

The remediation depends on the cause:

- **Provider API version bump.** Pin the API version at the SDK boundary if the provider supports it. Many provider SDKs accept a version header (`anthropic-version`, `OpenAI-Version`, etc.) or a path-based version prefix. Pin to the version your agent was calibrated on, then update both the parser and the pin together when you intentionally upgrade.

- **Tool wrapper change.** Revert the wrapper change if the schema change was unintended. If it was intentional, update the agent's parser to handle both shapes during the transition window.

- **Different request path.** Make the request path explicit in your agent code so you can reason about which shape to expect. If both paths are valid, write the parser to handle both shapes.

## How to test the fix

After deploying the fix, the `tool_schema_drift` failure_group should stop accumulating new affected executions. If it continues, the parser is still seeing both shapes; widen the parser's coverage. If a new variant of the drift surfaces (the provider changed again), Mesedi will create a new failure_group with a new shape hash.

## Per-project tunables

Two configurable knobs:

- **`min_history_calls`** (default 10; bounds [2, 1000]; no tier cap). Lower it to fire on tools with little history; raise it to require a longer baseline before firing. Detector-thresholds primitive.
- **`tool_return_value_max_bytes`** (default 8192; tier-capped). A handler-layer cap, NOT a detector knob: it's applied at event ingest before the detector sees the data. Returns above this threshold are excluded from fingerprinting (treated as inconclusive, mirroring the SDK's `<truncated>` sentinel). Raise it if your tools legitimately return large payloads whose tail bytes are signal-bearing; lower it to amortize storage costs if your tools return mostly-irrelevant tail bytes. The Settings dashboard surfaces a truncation-rate telemetry tile so you can see whether the cap is firing.

The 2/3 majority threshold for baseline stability is hardcoded and not per-project tunable.

## A note on the detection threshold

`min_history_calls` defends against false positives on tools that are new to the project or rarely called. The 2/3 majority means a single odd-shaped call does not trip the detector; it requires the new shape to become dominant or the OLD shape to lose dominance before firing.
