# Data leakage

Mesedi's DLP scanner caught a credential, signed token, or PII pattern in an outbound payload from this execution. The detector scans every `llm_call` (system prompt, user prompt, response) and every `tool_call` (arguments, return value) against thirteen built-in rules covering AWS, GCP, Stripe, GitHub, Slack, OpenAI keys, JWTs, SSNs, credit cards, and private-key PEM blocks, plus any custom regex rules you configured for this project.

The signature is `data_leakage:<rule_id>` — one failure_group per pattern that matched in this project. The matching rule's severity is recorded on the sibling `dlp_scan_result` event (not in the signature) so each rule clusters cleanly regardless of severity. By default `critical` and `high` severities fire the alert; `medium` is scanned but not paged. Customers can tighten this via the per-project `data_leakage.severity_policy` threshold (e.g. `["critical", "high", "medium"]` to fire on PII patterns the default skips, or `["critical"]` for less noise). The full rule set scans regardless — the policy knob only controls firing.

## What's usually happening

Three common causes, in rough order of frequency:

1. **Accidental string interpolation.** Your prompt template, tool wrapper, or response renderer pulled in an environment variable, a session token, or a row from a credentials table. The agent then sent that string to an LLM provider as part of normal operation. The credential was never meant to be in the payload; the template builder didn't know to redact it.

2. **A user pasted a secret into the prompt.** End-users routinely paste API keys, account numbers, or sample tokens into chat-style inputs while testing or asking for help. Your agent forwards the input to the model verbatim.

3. **A tool returned data with secrets embedded.** A search tool retrieved a document that contains a credential. A database tool returned a row that includes a hashed-but-not-redacted password column. The agent ingested the result and may include it in its next prompt.

## How to investigate

Open the execution and look for the sibling `dlp_scan_result` event near the flagged `llm_call` or `tool_call`. The event records the rule_id that matched, the severity, and the field the match was found in (system_prompt, user_prompt, response, arguments, return_value). The actual secret has already been redacted in Mesedi storage; the event records the type of secret, not its value.

Trace back to where the matching field was built in your code. The most common patterns are an `f"..."` Python string that interpolated a config object, a `JSON.stringify(...)` in TypeScript that included a token field that should have been omitted, or a prompt template that took an entire env dict and dumped it into the model.

## How to fix

The remediation depends on which of the three causes you identified:

- **Template interpolation leak.** Add an allow-list to the prompt builder. Instead of "everything in this object, except secrets," start from an empty payload and only add fields that have been explicitly allowed. This is harder to forget than a deny-list.

- **User-pasted secrets.** Sanitize the input at the boundary before it reaches the model. A simple regex pre-pass against the same patterns Mesedi uses (AWS keys, Stripe tokens, JWTs, etc.) lets you redact the value and append a note like "[a credential was removed here]" so the model still understands the intent without seeing the secret.

- **Tool-returned secrets.** Sanitize at the tool wrapper. If a search tool can return credentials, your tool wrapper should run the same DLP scan before returning the result to the agent. Mesedi's DLP rules engine is the same regex set; you can borrow it directly or call into the configured project rules.

## A note on what was stored

Mesedi's DLP scanner redacts the matched value before storing the event. The string `[REDACTED:rule_id]` replaces the original. If you open the event in the dashboard you will see the redaction marker, not the secret. That means the secret is not at rest on Mesedi's side; the issue to remediate is the leak at the source.

## How to test the fix

After deploying the fix, trigger the same code path that produced the original event. The `dlp_scan_result` events should stop appearing on new executions. If they reappear, the fix did not cover all the call sites; widen the search.
