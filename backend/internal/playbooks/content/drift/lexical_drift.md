# Drift, lexical drift

An execution in this project produced `llm_call` user_messages whose **lexical distribution diverged from the project's historical baseline**. Mesedi's lexical-drift detector tokenizes user_messages into character 3-grams, builds a frequency bag for the current execution, builds another bag for the project's recent history, and computes the cosine distance between them. Three severity buckets:

- `lexical_drift_0.45+`, mild. Vocabulary or phrasing has shifted but the general topic is recognizable.
- `lexical_drift_0.55+`, moderate. Different sub-topic, different style, or different upstream source feeding the prompts.
- `lexical_drift_0.70+`, severe. The prompts are in different lexical territory from the project's baseline. Real change, not noise.

The detector catches **distributional** drift, the prompts changed continuously over time, whereas the sibling `new_model` detector catches **categorical** drift (the model identifier flipped). Both fire under the same `drift` failure class because they share the same root pattern: something upstream of the LLM changed without coordination, and now agent behavior is shifting.

The floor was raised from 0.30 to 0.45 after empirical observation against dogfood traffic, 0.30 fires on routine same-domain variation, which is noise; 0.45 keeps the meaningful signal and cuts false positives ~80%.

## What this catches

Three concrete failure modes:

1. **Upstream content rewrite.** A customer's knowledge base or document store got auto-rewritten, by a content team's editorial pass, by an LLM-driven cleanup job, by a migration script. Their RAG retriever now synthesizes subtly different prompts. The model is the same, the agent code is the same, but the prompts it sees are statistically different and the outputs have shifted.

2. **A/B test in upstream prompt engineering.** A prompt-engineering team is testing new prompt templates and shipped variant B to 50% of traffic. The agent observability team didn't get the memo. Lexical drift surfaces the change as "the population's prompt distribution shifted" without requiring anyone to instrument the test.

3. **Tool-return drift.** Your tools' outputs feed into the next LLM call's user_message. If a tool's output format changed, schema update, content source change, formatting tweak, the downstream user_messages will look different in aggregate.

## What this does NOT catch

Semantic drift that preserves lexical similarity. If your prompts shift from "what does this policy require" to "what is the policy's requirement," the two sentences have nearly-identical 3-gram bags, high cosine similarity, no drift detected. Semantic-but-lexically-similar drift requires embedding-based detection, which is on the v2 roadmap once Mesedi has an embeddings substrate.

The detector also doesn't tell you WHAT changed lexically, only that the distribution diverged. The diagnostic step is on you (and that's the next paragraph).

## How to investigate

Open the affected execution. Three diagnostics:

1. **Sample 3-5 user_messages from this execution and 3-5 from a historical execution in the same project.** Read them side by side. The lexical shift will usually be obvious to a human in 60 seconds even when it took a 3-gram cosine to detect numerically. Vocabulary shifted, style shifted, domain shifted, length shifted, one of those will jump out.

2. **Identify the upstream source.** Where do these user_messages come from? Direct user input, tool returns, RAG retrieval, agent-internal planning, a templating layer? The lexical shift's origin point is upstream of whichever of these you find. Trace from "the message that fired the detector" backward to whatever produced it.

3. **Check the affected-executions timestamp.** A drift signal that starts at a specific timestamp and persists is a real change, something deployed or shifted at that moment. A drift signal that fires sporadically across days is more likely a sampling artifact at the threshold edge and the floor needs tuning.

## How to fix

Lexical drift is unusual among Mesedi failure classes in that the "fix" is often not in your code at all, it's a coordination signal that something upstream changed:

- **If the drift is intentional, acknowledge it.** Your knowledge base got rewritten, your prompt template was deliberately updated, your tool's output schema changed. Add a note to your team or dismiss the failure-group as a known change. Mesedi will eventually re-baseline as the new distribution becomes the historical norm.

- **If the drift is unintentional, find the upstream change.** This is the more interesting case. The detector is telling you "something in your prompt pipeline changed without you noticing." That something is worth finding regardless of whether the agent output looks fine downstream, the cause that produced this drift may produce a bigger drift next time.

- **For RAG-heavy pipelines, instrument the retrieval step separately.** If your prompts come from a retrieval system, the retrieval output is where most lexical drift originates. Adding retrieval-side observability (separate from agent-side) gives you a leading indicator before the drift shows up here.

- **Don't tune the threshold downward to chase signal.** The 0.45 floor was empirically calibrated against real traffic. Lowering it generates more false positives faster than it generates true positives. If you want more sensitivity, the right move is to add a complementary semantic-drift detector (embedding-based) once that capability ships, not to make the lexical one twitchier.

## Why this detector exists separately from new-model

Both detectors are in the `drift` class because they share the same operational pattern, something upstream changed without coordination. But they're separated because the diagnostic and remediation are completely different:

- **new_model** is a binary signal that points at a specific model identifier and a specific configuration change. Fix is fast.
- **lexical_drift** is a continuous signal that points at "prompts shifted in general" and the cause might be anywhere in the prompt-construction pipeline. Fix requires investigation.

When you see both fire on the same project in the same window, look at the model change first, sometimes the new model is what's producing the lexically-different prompts (especially if the model is doing some of the prompt construction).

## Auto-fix in a future Mesedi release

The v2 roadmap includes semantic-drift detection via embedding similarity, which catches the lexically-similar-but-semantically-different cases this detector misses. Both signals would compose: lexical drift catches the obvious cases cheaply, semantic drift catches the subtle cases at higher compute cost. The Tier 2 layer also includes "show me which N-grams shifted between the current and historical bags" as a structured diagnostic surface, replacing the manual side-by-side read in the investigation section above.
