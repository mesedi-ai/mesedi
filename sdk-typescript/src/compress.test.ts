import { describe, expect, test } from "vitest";
import { gunzipSync } from "node:zlib";

import {
  DEFAULT_THRESHOLD_BYTES,
  SUPPORTED_ENCODING,
  maybeCompress,
} from "./compress.js";

const enc = new TextEncoder();
const dec = new TextDecoder();

describe("maybeCompress", () => {
  test("below threshold passes through unchanged", () => {
    const body = enc.encode('{"hello":"world"}');
    const { body: out, extraHeaders } = maybeCompress(body);
    expect(out).toBe(body);
    expect(extraHeaders).toEqual({});
  });

  test("at threshold compresses with the right Content-Encoding header", () => {
    const body = enc.encode(
      '{"prompt":"' + "a".repeat(DEFAULT_THRESHOLD_BYTES + 10) + '"}',
    );
    const { body: out, extraHeaders } = maybeCompress(body);
    expect(out).not.toBe(body);
    expect(out.byteLength).toBeLessThan(body.byteLength);
    expect(extraHeaders).toEqual({ "Content-Encoding": SUPPORTED_ENCODING });
  });

  test("compressed payload round-trips through a fresh decoder", () => {
    const original = enc.encode(
      '{"event_type":"llm_call","payload":"' + "x".repeat(2000) + '"}',
    );
    const { body: compressed, extraHeaders } = maybeCompress(original);
    expect(extraHeaders).toEqual({ "Content-Encoding": SUPPORTED_ENCODING });
    const decoded = gunzipSync(compressed);
    expect(dec.decode(decoded)).toBe(dec.decode(original));
  });

  test("custom threshold forces compression on small payloads", () => {
    const body = enc.encode('{"x":"abc"}');
    const { body: out, extraHeaders } = maybeCompress(body, 5);
    expect(out).not.toBe(body);
    expect(extraHeaders).toEqual({ "Content-Encoding": SUPPORTED_ENCODING });
  });

  test("100-event batch compresses to under half its original size", () => {
    const parts: string[] = [];
    for (let i = 0; i < 100; i++) {
      parts.push(
        '{"event_id":"evt_' +
          "x".repeat(24) +
          '","execution_id":"exec_abc123","event_type":"llm_call",' +
          '"payload":{"prompt":"summarize","response":"ok"}}',
      );
    }
    const original = enc.encode("[" + parts.join(",") + "]");
    expect(original.byteLength).toBeGreaterThan(DEFAULT_THRESHOLD_BYTES);
    const { body: compressed, extraHeaders } = maybeCompress(original);
    expect(extraHeaders).toEqual({ "Content-Encoding": SUPPORTED_ENCODING });
    expect(compressed.byteLength).toBeLessThan(original.byteLength / 2);
  });
});
