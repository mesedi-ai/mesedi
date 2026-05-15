/**
 * Mesedi TypeScript SDK — Guardians for Autonomous AI.
 *
 * Public API:
 *
 *   configure(opts)          — set up the module-level default client.
 *                              Reads MESEDI_API_KEY / MESEDI_BASE_URL
 *                              from process.env as fallbacks.
 *   wrap(opts?, fn)          — higher-order function that records the
 *                              wrapped function's invocation as an
 *                              agent execution.
 *   tool(opts?, fn)          — higher-order function that records each
 *                              call as a tool_call event linked to the
 *                              surrounding wrap()'d execution.
 *   checkpoint(name, meta?)  — mark a notable point in execution.
 *   validatorResult(name, passed, opts?) — report a validator outcome.
 *   instrumentAnthropic(cls?)— patch @anthropic-ai/sdk's
 *                              Messages.create to emit llm_call events.
 *                              Optional `cls` for testing.
 *   flush(timeoutMs?)        — wait for the background shipper to
 *                              drain all events submitted so far.
 *   MesediClient             — explicit client for advanced use cases.
 *   Event, Execution         — wire-format dataclasses.
 *   EventType, Status        — enum-style constants.
 */

export { MesediClient, configure, flush, getClient } from "./client.js";
export type { ConfigureOptions } from "./client.js";
export {
  EventType,
  Status,
  utcNowRfc3339,
} from "./events.js";
export type { Event, Execution } from "./events.js";
export { wrap } from "./wrap.js";
export type { WrapOptions } from "./wrap.js";
export { tool } from "./tool.js";
export type { ToolOptions } from "./tool.js";
export { checkpoint, validatorResult } from "./observe.js";
export type {
  ValidatorResultOptions,
  ValidatorSeverity,
} from "./observe.js";
export { instrumentAnthropic } from "./anthropic_integration.js";
export type { MessagesClassLike } from "./anthropic_integration.js";

export const VERSION = "0.0.2";
