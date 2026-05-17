# Mesedi — repair-tier roadmap

Strategic note on where "automatic remediation of agent failures" fits in the Mesedi product roadmap, and how it sequences across v1, v1.5, v2, and v3+. Captures decisions made during the 2026-05-15 design pass after weighing whether the repair surface should be its own company.

The short answer: no, repair is a Mesedi feature tier, not a separate company. The medical-clinic metaphor that initially suggested otherwise collapses under examination — humans heal autonomously and doctors operate with bounded standard-of-care liability, but AI agents do not heal and any "treatment" is new code that absorbs full liability into the repairer. The doctor/patient distinction disappears. Adjacent factors compound: seven failure classes split into seven different remediation practices (crash needs debugging, loop needs prompt engineering, tool failure needs upstream tool fixes, prompt injection needs input filtering and system prompt hardening, cost velocity needs budget/model decisions, drift needs retraining, validator failure needs scaffolding rework — that is seven specialist practices, not one clinic); fixes do not generalize across customers because each agent's prompts, tools, RAG corpus, and business context are bespoke; and the unit economics look like consulting at services margins rather than SaaS.

The useful question is not "should this be a separate company" but "which repair tiers ship in which Mesedi release, and how is the auto-fix risk bounded so a wrong fix does not become worse than the original failure." That is what this document is for.

## The repair-ambition gradient

Repair admits four ordered tiers, in increasing order of how much trust Mesedi must hold before it can act:

**Tier 1 — Recommendation.** Failure-class plus signature maps to a human-readable canonical fix. Mesedi tells the developer what is probably broken and what the standard remediation looks like. The developer applies the fix themselves. Zero Mesedi liability. Lowest moat — the value is the playbook content, which competitors can replicate over time, but having the playbook surfaced in the failure-group view is a natural reading surface that locks in dashboard engagement.

**Tier 2 — Suggested code diff.** Mesedi generates a concrete patch or pull request the developer reviews and applies. Still zero Mesedi liability (developer is the actor), but UX risk is real because a bad diff burns trust fast and the value is weak relative to dedicated code-assistant tools (Cursor, Augment Code's Cosmos, Latitude) that already own this layer of the customer's stack.

**Tier 3 — Runtime policy auto-fix at the wrap layer.** Mesedi changes what the agent does next time, without changing customer code. This is the architecturally differentiated tier and the natural extension of Mesedi's existing hard-halt machinery — the SDK already has the entry-point checkpoint where it can intercept and rewrite agent behavior before it commits. Auto-fix policies live per-project, per-failure-class, are opt-in, and never on by default. Mesedi takes liability for the action.

**Tier 4 — Closed-loop auto-apply with auto-rollback.** Mesedi modifies behavior, observes whether the fix worked, rolls back if not, all without human review. Dangerous and not ready until Tier 3 has at least twelve months of production telemetry demonstrating the policies actually work and the rollback path is genuinely safe. Until then, "Mesedi changes production behavior without a human in the loop" is more liability than value.

## Which failure classes are candidates for which tier

The seven Mesedi failure classes split cleanly into auto-fixable and recommendation-only buckets when the question is "can Mesedi safely intervene at runtime without changing customer code." The mapping is the spine of the repair feature; building auto-fix on a recommendation-only class would be a category error that produces a wrong fix and burns more trust than ten correct fixes earn.

**Auto-fixable at runtime (Tier 3 candidates):**

- `cost_velocity` — policy-driven model downgrade, cache injection, tighter rate limit. Easiest to ship cleanly because the intervention does not change agent outputs, only their resource consumption.
- `loops:identical_call` — SDK-layer dedup. When the same hash of (model, user_message) recurs in one execution, return the cached first-call response. Saves money and breaks the loop simultaneously.
- `prompt_injection` — input-sanitization filter applied at wrap-time. Strip recognized injection patterns from user-supplied fields before they reach the LLM. Higher tuning risk than the others because false-positive sanitization can break legitimate prompts that happen to look injection-shaped.
- `drift:model_mix` — pin to previously-validated model. If an unseen model appears, route to the last-validated model until a human approves the new one.
- Subset of `tool_failures` — automatic retry-with-exponential-backoff for transient errors (timeouts, 503s, connection-refused). Skipped for terminal errors (404, auth failures, schema mismatch).

**Recommendation-only (Tier 1 only, never auto-fix):**

- `crashes` — needs actual debugging by a human; the fix is code.
- `validator_failures` — needs reasoning or scaffolding rework; the fix is a structural change to how the agent assembles outputs.
- `loops:similar_call` — usually needs prompt redesign; the agent's prompt template is producing near-duplicates by design.
- `drift:lexical` — usually upstream RAG or training-data fix; the prompts are shifting because their inputs shifted, not because of a runtime mistake.
- `tool_failures` where the tool itself is broken — the customer's tool implementation needs fixing.

## Release sequencing

**Mesedi v1** (currently in-flight, local-only posture, post-LOI launch) — no repair tier yet. The story is "detect, cluster, optionally stop, escalate." All seven failure-class detectors, failure-group dedup, hard-halt with local budgets plus SSE remote channel, dashboard with collapse-by-class view, dogfood substrate, operator Halt button, continuous synthetic-org traffic via launchd, **webhook escalation on first-occurrence of a failure_group with HMAC-signed payloads + retry/backoff + per-project class filter + dashboard UI**. Remaining v1 work: framework adapters (LangChain / CrewAI / Vercel AI SDK), docs and quickstart polish.

**Mesedi v1.5** — Tier 1 (Recommendation). A "Playbooks" feature surfaces canonical fix descriptions per failure-class signature on the dashboard's failure-group detail page. Text only, no actions taken, no Mesedi liability. Ships as a point release between v1 launch and v2.

Status (as of 2026-05-16): **Tier 1 Playbooks v1 shipped (local).** Infrastructure (`backend/internal/playbooks` with `embed.FS` pattern-match resolver, `GET /playbooks` endpoint, dashboard playbook-card with client-side markdown rendering) plus 14 content entries covering every detector-signature shape Mesedi v0.0.1 emits: loops (4 sub-detectors), tool_failures (catch-all), validator_failures (catch-all), prompt_injection (6 per-instance patterns), cost_velocity (catch-all), drift (new_model, lexical_drift). Crashes intentionally has no playbook — stack-trace hashes can't be enumerated and crashes need actual debugging rather than generic guidance.

Future v1.5 work: per-tool playbook overrides where customer base reveals which tools fail most (e.g. `slack_post_message`-specific instead of `_default.md`), per-validator overrides on the same pattern, per-project pattern tuning so legitimate uses can opt out of specific injection signatures.

**Mesedi v2** — Tier 3 (Runtime auto-fix) for the safe classes. Five auto-fix paths (cost-velocity downgrade, identical-call dedup, prompt-injection sanitization, drift-model-mix pin, tool-failure retry) plus three pieces of cross-cutting infrastructure:

1. **Per-project per-class policy config** extends task #83's `project_alert_configs` table from "observe / halt / webhook" to "observe / halt / webhook / auto-fix" with auto-fix-specific parameters per class. Always opt-in. Never on by default.

2. **Rollback infrastructure** — every auto-fix action is logged with a `policy_action_id`, and the dashboard exposes a "Revert this policy" control. Without rollback, Tier 3 ships broken. The discipline is: you cannot take an action you cannot undo.

3. **Auto-fix effectiveness telemetry** — when a policy auto-fixes an execution, does the same failure class recur on the next run? If yes, the policy did not solve the underlying issue and should be flagged for human review. Without this you have no signal whether auto-fix is helping or papering over a deeper problem.

Roughly eight to ten weeks if built sequentially, less if two or three auto-fix paths ship in parallel.

**Mesedi v3+** — deferred indefinitely:

- **Tier 2 (suggested code diffs)** — UX risk high, build cost real (LLM generation plus code-context retrieval), differentiation weakest. Dedicated code-assistant tools eat this layer of the customer stack better than a Mesedi side-feature can. Revisit only if a strong customer signal emerges.
- **Tier 4 (closed-loop auto-apply with auto-rollback)** — only after Tier 3 production telemetry justifies it.

## The Verdifax handoff trigger

The repair tier creates a feature dependency that did not exist for pure observation: once Mesedi auto-modifies agent behavior in production, regulated customers (banks, hospitals, life-sciences, defense) will demand cryptographic proof of what changed and when. Tier 3 auto-fix in a SR 11-7-governed environment is unshippable without an answer to "prove what your policy did and when" — and that answer is exactly the LegalEvidenceArtifact plus Sigstore Rekor anchor plus offline-verifier triad Verdifax already builds.

This makes the Verdifax outreach outcome materially less binary:

- **If Verdifax sells in the current outreach window:** the acquirer gets cryptographic attestation as a standalone product. Mesedi v2 builds Tier 3 auto-fix and adds a partner integration to the acquirer's attestation surface (or to a generic cryptographic-evidence vendor) for the regulated-customer subset.

- **If Verdifax does not sell:** Mesedi v2's Tier 3 auto-fix and Verdifax's attestation primitives merge into a single product. The Verdifax codebase's CRES, manifest, and LegalEvidenceArtifact stack fits underneath Mesedi's policy-action log naturally. Net effect: one larger product instead of two smaller ones, with the cryptographic-evidence layer becoming a Mesedi feature tier for customers in regulated industries who need it.

Either outcome leaves Mesedi positioned correctly for its v2. Sale clarifies the partner integration story; no-sale clarifies the unified-product story. The repair-tier roadmap proceeds under either branch — the auto-fix paths are the load-bearing v2 work regardless, and the attestation question is a feature-source decision that resolves itself based on the outreach outcome.

## What to do with this document

This file lives alongside `V2_DEFERRAL_NOTES.md` as a forward-looking strategy doc. Re-read at the start of each v1.5 / v2 planning session. When work on a tier actually starts, move the active items into `DEVELOPMENT_CHECKLIST.md` as concrete phases / sub-slices; leave the strategic rationale here so future engineers and any acquirer-side technical readers can reconstruct the decision-making provenance.

This document is intended to be readable by acquirers, pilot users, and future Mesedi engineers — it answers the natural question "where does Mesedi go after v1" with specific scoped intentions rather than vague promises.
