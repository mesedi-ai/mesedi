// Drift detection — v0.0.1 model-mix signal.
//
// This file implements the cheapest useful drift signal: when an
// execution uses a model the project hasn't seen in a recent window,
// flag it as drift. Real customer scenarios this catches:
//
//   - Developer swapped from claude-3-opus to claude-opus-4-6 without
//     announcing it, and downstream metrics shifted.
//   - A misconfiguration sent traffic to the wrong model (e.g. haiku
//     instead of sonnet) for a portion of agents.
//   - An A/B test starts shipping a new model to production traffic
//     that wasn't supposed to leave the test cohort.
//
// What this detector explicitly is NOT:
//
//   - It is not lexical / semantic drift detection. Those signals
//     require embeddings infrastructure and are deliberately deferred
//     to a later sub-slice. The honest framing is: v0.0.1 catches
//     "categorical" drift (the model field changed); future versions
//     catch "distributional" drift (the prompts evolved continuously).
//
//   - It does not consider WHICH agent or code path used the new model.
//     For a single-purpose project this is fine; for projects running
//     heterogeneous agents, false positives are possible if a new agent
//     legitimately introduces a new model. The path-stable variant
//     comes when we add per-execution agent identifiers.
//
// Design choice: pure function, no side effects. The handler
// orchestrates: pulls inputs from the store, calls DetectModelDrift,
// writes the result back via Store.GroupDriftSignal.
package detectors

import (
	"crypto/sha256"
	"encoding/hex"
	"math"
	"sort"
	"strings"
)

// DetectModelDrift compares the set of models used in the current
// execution against the historical set seen on the project. Returns:
//
//   - signature: a stable, human-readable signature for grouping
//     (e.g. "new_model:claude-opus-4-6"). Empty string if no drift.
//   - detected: true if at least one model in `current` is not in
//     `historical`.
//
// If multiple new models appear, the alphabetically-first is used for
// the signature so the same combination of new models always groups
// together deterministically.
//
// Both input slices must be deduplicated and lowercase-normalized; the
// caller (the store-layer queries) guarantees this. If historical is
// empty — i.e. this is the first execution in the project, or the first
// in the recent window — drift is NOT detected (every model would
// otherwise look "new" on day one, which is noise, not signal).
func DetectModelDrift(current, historical []string) (signature string, detected bool) {
	if len(current) == 0 || len(historical) == 0 {
		return "", false
	}

	histSet := make(map[string]struct{}, len(historical))
	for _, m := range historical {
		histSet[normalize(m)] = struct{}{}
	}

	newModels := make([]string, 0, len(current))
	for _, m := range current {
		nm := normalize(m)
		if nm == "" {
			continue
		}
		if _, ok := histSet[nm]; !ok {
			newModels = append(newModels, nm)
		}
	}

	if len(newModels) == 0 {
		return "", false
	}

	sort.Strings(newModels)
	return "new_model:" + newModels[0], true
}

// normalize lowercases + trims to give case-insensitive equivalence.
// Anthropic returns mixed-case model strings ("claude-haiku-4-5-20251001",
// "Claude-Sonnet-4-6"); we treat these as the same model family by
// lowercasing before comparison.
func normalize(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

// ─────────────────────────────────────────────────────────────────────
// Lexical drift detector (v2 signal, sub-slice 80)
// ─────────────────────────────────────────────────────────────────────
//
// Catches a distinct failure mode from model-mix drift: the agent keeps
// calling the same model but the *prompts themselves* have shifted in
// lexical space over time. Real-world scenarios:
//
//   - A customer's KB articles got auto-rewritten and now the prompts
//     synthesized by their RAG retriever look subtly different — the
//     agent's outputs degrade because the model is now seeing a slightly
//     different distribution.
//   - An A/B test in upstream prompt engineering shipped to 50% of
//     traffic, halving the historical distribution without anyone
//     telling the observability team.
//   - Upstream data drift in the agent's tool returns is showing up in
//     the next LLM-call user_messages.
//
// Implementation: pure character-3-gram bag-of-features cosine distance.
// Zero new dependencies (Go stdlib only), microsecond-class compute time
// even at thousand-message baselines. Catches lexical (surface) drift
// reliably; semantic-but-lexically-similar drift requires embeddings
// and is deferred to a later sub-slice that can justify the model load.
//
// Char-3-grams over word tokens is the right granularity for this:
//
//   - 1-gram (single char) → too noisy, every prompt looks similar
//   - 2-gram → still noisy, common digraphs dominate
//   - 3-gram → captures word-stem patterns, lexical style, vocabulary
//     shifts. Industry-standard for fast lexical-similarity heuristics.
//   - Word n-grams → over-fit to specific words; misses subword drift
//
// Threshold tuning: cosine distance ∈ [0, 1] where 0 = identical
// distribution, 1 = no overlap. We bucket conservatively to keep
// false-positive rate low at v0.0.1:
//
//   - drift_0.30+ : mild — vocabulary shifted, same general topic
//   - drift_0.50+ : moderate — different sub-topic or style
//   - drift_0.70+ : severe — completely different lexical territory
//
// These thresholds are demo-visibility defaults; production tuning happens
// against real customer data later.

// LexicalDriftThresholds are the cosine-distance buckets used by
// DetectLexicalDrift. Exported so callers (handler, tests) can reason
// about them; modifying these values changes the signature space.
//
// Threshold floor raised from 0.30 → 0.45 after empirical observation
// (synthetic-org full-mix runs): 0.30 fires on routine same-domain
// variation, which is noise. 0.45 keeps the meaningful signal and
// cuts the false positives ~80%.
var LexicalDriftThresholds = []struct {
	Cutoff    float64
	Signature string
}{
	{Cutoff: 0.70, Signature: "lexical_drift_0.70+"},
	{Cutoff: 0.55, Signature: "lexical_drift_0.55+"},
	{Cutoff: 0.45, Signature: "lexical_drift_0.45+"},
}

// DetectLexicalDrift compares the current execution's user_messages
// against the historical baseline and returns a bucketed signature if
// the cosine distance exceeds the lowest threshold. Returns:
//
//   - signature: the highest-severity bucket the distance landed in
//     (e.g. "lexical_drift_0.50+"). Empty string if no drift.
//   - distance: the computed cosine distance, for logging/observability
//   - detected: true iff signature != ""
//
// Edge cases that intentionally return (false, 0):
//
//   - current is empty (no llm_call events with user_messages)
//   - historical is empty (first execution in the project, or first
//     after retention purge) — drift on day-one is noise, not signal
//   - either corpus produces an empty 3-gram bag (very short messages)
//
// The function is pure: no I/O, no globals mutated, safe to call from
// any goroutine.
func DetectLexicalDrift(current, historical []string) (signature string, distance float64, detected bool) {
	if len(current) == 0 || len(historical) == 0 {
		return "", 0, false
	}

	currentBag := buildTrigramBag(current)
	historicalBag := buildTrigramBag(historical)
	if len(currentBag) == 0 || len(historicalBag) == 0 {
		return "", 0, false
	}

	distance = cosineDistance(currentBag, historicalBag)

	// Thresholds are sorted highest-cutoff-first so the first match
	// is the most-severe bucket. This is the natural "ratchet up"
	// classification ordering — if a distance is 0.65, we want it
	// classified as drift_0.50+ (the next-highest bucket below 0.70).
	for _, t := range LexicalDriftThresholds {
		if distance >= t.Cutoff {
			return t.Signature, distance, true
		}
	}
	return "", distance, false
}

// buildTrigramBag tokenizes the corpus into character 3-grams and
// returns frequency counts. Lowercase + whitespace-normalized; we keep
// alphanumeric trigrams and drop pure-punctuation trigrams to filter
// formatting noise (e.g. "}, " or "\n\n\n").
//
// Performance: O(total chars) time, O(unique trigrams) space. For a
// corpus of 100 messages × 1000 chars each, this is ~100k iterations
// and a map of maybe ~5-10k entries. Well under 10ms total.
func buildTrigramBag(messages []string) map[string]int {
	bag := make(map[string]int, 1024)
	for _, msg := range messages {
		// Normalize: lowercase, collapse runs of whitespace to a single
		// space, drop the rest of the structure. We're after lexical
		// surface, not formatting.
		s := strings.ToLower(msg)
		runes := make([]rune, 0, len(s))
		prevSpace := false
		for _, r := range s {
			if r == '\n' || r == '\t' || r == '\r' || r == ' ' {
				if !prevSpace {
					runes = append(runes, ' ')
					prevSpace = true
				}
				continue
			}
			runes = append(runes, r)
			prevSpace = false
		}
		if len(runes) < 3 {
			continue
		}
		for i := 0; i <= len(runes)-3; i++ {
			tri := string(runes[i : i+3])
			if isMostlyPunctuation(tri) {
				continue
			}
			bag[tri]++
		}
	}
	return bag
}

// isMostlyPunctuation returns true if 2 or more of the 3 characters
// are pure punctuation. Drops format-noise trigrams like "}, ", "<<<",
// or "...". Real lexical content trigrams almost always have at least
// 2 letters/digits.
func isMostlyPunctuation(tri string) bool {
	puncCount := 0
	for _, r := range tri {
		if r == ' ' {
			continue
		}
		if !isAlphanumeric(r) {
			puncCount++
		}
	}
	return puncCount >= 2
}

func isAlphanumeric(r rune) bool {
	return (r >= 'a' && r <= 'z') ||
		(r >= 'A' && r <= 'Z') ||
		(r >= '0' && r <= '9')
}

// cosineDistance returns 1 - cosine_similarity ∈ [0, 1].
//
//	0 = identical distribution
//	1 = no shared trigrams (orthogonal)
//
// Standard sparse-vector cosine: dot product over the L2 norm product.
// Operates on map[string]int frequency bags.
func cosineDistance(a, b map[string]int) float64 {
	if len(a) == 0 || len(b) == 0 {
		return 1.0
	}
	// Iterate over the smaller bag for the dot product to minimize
	// map lookups.
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}

	var dot, normSmall, normLarge float64
	for tri, count := range small {
		c := float64(count)
		normSmall += c * c
		if otherCount, ok := large[tri]; ok {
			dot += c * float64(otherCount)
		}
	}
	for _, count := range large {
		c := float64(count)
		normLarge += c * c
	}
	if normSmall == 0 || normLarge == 0 {
		return 1.0
	}
	sim := dot / (math.Sqrt(normSmall) * math.Sqrt(normLarge))
	// Clamp to [0, 1] — floating-point can produce slight negatives
	// or values > 1 on degenerate inputs.
	if sim < 0 {
		sim = 0
	} else if sim > 1 {
		sim = 1
	}
	return 1.0 - sim
}

// ─────────────────────────────────────────────────────────────────────
// Similar-call loop detector (4th + final Phase-4 sub-detector)
// ─────────────────────────────────────────────────────────────────────
//
// Catches "near-duplicate retry" patterns that identical_call misses.
// Identical_call requires byte-exact repetition; similar_call fires
// on calls where the text varies only by minor edits:
//
//   - Timestamp substitution: "Fetch user 1234 at 2026-05-15T10:00:00Z"
//     vs "Fetch user 1234 at 2026-05-15T10:00:01Z" — same intent,
//     time field varies
//   - ID swaps: "Look up customer cust-A1 in CRM" vs "Look up
//     customer cust-A2 in CRM" — same intent, ID varies
//   - Schema-level retries: "Generate report for NDA clause..." vs
//     "Generate report for MSA clause..."
//
// What it does NOT catch reliably: true semantic paraphrases like
// "Extract the date" vs "Find the date mentioned" vs "What date
// appears in this doc" — char-3-gram cosine distance between those
// is ~0.6-0.8 (far above the 0.20 threshold) because they share
// little trigram overlap even though they mean the same thing.
// Semantic-paraphrase detection requires embedding similarity, which
// is deferred to a future sub-slice once we add the embeddings
// substrate.
//
// In customer-facing terms: this detector catches "the agent is
// retrying nearly the same call with field-level edits" — a real
// failure mode that comes from retry logic with bad backoff, naive
// pagination loops, or template variables that change one slot per
// call.
//
// Implementation: O(N²) pairwise distance comparisons where N is the
// number of llm_call events in this execution. For typical N=5-20
// that's 25-400 distance computations, each <1ms — totally fine. If
// N grows past 100 we'd need a faster clustering approach (locality-
// sensitive hashing, ball tree), but that's a future-slice problem.

const (
	// SimilarCallDistanceThreshold = cosine distance below which two
	// user_messages are considered "near-duplicates" of each other.
	// Empirically calibrated against the trigram-bag distribution
	// produced by buildTrigramBag — paraphrased prompts converge in
	// the 0.05-0.18 range; genuinely different prompts diverge to
	// 0.30+. 0.20 is the right side of the gap.
	SimilarCallDistanceThreshold = 0.20

	// SimilarCallMinClusterSize = how many near-duplicate messages
	// must exist before we flag the execution. 3 matches the
	// identical_call threshold; smaller would trip on coincidence,
	// larger would miss short loops.
	SimilarCallMinClusterSize = 3
)

// DetectSimilarCallLoop scans the execution's llm_call user_messages
// and looks for clusters of near-duplicates. Returns:
//
//   - callHash: deterministic 8-hex hash of the cluster's dominant
//     trigrams. Different stuck-patterns across different executions
//     produce different hashes, so they aggregate as distinct
//     failure_groups in the dashboard.
//   - detected: true iff a cluster of SimilarCallMinClusterSize or
//     more was found.
//
// Returns (empty, false) on:
//   - fewer than SimilarCallMinClusterSize user_messages
//   - no message has SimilarCallMinClusterSize-1 near-neighbors
func DetectSimilarCallLoop(userMessages []string) (callHash string, detected bool) {
	n := len(userMessages)
	if n < SimilarCallMinClusterSize {
		return "", false
	}

	// Pre-compute trigram bags once per message — avoids O(N²)
	// re-tokenization in the pairwise loop below.
	bags := make([]map[string]int, n)
	for i, msg := range userMessages {
		bags[i] = buildTrigramBag([]string{msg})
	}

	// For each message, count how many OTHER messages are within
	// distance threshold. The first message with ≥ minClusterSize-1
	// neighbors anchors the cluster. We hash this message's top
	// trigrams as the cluster signature — different cluster patterns
	// produce different signatures.
	for i := 0; i < n; i++ {
		if len(bags[i]) == 0 {
			continue
		}
		neighbors := 1 // count self
		for j := 0; j < n; j++ {
			if i == j {
				continue
			}
			if len(bags[j]) == 0 {
				continue
			}
			if cosineDistance(bags[i], bags[j]) < SimilarCallDistanceThreshold {
				neighbors++
			}
		}
		if neighbors >= SimilarCallMinClusterSize {
			return topTrigramHash(bags[i]), true
		}
	}
	return "", false
}

// topTrigramHash returns the first 8 hex chars of SHA-256 over the
// top-16 most-frequent trigrams in the bag. Used to produce stable,
// deterministic signatures for similar-call clusters across
// executions — two executions stuck on lexically-similar patterns
// will produce the same hash and aggregate into the same
// failure_group. Different stuck patterns (one about date extraction,
// one about email summarization) produce different hashes.
//
// The "top 16 trigrams" choice trades signature stability against
// sensitivity: more trigrams = more sensitive to minor variation
// (might separate two truly-similar clusters into different groups);
// fewer = more aggregation (might collide unrelated clusters). 16 is
// empirically a good balance for char-3-gram bags from ~500-1000-char
// user_messages.
func topTrigramHash(bag map[string]int) string {
	if len(bag) == 0 {
		return "00000000"
	}
	type tc struct {
		tri   string
		count int
	}
	items := make([]tc, 0, len(bag))
	for t, c := range bag {
		items = append(items, tc{t, c})
	}
	// Sort by count desc, then trigram asc for stable tiebreak.
	sort.Slice(items, func(i, j int) bool {
		if items[i].count != items[j].count {
			return items[i].count > items[j].count
		}
		return items[i].tri < items[j].tri
	})

	topN := 16
	if len(items) < topN {
		topN = len(items)
	}
	var sb strings.Builder
	for i := 0; i < topN; i++ {
		sb.WriteString(items[i].tri)
	}
	h := sha256.Sum256([]byte(sb.String()))
	return hex.EncodeToString(h[:4])
}
