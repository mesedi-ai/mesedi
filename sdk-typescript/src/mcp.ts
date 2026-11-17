/**
 * Model Context Protocol call observation.
 *
 * Moved verbatim out of observe.ts on 2026-09-10 as the mirror of
 * the Python SDK's mesedi/mcp.py split. Re-exported from observe, so
 * every existing import keeps working.
 */

import { getClient } from "./client.js";
import { currentExecutionContext, newEventId } from "./context.js";
import { cleanEffects } from "./effects.js";
import { Event, EventType, utcNowRfc3339 } from "./events.js";
import { inputSchemaHash } from "./jcs.js";

/**
 * Options for emitMcpCall. Mirrors the Go MCPCallPayload struct in
 * backend/attest/events/types.go field-for-field, omitting
 * `serverName` and `method` which are positional args.
 */
export interface McpCallOptions {
  /** Optional server URL or stdio target for the expanded view. */
  serverUrl?: string;
  /** Method arguments (any JSON-serializable value). */
  arguments?: unknown;
  /** Successful return value; omit on error. */
  returnValue?: unknown;
  /** Wall-clock duration in milliseconds. */
  latencyMs?: number;
  /** Error message when the call failed. */
  error?: string;
  /** Failure classifier: "hard_error" / "soft_error" / "timeout" / "server_unreachable" / "method_not_found". */
  errorClass?: string;
  /**
   * The method's DECLARED input schema as the MCP server advertises
   * it (the tool definition's inputSchema member). Only a canonical
   * SHA-256 is sent, feeding the definition-drift detector. Pass it
   * fresh from the server's current tool listing on each call, not a
   * copy cached at startup: a poisoned server swaps the definition
   * between calls and a cached copy hides exactly that swap.
   */
  inputSchema?: unknown;
  /**
   * The description the server advertises for this method, the text
   * the model reads. Checked against the first description Mesedi
   * recorded for "server/method" and scanned for injected
   * instructions. Pass it fresh from the server's current tool
   * listing, for the same reason as inputSchema.
   */
  toolDescription?: string;
  /**
   * The places this call changed ("what this call changed"), in your
   * own names: a repository, a table, a bucket path. Read with the
   * run's effect declaration by the effect_aliasing check (see
   * declareEffects).
   */
  effects?: string[];
}

/**
 * Emit an `mcp_call` event for one Model Context Protocol server
 * invocation.
 *
 * Use this when your agent talks to an MCP server (Anthropic's
 * filesystem / github servers, a customer-hosted MCP server, etc.).
 * The dashboard renders MCP calls in a distinct chip so cost
 * attribution can break down by server identity, and the existing
 * tool_failures detector picks up failed MCP calls when `error` or
 * `errorClass` is non-empty.
 *
 * Outside `wrap()`: silent no-op.
 */
export function emitMcpCall(
  serverName: string,
  method: string,
  opts: McpCallOptions = {},
): void {
  const ctx = currentExecutionContext();
  if (!ctx) return;

  const payload: Record<string, unknown> = {
    server_name: serverName,
    method,
  };
  if (opts.serverUrl) payload["server_url"] = opts.serverUrl;
  if (opts.arguments !== undefined) payload["arguments"] = opts.arguments;
  if (opts.returnValue !== undefined) payload["return_value"] = opts.returnValue;
  if (opts.latencyMs) payload["latency_ms"] = Math.trunc(opts.latencyMs);
  if (opts.error) payload["error"] = opts.error;
  if (opts.errorClass) payload["error_class"] = opts.errorClass;
  {
    const schemaHash = inputSchemaHash(opts.inputSchema);
    if (schemaHash) payload["input_schema_hash"] = schemaHash;
  }
  if (opts.toolDescription && opts.toolDescription.trim()) {
    payload["tool_description"] = opts.toolDescription;
  }
  {
    const effects = cleanEffects(opts.effects);
    if (effects.length) payload["effects"] = effects;
  }

  const client = getClient();
  const event: Event = {
    event_id: newEventId(),
    execution_id: ctx.executionId,
    event_type: EventType.MCP_CALL,
    sequence: ctx.nextSequence(),
    timestamp: utcNowRfc3339(),
    duration_ms: opts.latencyMs,
    payload,
  };
  client.submitEvent(event);
}
