# Provider incident

Multiple distinct tenants in this project hit the same LLM provider error (Anthropic, OpenAI, Gemini, Bedrock, Mistral) within the last 15 minutes. This is a cross-tenant signal that the provider is almost certainly having an incident rather than per-tenant code being independently broken. The detector defaults to firing when at least two distinct tenants are affected.

The signature is `provider_incident:<provider>:<error_class>` (e.g. `provider_incident:anthropic:service_unavailable`, `provider_incident:openai:rate_limited`) so each unique (provider, error_class) pair clusters separately. An Anthropic 529 incident and an OpenAI rate-limit incident at the same time would produce two distinct groups.

## What's usually happening

The error_class segment is drawn from Mesedi's canonical provider-error vocabulary (sourced from `spec/error_classes.yaml`). Only the **provider-side** subset can trigger this detector — customer-side classes (bad API keys, malformed requests) are explicitly excluded because they signal customer code defects, not provider outages.

Provider-side error classes:

- **`service_unavailable`** is the canonical class for HTTP 500 / 502 / 503 / 529 — the provider's API is returning infrastructure-trouble status codes. Check their status page first.

- **`internal_error`** is the provider's "something went wrong on our side" response (typically a generic 500 with no further detail). Same investigation as `service_unavailable`.

- **`timeout`** means the provider's response exceeded the SDK's timeout window. Could be provider-side latency spikes or network-path issues. If only one provider is affected, lean provider-side; if multiple providers timeout simultaneously, look at your egress network.

- **`rate_limited`** is broadly the same as `infrastructure_throttled:rate_limit:*` but cross-tenant: many tenants are hitting the same provider rate limit, which can mean an org-level quota was lowered or your traffic distribution shifted.

- **`quota_exhausted`** is the same as `infrastructure_throttled:quota_exhausted:*` but cross-tenant: many tenants tripping the same hard cap simultaneously, e.g. a shared monthly-spend ceiling across the project.

Customer-side error classes (`invalid_api_key`, `client_error`, `unknown`) are filtered out of provider_incident clustering by the `IsProviderSideErrorClass` guard. Cross-tenant credential failures are usually a project-wide rotation event, not a provider outage, and surface in different places.

## How to investigate

Check the provider's public status page first. Anthropic, OpenAI, and Google all post incidents within minutes of detection most of the time. If they confirm an incident, your action is to wait, communicate to your users, and let your retry logic ride out the window.

If the provider's status page says "all systems operational" and Mesedi's detector still fired, the cross-tenant pattern suggests either:

1. A regional issue not yet reflected on the global status page
2. An issue specific to your model selection (a newer or deprecated variant)
3. A network or auth issue on your side that looks like a provider problem

## How to fix

The remediation depends on root cause:

- **Confirmed provider incident.** Wait for the provider to recover. Communicate degradation to your users via a banner or status page. If your application has fallback providers (Anthropic falling back to OpenAI, or vice versa), trigger the failover.

- **Regional incident not on the status page.** File a support ticket with the provider including the failing request IDs from your logs. They will often correlate against their internal telemetry and either confirm the regional issue or rule out a provider-side cause within a few hours.

- **Network or auth issue mislabeled as provider incident.** Rotate the affected credentials, verify the network egress path, and check whether a recent infrastructure change affected the provider's TLS or DNS resolution.

## How to test the fix

After the provider recovers or you cut over to a fallback, the provider_incident failure_group should stop accumulating new affected_executions. New executions from the same project should land cleanly. If the group continues to grow after the provider has confirmed recovery, the residual signal is on your side and needs separate investigation.

## A note on cross-tenant detection

This detector is specifically a cross-tenant signal. It looks at the `tenant_id` field on executions and counts distinct tenants with the same provider error in the recent window. If you do not populate `tenant_id` on your executions (single-tenant project, or you have not wired the field), **this detector silently does not fire** — there is no error, no warning, just no failure_group. Cross-reference with the cost-by-tenant report to see whether you are passing the tenant_id correctly; if that report shows everything under a single empty-string tenant bucket, provider_incident is non-functional for your project until you start populating tenant_id.
