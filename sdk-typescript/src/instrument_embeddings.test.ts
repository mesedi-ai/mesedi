/**
 * Unit tests for non-chat surface instrumentation in the TS
 * SDK. Uses dependency-injected fake classes (same pattern as
 * instrument_throttling.test.ts) so no real SDK calls happen.
 */

import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";

import * as clientMod from "./client.js";
import * as openaiMod from "./openai_integration.js";
import * as cohereMod from "./cohere_integration.js";
import * as geminiMod from "./gemini_integration.js";
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
    /* no-op; prevents real shipper from posting to api.mesedi.ai */
  }
  // eslint-disable-next-line @typescript-eslint/no-explicit-any
  submitExecutionEnd(_e: any): void {
    /* no-op; same reason */
  }
}

function llmCallPayloads(cap: CaptureClient): CapturedEvent["payload"][] {
  return cap.events
    .filter((e) => e.event_type === "llm_call")
    .map((e) => e.payload);
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

describe("OpenAI embeddings", () => {
  it("emits surface=embeddings on success", async () => {
    class FakeEmbeddings {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      async create(_args: any): Promise<any> {
        return { usage: { prompt_tokens: 42 } };
      }
    }
    openaiMod.patchEmbeddings(FakeEmbeddings);
    const agent = wrap(async () => {
      return await new FakeEmbeddings().create({
        model: "text-embedding-3-small",
        input: "hello world",
      });
    });
    await agent();
    const embeds = llmCallPayloads(cap).filter(
      (p) => p.surface === "embeddings",
    );
    expect(embeds.length).toBe(1);
    expect(embeds[0].provider).toBe("openai");
    expect(embeds[0].input_tokens).toBe(42);
    expect(embeds[0].output_tokens).toBe(0);
    expect(embeds[0].user_message).toBe("hello world");
  });
});

describe("OpenAI image + audio", () => {
  it("emits surface=image", async () => {
    class FakeImages {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      async create(_args: any): Promise<any> {
        return { data: [] };
      }
    }
    openaiMod.patchImages(FakeImages);
    const agent = wrap(async () => {
      return await new FakeImages().create({
        model: "gpt-image-1",
        prompt: "a red apple",
      });
    });
    await agent();
    const imgs = llmCallPayloads(cap).filter((p) => p.surface === "image");
    expect(imgs.length).toBe(1);
    expect(imgs[0].user_message).toBe("a red apple");
    expect(imgs[0].response_text).toBe("<image output>");
  });

  it("emits surface=audio_stt for transcriptions", async () => {
    class FakeT {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      async create(_args: any): Promise<any> {
        return { text: "hello" };
      }
    }
    openaiMod.patchAudioTranscriptions(FakeT);
    const agent = wrap(async () => {
      return await new FakeT().create({ model: "whisper-1", file: "a.mp3" });
    });
    await agent();
    const stts = llmCallPayloads(cap).filter((p) => p.surface === "audio_stt");
    expect(stts.length).toBe(1);
    expect(stts[0].user_message).toContain("a.mp3");
  });

  it("emits surface=audio_tts for speech", async () => {
    class FakeS {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      async create(_args: any): Promise<any> {
        return {};
      }
    }
    openaiMod.patchAudioSpeech(FakeS);
    const agent = wrap(async () => {
      return await new FakeS().create({
        model: "gpt-4o-mini-tts",
        voice: "alloy",
        input: "read this",
      });
    });
    await agent();
    const ttss = llmCallPayloads(cap).filter((p) => p.surface === "audio_tts");
    expect(ttss.length).toBe(1);
    expect(ttss[0].user_message).toBe("read this");
    expect(ttss[0].response_text).toBe("<audio output>");
  });
});

describe("Cohere embed + rerank", () => {
  it("emits surface=embeddings on embed", async () => {
    class FakeC {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      async embed(_args: any): Promise<any> {
        return { meta: { billed_units: { input_tokens: 99 } } };
      }
    }
    cohereMod.patchEmbedV1(FakeC);
    const agent = wrap(async () => {
      return await new FakeC().embed({
        texts: ["doc1", "doc2"],
        model: "embed-english-v3.0",
      });
    });
    await agent();
    const embeds = llmCallPayloads(cap).filter(
      (p) => p.surface === "embeddings",
    );
    expect(embeds.length).toBe(1);
    expect(embeds[0].provider).toBe("cohere");
    expect(embeds[0].input_tokens).toBe(99);
  });

  it("emits surface=rerank with result count", async () => {
    class FakeR {
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      async rerank(_args: any): Promise<any> {
        return { results: [{}, {}, {}] };
      }
    }
    cohereMod.patchRerankV1(FakeR);
    const agent = wrap(async () => {
      return await new FakeR().rerank({
        query: "best apples",
        documents: ["red", "green", "blue"],
        model: "rerank-english-v3.0",
      });
    });
    await agent();
    const rrs = llmCallPayloads(cap).filter((p) => p.surface === "rerank");
    expect(rrs.length).toBe(1);
    expect(rrs[0].response_text).toBe("<rerank results: 3>");
    expect(rrs[0].user_message).toContain("best apples");
  });
});

describe("Gemini embedContent", () => {
  it("emits surface=embeddings", async () => {
    class FakeGemini {
      modelName = "text-embedding-004";
      // eslint-disable-next-line @typescript-eslint/no-explicit-any
      async embedContent(_arg: any): Promise<any> {
        return { embedding: [0.1, 0.2, 0.3] };
      }
    }
    geminiMod.patchGeminiEmbedContent(FakeGemini);
    const agent = wrap(async () => {
      return await new FakeGemini().embedContent({ content: "hello world" });
    });
    await agent();
    const embeds = llmCallPayloads(cap).filter(
      (p) => p.surface === "embeddings",
    );
    expect(embeds.length).toBe(1);
    expect(embeds[0].provider).toBe("gemini");
    expect(embeds[0].user_message).toBe("hello world");
  });
});
