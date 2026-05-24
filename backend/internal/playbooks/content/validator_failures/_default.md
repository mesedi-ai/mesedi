# Validator failure

A `validator_result` event in this execution recorded `passed=false`. Mesedi's validator-failure detector groups affected executions by **validator name**, the signature on this failure group IS the name of the validator that rejected the output.

Validators are the last line of defense between your agent and the user. Where tool failures fire when an external dependency broke, validator failures fire when **your own code** decided the agent's output was unacceptable. The classification exists because validator failures are signal-rich: a validator rejecting the output means somebody already thought about what "wrong" looks like and wrote the check, and now the check is firing.

## Why this is one of the most useful failure classes

Three things make validator failures uniquely informative:

1. **The validator name tells you exactly what's wrong without further investigation.** A signature of `output_schema_match` means the model returned malformed JSON. `pii_redaction` means PII slipped through. `length_within_bounds` means the model wrote too much or too little. The diagnostic question "what is broken" is already answered.

2. **You wrote the validator, so you know what passing looks like.** Unlike crashes (where the failure is whatever the exception happens to be) or tool failures (where the failure is whatever the upstream API decided), validator failures have a documented success criterion in your own codebase.

3. **High counts on a single validator name mean the agent has a stable failure mode.** This is the most fixable kind of telemetry, same validator firing on many executions means a prompt-tuning or output-format change will move the whole population at once.

## How to find the bug

Open one of the affected executions in the timeline. Two diagnostics:

1. **The `validator_result` event payload.** Includes the validator name, `passed=false`, and (if your validator emits it) a structured rejection reason, which field failed, what the expected vs actual values were, which schema rule tripped. If your validator only emits a boolean and no reason, you've found a soft bug in the validator itself, improve its rejection message and you'll diagnose every future failure faster.

2. **The `llm_call` event(s) immediately before it.** The output that failed validation is in the assistant message of the last LLM call. Comparing that text against the validator's success criterion is the entire debugging session, you're looking for the gap between what the model produced and what your code requires.

## How to fix

The remediation depends on whether the model is wrong, the validator is wrong, or the prompt is wrong:

- **The model is wrong.** Most common case. The output is plausible but fails a real requirement (JSON malformed, required field missing, length out of bounds). Fix by tightening the prompt, show the model an explicit example of valid output, name the failure mode you want to avoid, use function-calling or structured-output if the model supports it for this task. If a smaller model is producing the bad output, try a bigger one; if a bigger one is also failing, the prompt is underspecified.

- **The validator is wrong.** The output is actually fine and the validator is over-strict. Common when validators are written from the happy-path output and then exposed to edge cases. Look at the rejected output in isolation, if it's actually correct, loosen the validator. The cost of a too-strict validator is a higher false-positive rate that desensitizes you to the real failures.

- **The prompt is wrong.** The model is doing exactly what the prompt asks, but what the prompt asks isn't what the validator wants. Common when the validator was added after the prompt and the two evolved separately. Re-derive the prompt from the validator's success criterion and you'll close the gap.

## A useful debugging shortcut

If a single validator is producing a high-count failure group, take 10 of the affected executions, extract their failed outputs, and read them as a batch. Three patterns:

- **All 10 fail the same way.** Structural prompt issue. One fix moves the whole population.
- **The 10 cluster into 2-3 distinct failure modes.** Multiple sub-bugs sharing one validator. Address them in order of frequency.
- **All 10 fail differently and the validator's correct each time.** The agent is genuinely producing bad output across a wide distribution, your task is too hard for the current prompt + model, escalate the model class or decompose the task.

## Auto-fix in a future Mesedi release

The v2 roadmap includes per-validator playbook overrides, when your `pii_redaction` validator fails, the playbook can name the specific PII patterns most likely to be leaking through, instead of giving generic guidance. The pattern table already supports per-validator overrides; the gating constraint is authoring content for the validators that fail most across the customer base.

There's also a Tier 2 capability on the roadmap where Mesedi suggests a prompt diff in response to a validator failure, "executions matching this validator failure share these three prompt characteristics; here's a candidate fix." That requires LLM inference at recommendation time and is opt-in per project.
