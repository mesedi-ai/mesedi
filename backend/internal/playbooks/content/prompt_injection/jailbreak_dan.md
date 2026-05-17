# Prompt injection — DAN-style jailbreak

An `llm_call` event in this execution had a `user_message` invoking the **DAN ("Do Anything Now") persona** or close variants. The matching regex catches `do anything now`, the literal `DAN` token, and `act as DAN`. Mesedi treats this as a Tier-2 signal because the strings are highly distinctive — they show up in plain user prose almost never.

## What this means

DAN is the longest-running, most-copied jailbreak template in the wild. It works (when it works) by asking the model to roleplay as an alternative version of itself that is "free from constraints," then chaining that into requests the constrained model would refuse. Hundreds of variants exist; most of them are now well-known to model safety teams and trigger refusal patterns directly. But the template itself, especially the literal "DAN" token, is still in active circulation as the entry point for less-sophisticated attacks.

If you see a DAN attempt against your agent, the user has crossed three thresholds: they've decided your agent is worth attacking, they've gone to a jailbreak forum or LLM hobbyist resource to grab a template, and they've pasted it in roughly verbatim. The first two thresholds matter for product reasoning; the third matters because copy-paste DAN attempts have very low success rates against current frontier models. The more concerning pattern is when DAN appears as an opening probe and later attempts use more sophisticated wording.

## How to investigate

Open the affected execution's timeline and find the matching `llm_call`. Three diagnostics:

1. **Did the model refuse, comply, or partially comply?** Read the assistant response. Modern frontier models almost always refuse DAN directly — if your model produced a refusal, the safety RLHF held. If it complied or partially complied (started in DAN voice, addressed the meta-request), you have an actual jailbreak success and the response should be reviewed manually.

2. **What was the actual request inside the DAN wrapper?** DAN is almost never the goal — it's the carrier for something else. Look at the part of the prompt that comes after the DAN setup. That's what the user actually wants and that's what tells you whether the attempt is hostile (asking for harmful content, asking to bypass terms of service) or curious (testing whether your safety holds, no real downstream use).

3. **Is this the only execution from this user/session?** Check the affected-executions table. A single DAN attempt is usually a one-off probe. Multiple attempts from the same source with escalating sophistication is a campaign worth flagging to whoever handles your trust-and-safety surface.

## How to fix

DAN attempts mostly bounce off frontier model safety on their own, so the remediation here is less "patch the model" and more "patch the application policy around the model":

- **Decide your refusal posture explicitly.** Some products refuse with a generic message, some respond with a topical answer that ignores the meta-request, some flag the session for review. Pick one and apply it consistently. Inconsistency teaches attackers what works.

- **Don't rely on the model alone.** Model-side safety is one defense; application-side policy is another. If your product has content categories that should be refused regardless of how the model was asked, enforce that at the application layer with a separate classifier or rule set. Defense in depth.

- **Rate-limit by user/IP/session when a jailbreak pattern fires.** An attacker who's iterating on jailbreak attempts will produce 50 of them in a session. Make the 50th attempt take 10 seconds longer than the first and most attackers give up.

- **Log the surrounding context.** When this playbook fires, the structured signal you want is "what was the user trying to get?" — not just "they tried DAN." Capture the prompt context and review periodically for trends.

## What this does NOT mean

DAN appears legitimately in academic security research, in red-team prompts you yourself wrote to test your defenses, and in agents whose purpose is to study or demonstrate jailbreaks. Same per-project tuning caveat as the other injection patterns — Mesedi v2 will let you opt out.

## Auto-fix in a future Mesedi release

Tier 2 capabilities on the roadmap include automatic classification of "what is the user actually trying to do" when an injection signature fires — strips the jailbreak wrapper and surfaces the underlying request as a separate signal. That makes the human review step much faster.
