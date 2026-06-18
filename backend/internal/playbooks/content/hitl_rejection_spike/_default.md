# HITL rejection spike

In the last hour, an unusually high fraction of human-in-the-loop runs in this project came back as either `rejected` (humans saying NO) or `edited` (humans modifying the output before approving). This is the strongest signal Mesedi produces that your agent's behavior regressed, because the humans who interact with the agent are the canary.

The signature is `hitl_rejection_spike:rejected` for the rejection variant and `hitl_rejection_spike:edited` for the edit variant. Both fire when, in the recent 1-hour window:

- At least 5 distinct executions had at least one `human_intervention` event, AND
- For the rejected variant: at least 40 percent of those executions had at least one `rejected` response, OR
- For the edited variant: at least 30 percent had at least one `edited` response

The minimums prevent the detector from firing on noise at the start of a new project; the percentage thresholds are calibrated to be actionable without false-alarming on a couple of bad runs.

## What's usually happening

The two variants signal different things:

- **`rejected`** means humans are saying NO outright. The agent's output was so wrong, unsafe, or unwanted that the human declined to approve any version of it. Almost always indicates a real regression in agent behavior: a recent prompt change, a model upgrade, a tool wrapper change, or a data pipeline shift that broke the agent's grounding.

- **`edited`** means humans are MODIFYING the output before approving. The agent's output is close to correct but persistently off in a specific way. Signals quality drift rather than outright failure: the agent's tone is wrong, its facts are slightly off, its formatting is broken. Less urgent than rejection but a leading indicator that quality is degrading.

## How to investigate

Open the affected executions (visible in the failure_group's affected_executions list). Read the `human_intervention` event payloads on each: the `response_payload` field is where humans usually document their reasoning, and the diff between the agent's original output and the human's edit (when captured) usually points at the regression.

Three diagnostic questions:

1. **What changed recently in your stack?** Cross-reference the timing of the spike with deploys, prompt-template changes, model upgrades, and RAG-pipeline changes. The most common cause is something deployed in the last 24-48 hours.

2. **Is the pattern consistent across executions?** If every rejection is for the same kind of error (formatting, factuality, tone), the regression is narrow and easy to fix. If the rejections are scattered across many kinds of error, the regression is broader (a model swap, a tokenizer change).

3. **Are the rejections concentrated by tenant or user segment?** A spike that affects all tenants is a regression. A spike that only affects one tenant or one segment is a configuration drift in that tenant's setup.

## How to fix

The remediation depends on what changed:

- **A recent change is the cause.** Roll back. Whatever you shipped in the last 24-48 hours is most likely the culprit. The cost of an unnecessary rollback is small; the cost of a degraded agent staying in production is large. Once rolled back, investigate offline and ship a fixed version with explicit test coverage on the regressed behavior.

- **No recent change but rejections are real.** Look for upstream changes: a provider model update (Anthropic and OpenAI both ship transparent improvements that can shift output style), a tokenizer or embedding change, a third-party API behavior change (cross-reference with `tool_schema_drift` groups). If you find one, decide whether to revert your dependency, pin a version, or accept the change with calibration.

- **Edit-only spike with no rejections.** This is quality drift, not regression. The agent is close but consistently off. Read the edits to identify the pattern: if humans are always adding citations, your prompt is missing a citation requirement; if humans are always softening tone, your system prompt is too aggressive.

## How to test the fix

After deploying the fix, watch the failure_group's affected_executions count. Within an hour the count should stop growing. Within several hours the older entries should age out of the 1-hour detection window and the detector should clear (assuming new HITL traffic comes through).

Leading indicators:

- Mean wait_duration_ms drops because humans are spending less time correcting outputs
- Approval rate (the fraction of `human_intervention` events with `response_kind=approved`) climbs back to the project's historical baseline
- No new HITL executions land in the failure_group

## A note on the threshold calibration

The 40 percent and 30 percent thresholds are intentionally conservative to avoid false positives at the cost of slower detection. A project running at 20 percent rejections steady-state will not trip the detector; a project that spikes from 10 percent to 50 percent inside an hour will. The thresholds use Mesedi-wide defaults today and are not per-project tunable. If you suspect a regression that did not trigger the detector, look at the project's rolling rejection rate directly in the dashboard and treat the rolling-rate trend as the authoritative signal.
