# Mesedi pilot pitch — template

Reusable cold-outreach template for finding the first real-agent pilot user. Drafted 2026-05-14.

**When to use:** any time you find a developer or operator who's running agents in production (or close to it) on X, LinkedIn, in Discord servers, in HN comments, at meetups, in cold inbound. Keep it ready so the lag between "right person found" and "message sent" is minutes, not days.

**Hard rules:**

1. The `[specific thing they posted / built / shipped]` slot is **non-negotiable**. Cold messages without evidence-of-reading get filed as spam in 1.5 seconds. Spend 5–15 minutes reading their recent output before reaching out.
2. Do **not** mention pricing, plans, roadmaps, or "demo calls." This is a one-week pilot exchange, period. The moment it sounds like a vendor pitch, the recipient bounces.
3. Sign their NDA if they have one. Offer one if they don't. Both are zero friction and remove an objection.
4. The backend runs on **your** infrastructure. Their data does not leave their network unless they explicitly point the SDK at you. Lead with this — it's the biggest objection killer.
5. Founder-to-founder honesty: don't pretend to have a company. You're a solo founder validating. People help solo founders; they don't help vendors.

---

## Template (Python-agent recipient)

> **Subject:** Free production observability for your agents — 1-week pilot, ~10 min to integrate
>
> Hi [Name],
>
> I saw [specific thing they posted / built / shipped — fill in]. The reason I'm reaching out: I've been building an observability tool for AI agents — crash detection, loop detection, cost tracking, prompt-injection alerts, hard-halt budgets — and I'm looking for one or two real agents to run it against for a week.
>
> What I'd need from you: roughly 10 minutes to add `@mesedi.wrap` to one of your agent entry points (Python or TypeScript SDK, zero external dependencies on the SDK side, fail-open so it can't break your agent), and permission to look at the aggregate failure patterns. The backend runs on my infrastructure; no data leaves your network unless you point the SDK at me. NDA available, happy to sign yours, no obligation past the week.
>
> What you get back: a written readout of every crash, loop, validator failure, and cost spike Mesedi caught during the week, with reproduction traces. If you've been running this agent for a while there's a decent chance I'll find something you didn't know was happening. If I find nothing, you've lost ten minutes and you've helped me ship a better product.
>
> I'm a solo founder, not selling anything yet, not on a roadmap call. Just trying to make sure the thing I'm building survives contact with a real agent.
>
> Worth a Zoom or async DM to talk through the shape?
>
> — Robert

---

## Variants

**TypeScript-agent recipient:** swap `@mesedi.wrap` for `wrap(async (...) => {...})` and "Python or TypeScript SDK" for "TypeScript SDK (Node 18+, ESM, zero runtime deps)".

**Framework-specific (LangChain, CrewAI, Vercel AI SDK):** add one sentence after the SDK paragraph: "I have a [framework] adapter ready, so the integration is `mesedi.[framework].auto_instrument()` rather than wrapping each tool by hand." (Only claim this if true at the moment of sending.)

**Recipient is at a known company:** add a line offering a written confidentiality letter on letterhead in addition to the NDA. Some procurement teams need that paper trail.

**Recipient runs an internal-tools agent (not customer-facing):** swap "your agent" for "your internal agent" — softens the implied stakes and makes "10 minutes to integrate" feel more believable.

---

## Follow-up cadence

- **Day 0:** initial message.
- **Day 4 (only if no reply):** one-line bump. *"Hey [Name] — quick bump on the pilot ask. Totally fine if not a fit, just want to make sure it didn't get buried."*
- **Day 14:** if still no reply, drop it. Do not send a third message. Solo-founder credibility costs more than any single pilot.

---

## What to actually deliver if they say yes

1. **Same day:** zip of the SDK (or a private git URL they can `pip install` / `npm install` from) and a one-page setup README that shows the `wrap` integration on a real example.
2. **Day 1:** confirmation that events are arriving in the backend. Send a short Loom or screenshot of their first event landing.
3. **Day 3 mid-pilot:** brief check-in — any surprises so far, anything they want surfaced differently in the readout, any SDK friction.
4. **Day 7:** the written readout — every distinct failure group, with reproduction traces, ranked by frequency × cost. Sent as a PDF or markdown doc, not a live dashboard link (a deliverable they can forward to their team without needing to log in is much more useful).
5. **Day 8:** a thank-you note with an offer to keep them on if they want, and an explicit "no obligation, you can pull the SDK any time."

If the pilot finds something genuinely surprising for them, ask permission to use the anonymized pattern as a case study in future outreach. About 1 in 3 say yes; that's how the second pilot gets easier than the first.
