#!/usr/bin/env node
/**
 * The mesedi-spend-check command (see spend_check.ts). Reads keys from
 * the environment; the provider admin key is sent only to the provider.
 */

import { spendCheckMain, type FetchLike } from "./spend_check.js";

const write = (text: string, err?: boolean) => (err ? process.stderr : process.stdout).write(text + "\n");

spendCheckMain(process.argv.slice(2), process.env, globalThis.fetch as unknown as FetchLike, write).then(
  (code) => process.exit(code),
  (err: unknown) => {
    write(`mesedi-spend-check: ${String(err)}`, true);
    process.exit(2);
  },
);
