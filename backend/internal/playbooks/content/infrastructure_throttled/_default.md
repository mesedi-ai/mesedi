# Infrastructure throttled

An LLM provider returned HTTP 429, or your application's circuit breaker tripped, or a hard quota was exhausted. This is an operations signal, not an agent-code bug: the agent did the right thing, the upstream said no.

The signature is `infrastructure_throttled:<reason>:<provider>` (and may include a third segment for the dimension or circuit state). The three canonical reasons are `rate_limit`, `circuit_breaker`, and `quota_exhausted`. Common shapes:

- `infrastructure_throttled:rate_limit:<provider>` or `infrastructure_throttled:rate_limit:<provider>:<dimension>` (e.g. `tokens_per_minute`)
- `infrastructure_throttled:circuit_breaker:<provider>:<state>` (state defaults to `open` when unspecified)
- `infrastructure_throttled:quota_exhausted:<provider>`

Mesedi auto-emits these signals from the `instrument_*` modules for Anthropic, OpenAI, Cohere, and Gemini (sync + async + streaming paths). Custom code paths can emit manually via the `emit_infrastructure_event` SDK helper.

## What's usually happening

The reason in the signature usually tells the story:

- **`rate_limit`** means the provider returned an HTTP 429 (with or without a `Retry-After` header). You are above the per-minute, per-day, or per-key limit configured on your provider account. The optional `<dimension>` suffix (e.g. `tokens_per_minute`, `requests_per_minute`) names which axis you crossed.

- **`circuit_breaker`** means your application code's circuit breaker tripped because a downstream dependency had too many consecutive failures. The breaker now refuses calls for a cool-down period to avoid hammering a broken upstream. The downstream was the cause; the breaker is the symptom. The signature carries the breaker state (`open`, `half_open`, etc.) so the "half_open re-test failed" pattern stays distinct from a fresh `open` trip.

- **`quota_exhausted`** means you hit a hard provider limit such as a monthly spend cap, a Pro plan token allotment, or an organization-level budget. The provider will not accept calls until the limit resets or the cap is raised.

## How to investigate

Open the execution and find the `infrastructure_event` payload. The `reason` and `provider` fields name what triggered. For rate limits, the payload usually includes the response headers from the provider, including `Retry-After`, `X-RateLimit-Remaining`, and the relevant limit window.

Look at the failure_group's affected-executions list to see how widespread the throttling is. A single execution suggests a code-path issue (one agent fanned out an unusually large number of calls); a long list across many executions suggests a project-wide quota or rate-limit issue.

## How to fix

The remediation depends on the reason:

- **`rate_limit`.** Add exponential backoff with jitter at the SDK call site. The `instrument_*` modules already capture the provider's `Retry-After` value into the `retry_after_ms` field on the event payload — honor it from your retry layer rather than rolling your own delay. For high-volume workloads, consider splitting traffic across multiple API keys, requesting a rate-limit increase from the provider, or implementing a per-agent rate limiter that smooths spikes.

- **`circuit_breaker`.** Find what the breaker is protecting. Whatever upstream the breaker is in front of is failing. Fix that upstream; the breaker will close itself once it sees healthy calls again. If the breaker is over-aggressive (tripping on transient errors that recover on their own), tune its thresholds.

- **Quota exhausted.** Raise the quota with the provider, or implement an application-side budget that caps spending below the provider quota so you fail fast and gracefully instead of mid-execution. Cross-reference with Mesedi's `cost_velocity` failure groups to identify which executions are driving the spend.

## How to test the fix

After deploying the fix, watch the failure_group's last_seen timestamp. New throttling events should slow or stop. If you implemented backoff, expect to see overall latency tick up slightly (the price of retries); that is normal. If latency increases dramatically, your backoff is too aggressive and is amplifying tail latency more than necessary.

## A note on this being an operations signal

Throttling is not a code bug, and the right people to triage it are operations or platform engineering, not the agent developer. Mesedi flags it distinctly from generic tool failures so triage routing can fork on the failure_class.

## Related detectors

- **`provider_incident:<provider>:rate_limited`** is the sibling cross-tenant signal. (`provider_incident`'s error_class vocabulary uses `rate_limited` — the past-tense form — distinct from infrastructure_throttled's `rate_limit` reason. They're sourced from different events: infra_throttled from infrastructure_event.reason; provider_incident from the canonical error-class mapping of provider exceptions.) If multiple unrelated Mesedi projects all see rate_limited errors against the same provider in the same window, the cause is provider-side and the playbook is "wait for the provider to recover," not "tune your retry logic." `infrastructure_throttled` is YOUR project's per-tenant view; `provider_incident` is the cross-tenant view. Open both pages side-by-side when triaging — if the provider_incident group is active, your local backoff and quota changes will not help until the provider stabilizes.
- **`cost_velocity`** is the financial sibling. Throttling and high spend often correlate (more calls per minute → both more throttled retries AND more dollars per minute). Cross-reference to identify whether the same code path is driving both.
