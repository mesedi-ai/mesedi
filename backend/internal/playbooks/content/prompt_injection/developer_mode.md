# Prompt injection, developer / admin / jailbreak mode

An `llm_call` event in this execution had a `user_message` invoking a **mode-override persona**, strings like "developer mode," "jailbreak mode," or "admin mode." Mesedi treats this as a Tier-2 signal: the phrases are distinctive enough to be a strong injection signal in user input, but they can appear legitimately in developer-tool contexts and in conversations about LLM behavior.

## What this means

The attack template is "tell the model it has a hidden mode where its normal rules don't apply." Variants of this template have been circulating since the first ChatGPT jailbreaks: the user asserts the existence of a special operating mode (developer mode, debug mode, admin mode, factory mode, root mode), describes its supposed behavior (no filters, no constraints, willing to discuss anything), and then asks the model to either enter that mode or simulate its output.

Like DAN, the attack mostly fails against modern frontier models because they've been trained against it specifically. But it persists in two contexts: as an opening probe by less-sophisticated attackers, and as a building block inside more sophisticated multi-turn manipulation. The signature firing tells you someone is at least trying.

## How to investigate

Open the affected execution's timeline and find the matching `llm_call`. Three diagnostics:

1. **Did the model refuse or comply?** Read the assistant response. A clean refusal means the model's safety held. A response that addresses the meta-claim (acknowledges "developer mode," explains it doesn't have one, refuses) is also a safety-held outcome. A response that adopts the persona or produces content the constrained model would refuse is an actual breach.

2. **What's the request inside the wrapper?** Same as DAN, the mode-override is the carrier, not the goal. Identify what the user is actually asking for once the persona setup is stripped away.

3. **Pattern across executions.** A user who attempts developer-mode once and gives up is a probe. A user who tries developer-mode → DAN → role-override in quick succession is iterating, and the multi-attempt sequence itself is more informative than any single pattern firing.

## How to fix

Same posture as the DAN playbook, the model usually handles this, the remediation is around application policy:

- **Decide your refusal posture and apply it consistently.** Decide whether to refuse with a canned message, respond to the underlying request while ignoring the persona, or surface the attempt to a moderation queue. Pick one and apply it consistently. Inconsistent responses teach attackers which permutation works.

- **Defense in depth.** Don't rely solely on the model's safety training. Layer an application-side classifier or rule set that flags or blocks content categories your product won't serve regardless of how the model is asked. Modern frontier safety is excellent but not infinite.

- **Rate-limit on injection-pattern bursts.** When several injection patterns fire within a short window from the same user/session, increase response latency or require additional auth. The asymmetry is on your side: a one-second delay costs you nothing and costs an iterating attacker their iteration speed.

- **Audit the request rate of legitimate developers in your product.** If you have a developer-tool product where "developer mode" is a legitimate concept, the signature will fire constantly and the failure-group count is meaningless. Per-project tuning (Mesedi v2) will let you opt out for those projects.

## What this does NOT mean

Three legitimate sources of this pattern in your traffic:

- A developer-tool product (debugger, IDE assistant, code reviewer) where the user is asking about literal developer mode in some other system.
- An LLM-education or LLM-research product where discussing jailbreak templates is in scope.
- A red-team workflow where you yourself are testing your defenses by sending these prompts.

For all three, the high-count failure group is expected and not actionable. Per-project pattern tuning is the long-term answer; in the short term, treat the group as known noise.

## Auto-fix in a future Mesedi release

Same as the other Tier-2 patterns, v2 roadmap includes automatic intent classification (what is the user actually trying to do, once the persona wrapper is stripped) and per-project rule tuning. Both deferred until the v1 detection surface stabilizes.
