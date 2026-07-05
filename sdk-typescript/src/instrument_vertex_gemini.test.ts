/**
 * Unit tests for — instrumentVertexGemini TS SDK.
 * Uses dependency-injected fake classes so no real Vertex SDK or
 * GCP credentials are needed.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import * as clientMod from "./client.js";
import { instrumentVertexGemini } from "./vertex_gemini_integration.js";
import { wrap } from "./wrap.js";

interface CapturedEvent {
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  payload: any;
  event_type: string;
}

class CaptureClient {
  events: CapturedEvent[] = [];
  base_url = "";
  api_key = "";
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  submitEvent(event: any): void {
    this.events.push(event);
  }
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  submitExecutionStart(_e: any): void {
    /* no-op */
  }
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  submitExecutionEnd(_e: any): void {
    /* no-op */
  }
}

let cap: CaptureClient;

beforeEach(() => {
  cap = new CaptureClient();
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  vi.spyOn(clientMod, "getClient").mockReturnValue(cap as any);
});

afterEach(() => {
  vi.restoreAllMocks();
});

describe("Vertex AI Gemini", () => {
  it("emits provider=gemini surface=chat on success", async => {
    class FakeVertexModel {
      modelName = "gemini-2.5-pro";
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      async generateContent(_arg: any): Promise<any> {
        return {
          response: {
            candidates: [
              { content: { parts: [{ text: "vertex output" }] } },
            ],
            usageMetadata: {
              promptTokenCount: 12,
              candidatesTokenCount: 34,
            },
          },
        };
      }
    }
    await instrumentVertexGemini(FakeVertexModel);
    const agent = wrap(async => {
      return await new FakeVertexModel().generateContent("hello vertex");
    });
    await agent();
    const chats = cap.events
      .filter((e) => e.event_type === "llm_call")
      .map((e) => e.payload)
      .filter((p) => p.provider === "gemini" && p.surface === "chat");
    expect(chats.length).toBe(1);
    expect(chats[0].model).toBe("gemini-2.5-pro");
    expect(chats[0].user_message).toBe("hello vertex");
    expect(chats[0].response_text).toBe("vertex output");
    expect(chats[0].input_tokens).toBe(12);
    expect(chats[0].output_tokens).toBe(34);
  });

  it("emits failure event with error_class when generateContent throws", async => {
    class _Err extends Error {
      constructor(msg: string) {
        super(msg);
        this.name = "VertexError";
      }
    }
    class FakeVertexModel {
      modelName = "gemini-2.5-pro";
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      async generateContent(_arg: any): Promise<any> {
        throw new _Err("boom");
      }
    }
    await instrumentVertexGemini(FakeVertexModel);
    const agent = wrap(async => {
      return await new FakeVertexModel().generateContent("x");
    });
    await expect(agent()).rejects.toThrow("boom");
    const failures = cap.events
      .filter((e) => e.event_type === "llm_call")
      .map((e) => e.payload)
      .filter(
        (p) =>
          p.provider === "gemini" &&
          p.surface === "chat" &&
          p.status === "failed",
      );
    expect(failures.length).toBe(1);
    expect(failures[0].error_class).toBeDefined();
  });

  it("is idempotent on re-patch of same class", async => {
    class FakeVertexModel {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      async generateContent(_arg: any): Promise<any> {
        return { response: { candidates: [], usageMetadata: {} } };
      }
    }
    const first = await instrumentVertexGemini(FakeVertexModel);
    const patched1 = FakeVertexModel.prototype.generateContent;
    const second = await instrumentVertexGemini(FakeVertexModel);
    const patched2 = FakeVertexModel.prototype.generateContent;
    expect(first).toBe(true);
    expect(second).toBe(true);
    expect(patched1).toBe(patched2);
  });
});
