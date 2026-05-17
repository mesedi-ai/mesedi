# Prompt injection — system-prompt injection

An `llm_call` event in this execution had a `user_message` containing **a literal "system" boundary marker** — `<system>`, `<|system|>`, or a leading `system:` declaration on its own line. Mesedi's prompt-injection detector treats these as a Tier-1 (high-precision) signal because the only legitimate place these tokens appear is in your own server-side prompt construction code, never in user-supplied text.

## What this means

The user-supplied text is trying to declare itself as a system-role message. If your prompt-construction code naively concatenates `user_message` into a multi-role template (`system: ... user: ...`), this attack can hijack the system-role slot of the next turn — the attacker's text becomes the model's instructions.

This is structurally similar to SQL injection: the application code is interpolating attacker-controlled text into a structured format where certain tokens carry control meaning. The model's tokenizer treats `<|system|>` as a privileged delimiter; the application treats it as plain text. The gap between those two interpretations is the entire vulnerability.

OpenAI's and Anthropic's chat APIs are mostly safe from this because the role structure is enforced at the API boundary, not via in-band sentinels. But any code path that builds a single concatenated string from multiple turns — a custom prompt template, a fine-tuned model's training format, a logging or replay system — is exposed.

## How to investigate

Open the affected execution's timeline and find the `llm_call` event with the matching `user_message`. Three diagnostics:

1. **How is the user_message being constructed into a prompt?** If your code is using the API's structured `messages` array, the attack probably failed (the API treats user content as user content regardless of what's inside it). If your code is concatenating into a single string — especially if it's emulating a chat format manually — the attack may have succeeded.

2. **What did the model do?** Read the assistant response. If the response addresses the attacker's spoofed instruction (different tone, different scope, ignored a safety constraint), the injection worked. If the response addresses the surrounding legitimate request, the injection failed.

3. **What's the source of the user_message?** Same triage as instruction_tag: was this user-typed, or was it returned by a tool? Tool-returned attacks are indirect injection and the affected tool needs hardening separately.

## How to fix

Two categories of fix:

- **Use the structured messages API exclusively.** OpenAI's `messages: [{role: "system"}, {role: "user"}]` and Anthropic's equivalent are safe by construction — the role boundary is metadata, not in-band. If you're currently building prompts by string concatenation, migrate. This is the single highest-leverage fix.

- **Strip system-boundary markers from user input at ingest.** A 5-line regex deletes `<system>`, `<|system|>`, and leading `system:` (and the equivalent for any other roles your template uses: `assistant`, `function`, `tool`). Apply this before the text reaches the prompt-construction layer. This is belt-and-suspenders even when you're using structured APIs.

A third measure for high-stakes deployments: after the model produces a response, run a separate verifier model (cheaper, simpler) that re-reads the original user input plus the response and asks "did this response deviate from the documented system prompt?" Flag deviations for human review. Mesedi v2's Tier 2 capabilities include this pattern as a built-in.

## What this does NOT mean

If you're building a developer tool, an LLM educational product, or a security-research interface, your users may legitimately need to type these tokens. Same caveat as `instruction_tag` — per-project pattern config in Mesedi v2 will let you opt out. Until then, treat this failure group as expected noise.

## Auto-fix in a future Mesedi release

Per the repair-tier roadmap, Tier 3 SDK-layer prompt normalization will strip role-boundary tokens automatically and emit structured events for each strip. Tier 2 includes the verifier-model pattern as an opt-in service. Both are deferred until the v1 detection surface is fully validated.
