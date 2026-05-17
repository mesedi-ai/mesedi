# Prompt injection — instruction-tag spoofing

An `llm_call` event in this execution had a `user_message` containing **literal chat-template control tokens** — `[INST]`, `[/INST]`, `<<SYS>>`, or `<</SYS>>`. These strings are the Llama-family instruction-tuning sentinels; some other model families have their own. Mesedi's prompt-injection detector treats them as a Tier-1 (high-precision) signal because they almost never appear in benign user input.

## What this means

When user-supplied text contains the exact bytes a model uses internally to demarcate "system instruction" vs "user message," there are only two possibilities: an attacker is trying to spoof a control-token boundary so their text gets interpreted as a higher-privilege instruction, or some benign upstream pipeline is leaking template scaffolding into user-visible text.

The attack version of this is one of the cleanest possible injection vectors against Llama-derivative models. If the model's tokenizer or your client library doesn't strip these tags before tokenization, the model can interpret the attacker's text as system-level direction. Even when the tokenizer does strip them, the very presence of the tag is a strong adversarial signal — a user who knows enough to type `<<SYS>>` is not casually browsing.

## How to investigate

Open the affected execution's timeline and find the `llm_call` event with the matching `user_message`. Three things to determine in order:

1. **Where did the tagged text come from?** If it's a single field your application populated from user input, you have a real injection attempt and the affected user / IP / session is identifiable. If it's coming from a downstream tool's output (a web-scrape, an email body, a PDF extraction), you have indirect injection via tool output — the user didn't type it, the tool returned it.

2. **What did the model do with it?** Read the assistant response that came back. If the model followed the spoofed instruction (changed format, ignored a safety rule, leaked a system prompt), the injection succeeded and the model needs hardening. If the model produced its normal output, the injection failed and you got off lucky — fix the input path before the next attacker tries a more sophisticated version.

3. **Is this the only execution from this user/session, or one of many?** Check the affected-executions table below. A single instance is a probe; multiple instances with similar tags are an active campaign.

## How to fix

Two categories of fix and both should land:

- **Strip the tags before sending to the model.** Run user-controlled input through a normalizer that deletes or escapes `[INST]`, `[/INST]`, `<<SYS>>`, `<</SYS>>` (and their model-family equivalents) before it goes into the `user_message`. This is a 5-line regex and it eliminates the entire class of attack at the input layer. It also prevents accidental leakage from benign upstream pipelines.

- **Add input length/character-class checks.** Most legitimate user input is plain prose. Inputs that contain control-token-style brackets, unusual unicode, or extreme length are signal-rich. Reject or flag them at the application layer before they reach the model.

A useful instrumentation pattern: when your normalizer fires and strips a tag, emit a custom event (`injection_tag_stripped`) into the Mesedi telemetry stream. That gives you a measure of attack volume separate from the "got past everything" signal that this playbook fires on.

## What this does NOT mean

If your application legitimately produces text containing these tags (a code-generation product, an LLM tutorial, a security tool that shows users injection examples), this detector will fire on every execution and the failure-group count will be misleading. In that case the right fix is per-project pattern config — Mesedi v2 will support per-project rule tuning so legitimate uses can opt out of specific patterns. Until then, treat the high-count failure group as expected noise rather than active attack.

## Auto-fix in a future Mesedi release

The v2 roadmap includes SDK-side input normalization — the `@wrap` decorator can transparently strip known control tokens before they reach the model, emitting a structured event for each strip. Opt-in per project. The detection here remains valuable as the canary for "the normalizer missed something" — if this failure group keeps firing after normalization is on, your normalizer is incomplete.
