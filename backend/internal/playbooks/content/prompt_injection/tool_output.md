# Prompt injection, through tool output

Something a tool or MCP server returned to the agent in this execution matched an injection pattern. The signature reads `tool_output:<rule>`: the rule is the same built-in pattern or custom pattern that is checked on prompts. The `injection_scan_result` event beside the call names the tool (or `server/method` for an MCP call) and keeps a short passage around the match.

## What this means

This is indirect prompt injection. The text did not come from your user or your system prompt; it came back from a web page, a document, an email, a search result, an API or an MCP server the agent called. The model reads a tool's answer as part of its context, so instructions planted there can steer what it does next. It is the most common injection path in agents that browse, read files or call third-party servers.

## What to do

1. Open the `injection_scan_result` event and read the passage. Decide whether it is an instruction aimed at the model or ordinary text that quotes one (a security article, a test fixture).
2. Look at the calls that follow it in the same execution. A planted instruction matters most when the next tool calls do what it asked: send data somewhere, change a permission, call a tool the task did not need.
3. Find where the text came from. For a fetched page or document, note the address; for an MCP server, note the server and method. Untrusted sources should not be able to reach tools that make changes.
4. Contain it in your own code: mark tool output as data in the prompt (for example inside clearly labelled delimiters), keep tools that make changes behind a confirmation step, and do not let content fetched from outside choose the next destination on its own.

## The setting

The scan is on by default. A project whose agent routinely reads trusted text full of phrases like "ignore the above" can turn it off with the `prompt_injection` setting `scan_tool_output`. The scan of prompts is not affected.

## Limits

A pattern match is a text match. It cannot tell an attack quoted for discussion from an attack in progress, it cannot catch phrasings the patterns do not describe, and it sees only what your SDK recorded: a tool whose return value is not recorded is not scanned. Text hidden in an image or in markup the model reads but the record does not keep is not seen.
