# Tool schema drift

A tool that this project has called many times returned a result whose shape differs from the historical baseline. Mesedi fingerprints each successful `tool_call` return value (sorted keys plus value types only, never the values themselves) and tracks the majority fingerprint per tool over the most recent 10 or more calls. When a return shape diverges, this detector fires.

The signature is `tool_schema_drift:<tool_name>:<shape_hex8>` so each unique (tool, new shape) pair clusters separately.

## What's usually happening

The single most common cause is a third-party API version change that the provider rolled out without coordinating with you. The provider added a field, renamed a field, changed a value type from string to object, or restructured the response. Your agent is now parsing a response shape it has not been calibrated for and is probably misreading it. The agent's output may look plausible but be subtly wrong because the parser is now interpreting the wrong field.

Other less-common causes:

- The tool wrapper itself changed (you updated the wrapper code and the return shape changed as a side effect)
- The provider returns different shapes for different request paths and your agent took a path it had not previously hit
- A feature flag or A/B test on the provider's side flipped between request and response

## How to investigate

Open the affected execution. On the tool_call event, the return_value shape will be the new shape. Cross-reference with prior `tool_call` events in this project for the same tool name to see the historical shape. The diff will tell you exactly what changed.

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

## A note on the detection threshold

The detector requires at least 10 prior successful calls to the same tool to establish a baseline before it fires. This avoids false positives on tools that are new to the project or rarely called. The 2/3 majority threshold means a single odd-shaped call does not trip the detector; it requires the new shape to become dominant before firing. Both thresholds can be tuned per-project in a future release.
