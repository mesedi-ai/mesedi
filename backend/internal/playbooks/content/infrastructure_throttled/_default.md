# Infrastructure throttled

An LLM provider returned HTTP 429, or your application's circuit breaker tripped, or a hard quota was exhausted. This is an operations signal, not an agent-code bug: the agent did the right thing, the upstream said no.

The signature is `infrastructure_throttled:<reason>` where reason is one of `rate_limited`, `circuit_breaker_open`, or `quota_exhausted`.

## What's usually happening

The reason in the signature usually tells the story:

- **`rate_limited`** means the provider returned HTTP 429 (Anthropic) or 429 with a Retry-After header (OpenAI). You are above the per-minute, per-day, or per-key request limit configured on your provider account.

- **`circuit_breaker_open`** means your application code's circuit breaker tripped because a downstream dependency had too many consecutive failures. The breaker now refuses calls for a cool-down period to avoid hammering a broken upstream. The downstream was the cause; the breaker is the symptom.

- **`quota_exhausted`** means you hit a hard provider limit such as a monthly spend cap, a Pro plan token allotment, or an organization-level budget. The provider will not accept calls until the limit resets or the cap is raised.

## How to investigate

Open the execution and find the `infrastructure_event` payload. The `reason` and `provider` fields name what triggered. For rate limits, the payload usually includes the response headers from the provider, including `Retry-After`, `X-RateLimit-Remaining`, and the relevant limit window.

Look at the failure_group's affected-executions list to see how widespread the throttling is. A single execution suggests a code-path issue (one agent fanned out an unusually large number of calls); a long list across many executions suggests a project-wide quota or rate-limit issue.

## How to fix

The remediation depends on the reason:

- **Rate limited.** Add exponential backoff with jitter at the SDK call site. If you are using the official Anthropic or OpenAI Python SDK, both have built-in retry logic but it may need to be configured. For high-volume workloads, consider splitting traffic across multiple API keys, requesting a rate-limit increase from the provider, or implementing a per-agent rate limiter that smooths spikes.

- **Circuit breaker open.** Find what the breaker is protecting. Whatever upstream the breaker is in front of is failing. Fix that upstream; the breaker will close itself once it sees healthy calls again. If the breaker is over-aggressive (tripping on transient errors that recover on their own), tune its thresholds.

- **Quota exhausted.** Raise the quota with the provider, or implement an application-side budget that caps spending below the provider quota so you fail fast and gracefully instead of mid-execution. Cross-reference with Mesedi's `cost_velocity` failure groups to identify which executions are driving the spend.

## How to test the fix

After deploying the fix, watch the failure_group's last_seen timestamp. New throttling events should slow or stop. If you implemented backoff, expect to see overall latency tick up slightly (the price of retries); that is normal. If latency increases dramatically, your backoff is too aggressive and is amplifying tail latency more than necessary.

## A note on this being an operations signal

Throttling is not a code bug, and the right people to triage it are operations or platform engineering, not the agent developer. Mesedi flags it distinctly from generic tool failures so triage routing can fork on the failure_class.
