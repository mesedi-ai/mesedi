# Mesedi v2 — explicitly deferred work

Items intentionally deferred from the v1 roadmap to a v2 conversation. Each entry records what the deferred work is, why it was deferred (not because it's low value but because it's high-cost-and-not-yet-blocking), and what we plan to ship instead in v1 as the "good enough" substitute.

Owner: solo founder, v1 build window 2026-05 → 2026-09 (8-week Verdifax-outreach window plus follow-on). Re-evaluate this file at the start of each v2-planning session.

---

## Replay-UI scrubber (true play / pause / rewind player over event timeline)

**Originally surfaced:** 2026-05-15, in the "human-in-the-loop escalation" capability discussion. The original spec called for a "replay-first debugging UI" where humans called in via webhook escalation could play the agent's actions forward and backward, scrubbing through the execution timeline interactively, seeing exactly what the agent was thinking and doing when it failed.

**v1 substitute (already shipped):** the execution-detail page renders the full event timeline statically. Each `llm_call`, `tool_call`, `checkpoint`, `validator_result` row shows in sequence order with timing data, status badges, and an expandable `▸ payload` reveal that dumps the full JSON. Humans investigating a failure can scroll the timeline, expand payloads in any order, and copy reproduction details. It's not interactive scrubbing — there's no play button, no "pause at step 7," no DAG-style branching — but the data needed to debug a failure is all there in one screen.

**Why deferred to v2:**

1. **Engineering cost.** A true scrubber UI is a real frontend build: timeline component with frame-style stepping, state-diff visualization at each step, the ability to "rewind to checkpoint X and inspect the agent's context as it was then." That's 1-2 weeks of focused frontend work minimum, probably more once the cross-event-type state model gets nailed down.
2. **Production-dashboard dependency.** The v1 dashboard is local-only HTML/JS embedded in the Go binary. The full production dashboard is already deferred to post-LOI as Next.js + Clerk + Vercel — and scrubber UI belongs in that production surface, not retrofitted into the embedded local dashboard. Building scrubber-UI now means building it twice.
3. **Marginal value over the static timeline at v1 scale.** The static timeline already answers 80%+ of the debugging questions a customer would scrubber-up to answer. The remaining 20% (multi-event-type state reconstruction, branching/parallel agent flows) is a v2 problem.

**What unblocks v2 work on this:**

- Production dashboard build started (Phase 13+ work)
- First real pilot user with enough sustained traffic that "I need to scrub through a 50-step execution interactively" is a real recurring pain point, not a hypothetical
- Resolution on the visual-design system (logo, color palette, typography for the production surface) so the player UI is built once on the final design system

**Rough scope estimate for v2 work:** 5-8 days of focused frontend work, depending on whether the player supports multi-execution comparison (sliding window across N parallel agent runs) or just single-execution playback.

---

## Other v1 → v2 deferrals worth keeping in this file

These were deferred earlier in the v1 build and should be re-evaluated alongside the replay-UI work:

### Embeddings-based semantic similarity (extends drift v2 + similar-call)

**v1 reality:** `drift v2` and `similar-call` both use char-3-gram cosine similarity. Pure Go, no dependencies, ships in ~5ms per execution. Catches lexical / surface-level drift and near-duplicate retry loops (timestamp/ID variations).

**Misses in v1:** true semantic paraphrases. "Extract the date from this doc" vs "Find the date mentioned" are obvious paraphrases that have char-3-gram cosine distance ~0.65-0.80 — well above any reasonable threshold. Semantic-similarity detection requires embedding the text via a model (sentence-transformers MiniLM, OpenAI text-embedding-3-small, Voyage AI, etc.) and computing cosine distance in embedding space.

**Why deferred:** embedding infrastructure is a real dependency add (SDK weight, model load, or external API call), plus operational cost (per-event embedding calls), plus storage (vector column on events table, possibly pgvector once we're on Postgres).

**v2 plan:** add an optional embedding path that customers opt into. Same detector code, different similarity computation. SDK-side embedding (lightweight local model) for cost-sensitive customers, server-side embedding (provider API) for accuracy-sensitive customers.

### Production deployment surface (post-Verdifax-LOI)

**Currently deferred:** GitHub org, PyPI publish, npm publish, public DNS, Clerk auth, Vercel-hosted Next.js dashboard, production Postgres, Fly.io deployment of the Go backend.

**Why deferred:** local-only posture during the Verdifax acqui-IP outreach window means zero acquirer-discoverable Mesedi artifacts. Every public surface gets stood up post-LOI (whichever direction the LOI lands).

**v2 plan:** unchanged — when Verdifax outreach resolves, flip the switch on the deferred public-surface work in one focused 2-3 week push.

### Framework adapters at scale (LangChain, CrewAI, Vercel AI SDK)

**v1 plan:** `mesedi.langchain.auto_instrument()`, `mesedi.crewai.auto_instrument()`, `mesedi.vercel.autoInstrument()` — drop-in instrumentation that hooks each framework's native callback/instrumentation system.

**v1 reality so far:** not yet built. Roadmap items #6-#8 in the local-only work queue. Probably 1-2 days each.

**Why deferred:** each adapter is a real coding effort that's better validated against a real customer's agent than against synthetic-org. The synthetic-org dogfood substrate is framework-free today (raw Anthropic in Python and TypeScript); validating against synthetic-org alone produces adapters that look correct but might miss real-customer integration friction.

**v2 plan:** ship each adapter against the first real pilot user who uses that framework. Pre-build skeleton stubs in v1 (we may do this in the local-only work queue) so the day-1 customer integration is fast.

---

## How to use this file

- **Adding a deferral:** when scoping a feature out of v1 explicitly (not implicitly — implicit deferrals just stay un-built), drop a section here with the same shape: what's deferred, what's the v1 substitute, why deferred, what unblocks v2 work, rough v2 scope estimate.
- **Removing a deferral:** when v2 work on an item actually starts, MOVE the section into `DEVELOPMENT_CHECKLIST.md` as an active phase / sub-slice. Don't delete from this file silently — keep the deferral history for design-decision provenance.
- **Re-evaluating:** at the start of each v2-planning session, scan this file. Items that have become blocking (real customer asking for them, real architectural problem that the v1 substitute can't cover) move to the active checklist. Items that are still "would be nice but no one is blocked" stay here.

This file is meant to be readable by acquirers / pilot users / future engineers — it answers the natural question "what's the post-v1 product direction" with specific scoped intentions, not vague promises.
