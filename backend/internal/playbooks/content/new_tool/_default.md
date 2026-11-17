# New tool

This is a notice, not a failure. This execution is the first in the project to use a tool Mesedi had not seen before. The signature is the tool's name, or `server/method` for an MCP call. The notice is linked to the execution but never becomes its classification, so any real finding on the same run is still the one shown first.

## What this means

A new tool is usually a release: someone added a capability to the agent. It is worth a glance because it is also how an unexpected capability arrives: an MCP server that started advertising a new method, a framework update that registered extra tools, or a configuration change nobody meant to ship.

From this first run on, Mesedi remembers the tool's description and declared schema as they were first recorded. A later run whose description or schema differs is reported as `tool_schema_drift` with a `first:` signature, from the first changed call.

## What to do

1. Confirm the tool is one you meant to add. Check where it comes from: your own code, a framework, or an MCP server.
2. For a third-party or MCP tool, read the description the model is shown. Instructions in a description reach the model on every run.
3. If the tool should never change without review, pin its description and definition with `PUT /me/tool-pins/{tool_name}/{kind}`. A pin replaces the first-seen baseline for that half of the contract.

## Limits

Mesedi sees only tools your SDK records calls for. A tool that is registered but never called raises no notice until its first recorded call. The first-seen baseline is the oldest call Mesedi holds when the tool is first checked, so for a tool already in use before this check existed it is the oldest call still stored, not necessarily the tool's original version.
