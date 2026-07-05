# Data leakage

Mesedi's DLP scanner caught a credential, signed token, or PII pattern in an outbound payload from this execution. The detector scans every `llm_call` (`system_prompt`, `user_message`, `response_text`, `exception_message`), every `tool_call` (`arguments`, `return_value`, `error`, `exception_message`), and every `validator_result` (`reason`, `message`) against sixteen built-in rules covering Anthropic, OpenAI, Gemini, AWS, GCP, GitHub (PAT + OAuth), Slack, Stripe (live secret + live publishable + live restricted), JWTs, SSNs, credit cards, and private-key PEM blocks — plus any custom regex rules you configured for this project.

The signature is `data_leakage:<rule_id>` — one failure_group per pattern that matched in this project. The matching rule's severity is recorded on the sibling `dlp_scan_result` event (not in the signature) so each rule clusters cleanly regardless of severity. By default `critical` and `high` severities promote to a failure_group; `medium` is scanned and stored but does not promote. Customers can change which severities promote via the per-project `data_leakage.severity_policy` knob: `["critical"]` for less noise, or `["critical", "high", "medium"]` to fire on PII patterns the default skips. The scan itself always runs the full rule set regardless of policy — the knob controls promotion, not detection.

## What's usually happening

Three common causes, in rough order of frequency:

1. **Accidental string interpolation.** Your prompt template, tool wrapper, or response renderer pulled in an environment variable, a session token, or a row from a credentials table. The agent then sent that string to an LLM provider as part of normal operation. The credential was never meant to be in the payload; the template builder didn't know to redact it.

2. **A user pasted a secret into the prompt.** End-users routinely paste API keys, account numbers, or sample tokens into chat-style inputs while testing or asking for help. Your agent forwards the input to the model verbatim.

3. **A tool returned data with secrets embedded.** A search tool retrieved a document that contains a credential. A database tool returned a row that includes a hashed-but-not-redacted password column. The agent ingested the result and may include it in its next prompt.

## How to investigate

Open the execution and look for the sibling `dlp_scan_result` event near the flagged `llm_call`, `tool_call`, or `validator_result`. The event records the `rule_id` that matched, the `highest_severity`, the `scan_layer` (which event-type the hits came from), and a per-rule hit count. The actual secret has already been redacted in Mesedi storage — the event records the type of secret, not its value.

Trace back to where the matching field was built in your code. The most common patterns are an `f"..."` Python string that interpolated a config object, a `JSON.stringify(...)` in TypeScript that included a token field that should have been omitted, or a prompt template that took an entire env dict and dumped it into the model.

## How to fix

The remediation depends on which of the three causes you identified:

- **Template interpolation leak.** Add an allow-list to the prompt builder. Instead of "everything in this object, except secrets," start from an empty payload and only add fields that have been explicitly allowed. This is harder to forget than a deny-list.

- **User-pasted secrets.** Sanitize the input at the boundary before it reaches the model. A simple regex pre-pass against the same patterns Mesedi uses (provider keys, JWTs, SSNs, etc.) lets you redact the value and append a note like "[a credential was removed here]" so the model still understands the intent without seeing the secret.

- **Tool-returned secrets.** Sanitize at the tool wrapper. If a search tool can return credentials, your tool wrapper should run the same DLP scan before returning the result to the agent. Mesedi's DLP rules engine is the same regex set; you can borrow it directly or call into the configured project rules.

## A note on what was stored

Mesedi's DLP scanner redacts the matched value at event ingest, before the event reaches durable storage. The string `[REDACTED:rule_id]` replaces the original (built-in rules) or `[REDACTED:custom-<pattern_id>]` (custom rules — closed the gap where custom rules detected but didn't redact). If you open the event in the dashboard you will see the redaction marker, not the secret. That means the secret is not at rest on Mesedi's side; the issue to remediate is the leak at the source.

## How to test the fix

After deploying the fix, trigger the same code path that produced the original event. The `dlp_scan_result` events should stop appearing on new executions. If they reappear, the fix did not cover all the call sites; widen the search.

## Per-project tunables

Two configurable knobs:

- **`severity_policy`** (default `["critical", "high"]`; closed set — only `"critical"`, `"high"`, `"medium"` are valid; max 3 entries). Which severities promote to a failure_group at execution close. The scan itself always runs the full rule set; this knob only affects promotion. Defensive: empty input or any value outside the closed set reverts the whole slice to the default. Tighten to `["critical"]` for less noise on a high-volume project, or loosen to `["critical", "high", "medium"]` for regulated workloads that want PII patterns to page.
- **Custom DLP patterns** (per-project regex rules). Add via Settings → Security → Custom data_leakage patterns. Each rule has a severity (`low` → SeverityMedium internally; `medium` → SeverityHigh; `high` → SeverityCritical) — the dashboard chip mapping. Matches are scanned, redacted in place, and promote per the same severity_policy as built-in rules.

## A note on severity tiers

Mesedi's DLP severity model has three tiers internally: `critical`, `high`, `medium`. The 16 built-in rules currently use only `critical` (14 rules covering real credentials and signed tokens) and `high` (2 rules — SSN and credit card; PII with non-trivial false-positive risk). The `medium` tier is reserved for custom rules and for future built-in expansion of patterns with higher false-positive rates. A rule's severity is the policy hint for whether it pages, not a confidence score — RE2 matches are exact, not probabilistic.
