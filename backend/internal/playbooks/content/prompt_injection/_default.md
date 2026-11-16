# Prompt injection

An `llm_call` event in this execution matched a prompt-injection pattern that has no playbook of its own. Most often that is one of **your project's custom patterns**: its signature reads `custom:<pattern_id>`, and the pattern ID is the one shown on your project's custom patterns page.

## What this means

Mesedi checks every `user_message` against its built-in injection patterns first, then against the custom patterns your project has added. A custom match means text arrived that your own rule describes as an attack. Mesedi does not know why you wrote the rule, so the judgement of how serious the match is stays with you.

## What to do

1. Open the failure group and read the matched text in the execution's `llm_call` event.
2. Compare it with the custom pattern it matched. A pattern that is too broad catches ordinary text; narrow it, or add an allowlist entry for the benign case.
3. If the text is a real attack, find where it came from: a user, a retrieved document, a tool result, or another agent. Injection through retrieved content is the hardest to see and the most common in agents that browse or search.
4. Check what the model did next in the same execution. A flagged input matters most when the following tool calls or outputs show the model acting on it.

## Limits

A pattern match is a text match. It cannot tell an attack quoted for discussion from an attack in progress, and it cannot catch phrasings your patterns do not describe.
