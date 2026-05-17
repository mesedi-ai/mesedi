# Drift — new model

An execution in this project used **a model name that hasn't appeared in the project's recent history**. Mesedi's drift detector compares the set of models used in the current execution against the set used in prior executions over the historical window and flags any that are new. The signature on this failure group encodes the alphabetically-first new model — e.g. `new_model:claude-opus-4-6`.

This is categorical drift — the model identifier literally changed. It's the cheapest useful drift signal and v0.0.1 ships only this version. Future Mesedi versions catch distributional drift (continuous prompt evolution) via the lexical-drift detector, which is the sibling failure-group class on this dashboard.

## Why this matters

Model changes are one of the highest-leverage upstream causes of agent behavior change. A team swaps `claude-3-opus` for `claude-opus-4-6` to take advantage of the new release. The intent is "same behavior, better quality." The reality is often "same overall benchmark score, materially different behavior on the long tail of agent inputs your team wasn't testing." Latency profiles change. Cost per execution changes. Output format quirks change. Tool-use behavior changes. Safety-refusal patterns change.

Without an explicit detector for "the model field changed," this kind of change shows up as a generic regression weeks later when someone notices a downstream metric drift. The new-model signature catches it on the first execution and gives you the diff before the regression spreads.

Three concrete failure modes this detector catches:

1. **Intentional but uncoordinated model swap.** A developer upgraded the model in the agent code without flagging it to the observability or product team. The change is real, the intent is fine, but the surrounding team doesn't know to look for behavior shifts.

2. **A/B test leak.** A model variant that was supposed to be confined to a test cohort is now appearing in production traffic. The routing logic has a bug that's leaking the variant to a broader population.

3. **Misconfiguration.** A typo, an env-var mistake, a stale config file, a feature flag default — something routed traffic to the wrong model and nobody noticed because the wrong model is still producing plausible output.

## What this does NOT mean

If your project legitimately uses many models — a router that picks a model per task type, a customer-facing product where users choose a model, a multi-tenant deployment — the new-model signal will fire whenever a model is used for the first time in the historical window. After enough executions, the signal stabilizes (no model is "new" once it's been seen) and the failure-group ages out. Until then, expect noise.

The detector also doesn't distinguish "new to the project" from "new to anyone" — `claude-opus-4-6` is a real well-tested model; the signal is that YOUR PROJECT hasn't used it before, not that it's experimental.

## How to investigate

Open one of the affected executions. Three diagnostics:

1. **Find the new model in the execution's `llm_call` events.** The execution detail view shows distinct models. The one that doesn't appear in your project's prior executions is what triggered the signature. Note: if more than one new model appears, the signature names the alphabetically-first; the others are also flagged on the same group.

2. **Trace the routing.** Look at where the model is selected in your code. Is it hardcoded, env-var-driven, feature-flag-driven, request-parameter-driven? Each routing source has a different audit trail. A hardcoded model means a code change is what introduced the new value; an env var means a deploy config change; a feature flag means a flag toggle.

3. **Check what changed in the last deploy.** If the affected-executions table shows the signature starts at a recent timestamp, cross-reference with your deploy log. Most new-model events trace back to a deploy in the prior few hours.

## How to fix

If the new model is intentional, the fix is not in code but in process — make the change visible:

- **Announce model changes before deploying.** A brief note in your team chat ("upgrading agent X from haiku to sonnet, watch for cost and latency changes for the next 24h") turns a Mesedi alert from "what just happened" into "we knew this was coming."

- **Pin model versions explicitly in code.** `claude-opus-4-6` is better than `claude-opus-4` because the latter could silently bump to a new version when the provider rolls forward. Explicit pins make model swaps require code changes, which makes them auditable.

- **Add the model to your project's expected-model allowlist.** If you maintain a list of approved models, adding the new one acknowledges the change. The failure-group on Mesedi can then be dismissed as known.

If the new model is unintentional, the fix is structural:

- **Add startup-time validation.** When your agent boots, check that the configured model is in an allowlist. Crash on boot if it isn't, with a clear error message naming the bad value. This catches misconfiguration at deploy time, not at request time.

- **Test routing logic on every change.** If your model selection lives in routing logic (router by task type, A/B variant chooser, feature-flag dispatcher), add a unit test that asserts the routing for the cases you care about. Routing logic bugs are usually one-line typos and are easy to catch with focused tests.

## Auto-fix in a future Mesedi release

The v2 roadmap includes a per-project expected-models allowlist managed via the dashboard — Mesedi only fires the new-model signal for models outside the allowlist, dramatically reducing false-positive volume on multi-model projects. The detector itself stays unchanged; the failure-group classification gets a "matches allowlist / does not match allowlist" axis.
