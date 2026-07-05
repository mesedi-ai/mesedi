// Token-waste detector.
//
// Catches the production case from the marketing page: a $500/month
// POC becomes $847,000/month once it ships, because the agent
// re-sends the entire conversation history on every retry. By step
// 20 the customer is paying for the same system prompt 20 times.
//
// Three-layer defense-in-depth so the detector catches the broader
// near-duplicate class instead of just the textbook
// character-identical case:
//
//  1. stripVariablePrefixes normalizes each user_message by stripping
//     leading patterns that change per turn but don't affect the
//     wasteful body: ISO-8601 / RFC3339 timestamps, leading numeric
//     counters or line numbers, hex UUIDs (8-4-4-4-12 and naked
//     32-hex), and request-id-style prefixes (`req_*`, `id:`).
//
//  2. Existing 2048-char SHA-256 exact-prefix hash runs on the
//     NORMALIZED text. Same `token_waste:<hex8>` signature shape as
//     before; existing customer dashboards keep clustering. A
//     timestamp-prefixed conversation that previously hashed to a
//     fresh value every turn now hashes consistently and clusters
//     correctly.
//
//  3. Shingle-Jaccard fallback runs ONLY when the exact-hash path
//     finds no match. Builds k=8 char-shingles per (normalized)
//     payload, computes pairwise Jaccard, and fires when 3+ payloads
//     share Jaccard >= 0.85 with each other. Catches the structurally
//     similar but lexically distinct cases that the leading-prefix
//     strip can't reach (mid-prefix variable material, conversation
//     histories that drift inside the 2048-char window). Fires under
//     a NEW signature `token_waste:near_dup:<hex8>` so it doesn't
//     pollute the canonical `token_waste:<hex8>` clusters customers
//     have built dashboards against.
//
// Threshold is the same value used by the semantic_loop detector
// (minRepeats = 3). Three is the established "agent is in a loop,
// not just retrying once" boundary across the loops family.
package detectors

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
)

const (
	// prefixWindowChars is how many leading characters of each
	// (normalized) user_message to hash. Big enough that distinct
	// prompts produce distinct hashes (avoiding false positives on
	// agents that legitimately share a short system header), small
	// enough that the accumulation pattern (same prefix, growing
	// suffix) still matches.
	prefixWindowChars = 2048
	// minRepeats is the threshold at which a recurring prefix flips
	// from "noise" to "wasteful accumulation." Same value as the
	// semantic_loop detector for consistency across the loops
	// family.
	minRepeats = 3
	// shingleSize is the k for k-shingle character ngrams used by
	// the near-duplicate fallback. k=8 is the established choice in
	// plagiarism-detection literature — small enough to catch
	// structural similarity, large enough to keep the shingle set
	// manageable and unique-ish.
	shingleSize = 8
	// jaccardThreshold is the minimum pairwise Jaccard similarity
	// for two payloads to count as near-duplicates. 0.85 is high
	// enough to reject legitimately distinct prompts that share
	// boilerplate vocabulary, low enough to catch UUID-prefixed /
	// timestamp-mid-drift / conversation-history-drift cases.
	jaccardThreshold = 0.85
)

// variablePrefixPatterns is the ordered list of normalization
// regexes applied to the LEADING edge of each user_message before
// hashing. Order matters: longer / more-specific shapes come first
// so a string like "2026-06-23T14:23:00Z req_abc123" gets both the
// timestamp AND the request-id stripped, not just the first match.
//
// All patterns are anchored to the start (`^`) so they only strip
// LEADING variable material — they will not chew arbitrary
// substrings out of the middle of a prompt. The strip is
// applied repeatedly until no pattern matches, so multi-layer
// leading material (timestamp + counter + UUID, on three lines)
// gets fully stripped.
var variablePrefixPatterns = []*regexp.Regexp{
	// ISO-8601 / RFC3339 timestamps with optional fractional seconds
	// and timezone, optionally followed by trailing whitespace /
	// punctuation: "2026-06-23T14:23:00Z", "2026-06-23 14:23:00",
	// "2026-06-23T14:23:00.123456+00:00".
	regexp.MustCompile(
		`^\s*\d{4}-\d{2}-\d{2}[T ]\d{2}:\d{2}:\d{2}(?:\.\d+)?` +
			`(?:Z|[+-]\d{2}:?\d{2})?[\s\-:,\]\}\)]*`,
	),
	// Hex UUID: 8-4-4-4-12, case-insensitive.
	regexp.MustCompile(
		`^\s*[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-` +
			`[0-9a-fA-F]{4}-[0-9a-fA-F]{12}[\s\-:,\]\}\)]*`,
	),
	// Naked 32-hex (md5-style request IDs, doc fingerprints).
	regexp.MustCompile(`^\s*[0-9a-fA-F]{32}[\s\-:,\]\}\)]*`),
	// req_*, id:*, request_id=* style prefixes — strip the key + a
	// short identifier blob (up to 64 chars of word / hex / dash).
	regexp.MustCompile(
		`(?i)^\s*(?:req(?:uest)?[-_]?id|request|id|trace[-_]?id|correlation[-_]?id)` +
			`\s*[=:]\s*[\w\-]{1,64}[\s\-:,\]\}\)]*`,
	),
	// Leading numeric counter / line number / "Turn 12:" / "[42]" /
	// "(123)" / "". Anchored leading numeric run, optional
	// surrounding punctuation, optional short label like "Turn"
	// before the number.
	regexp.MustCompile(
		`(?i)^\s*[\[\(#]?(?:turn|step|iter(?:ation)?|message|msg|line|seq)?` +
			`\s*[\[\(#]?\s*\d{1,9}[\]\)\.\s:,\-]*`,
	),
}

// stripVariablePrefixes applies the variable-prefix regexes to the
// LEADING edge of the input repeatedly until no pattern matches.
// Returns the normalized string. A returned string equal to the
// input means no variable prefix was found.
//
// Repeated application handles the multi-layer case: a prompt that
// starts with "[2026-06-23T14:23:00Z] req_abc123 Turn 12: ..." needs
// three sequential strip rounds to reach the wasteful body.
//
// The loop has a hard cap of 8 iterations to prevent any pathological
// regex from spinning — even on the worst real input, all four
// prefix shapes won't stack more than 5-6 deep.
func stripVariablePrefixes(s string) string {
	const maxIterations = 8
	for i := 0; i < maxIterations; i++ {
		matched := false
		for _, re := range variablePrefixPatterns {
			loc := re.FindStringIndex(s)
			if loc == nil || loc[0] != 0 || loc[1] == 0 {
				continue
			}
			s = s[loc[1]:]
			matched = true
			break
		}
		if !matched {
			return s
		}
	}
	return s
}

// kShingles returns the set of k-character substrings of s. Returns
// an empty set when len(s) < k. Used by the near-duplicate fallback;
// representation is a map[string]struct{} so set union / intersection
// stay O(|A| + |B|).
func kShingles(s string, k int) map[string]struct{} {
	out := map[string]struct{}{}
	if len(s) < k {
		return out
	}
	// Window slides one byte at a time. We operate on bytes rather
	// than runes because user_message is dominated by ASCII for the
	// shapes this detector catches, and byte shingles are 4-8x
	// cheaper than rune shingles.
	for i := 0; i+k <= len(s); i++ {
		out[s[i:i+k]] = struct{}{}
	}
	return out
}

// jaccardSimilarity returns |A ∩ B| / |A ∪ B| for two shingle sets.
// Returns 0.0 when both sets are empty.
func jaccardSimilarity(a, b map[string]struct{}) float64 {
	if len(a) == 0 && len(b) == 0 {
		return 0
	}
	// Iterate the smaller set against the larger for the
	// intersection — keeps the inner loop short on lopsided pairs.
	small, large := a, b
	if len(b) < len(a) {
		small, large = b, a
	}
	inter := 0
	for s := range small {
		if _, ok := large[s]; ok {
			inter++
		}
	}
	union := len(a) + len(b) - inter
	if union == 0 {
		return 0
	}
	return float64(inter) / float64(union)
}

// detectNearDuplicateCluster scans the normalized prefixes for a
// group of 3+ entries that share pairwise Jaccard >= threshold.
// Returns (signature, true) on detection. Returns ("", false) when
// no cluster meets the threshold.
//
// The signature is SHA-256 of the canonical-sorted intersection of
// the cluster members' shingle sets, truncated to 8 hex chars. This
// makes the signature deterministic across re-runs: the same
// near-duplicate group always produces the same `near_dup:<hex8>`
// suffix regardless of map iteration order.
//
// Algorithm: greedy seed-based clustering. Pick a seed payload, find
// all payloads with Jaccard >= threshold against the seed; if the
// resulting group is >= minRepeats, return it. Try each payload as
// seed; pick the smallest seed-index that produces a valid cluster
// (deterministic tie-break). Stop at the first matching cluster.
//
// Quadratic in payload count but bounded by the typical N=10-30
// llm_calls per execution and the gating that this only runs when
// the exact-hash path found no match.
func detectNearDuplicateCluster(
	normalized []string,
	shingles []map[string]struct{},
	threshold int,
) (signature string, detected bool) {
	if len(normalized) < threshold {
		return "", false
	}
	for seed := 0; seed < len(normalized); seed++ {
		group := []int{seed}
		for j := 0; j < len(normalized); j++ {
			if j == seed {
				continue
			}
			if jaccardSimilarity(shingles[seed], shingles[j]) >= jaccardThreshold {
				group = append(group, j)
			}
		}
		if len(group) < threshold {
			continue
		}
		// Build the deterministic signature from the intersection
		// of all group members' shingle sets. Empty intersection
		// means we got here through transitive similarity rather
		// than a true shared core — keep walking.
		intersection := map[string]struct{}{}
		for s := range shingles[group[0]] {
			intersection[s] = struct{}{}
		}
		for _, idx := range group[1:] {
			for s := range intersection {
				if _, ok := shingles[idx][s]; !ok {
					delete(intersection, s)
				}
			}
		}
		if len(intersection) == 0 {
			continue
		}
		keys := make([]string, 0, len(intersection))
		for s := range intersection {
			keys = append(keys, s)
		}
		sort.Strings(keys)
		sum := sha256.Sum256([]byte(strings.Join(keys, "\x00")))
		suffix := hex.EncodeToString(sum[:])[:8]
		return fmt.Sprintf("token_waste:near_dup:%s", suffix), true
	}
	return "", false
}

// TokenWasteThresholds carries the per-project tunable values for
// this detector. PrefixWindowChars + MinRepeats default
// to prefixWindowChars (2048) + minRepeats (3) for customers who
// don't tune. PrefixWindowChars is the one tier-capped knob
// (real CPU vector — more chars hashed + bigger shingle set).
type TokenWasteThresholds struct {
	PrefixWindowChars int
	MinRepeats        int
}

// DefaultTokenWasteThresholds returns the hardcoded historical
// defaults. Used by legacy call sites and tests.
func DefaultTokenWasteThresholds() TokenWasteThresholds {
	return TokenWasteThresholds{
		PrefixWindowChars: prefixWindowChars,
		MinRepeats:        minRepeats,
	}
}

// DetectTokenWaste scans the supplied llm_call payloads in sequence
// order and reports either:
//
//   - "token_waste:<hex8>" — the canonical exact-prefix signature
//     when a normalized prefix hash repeats minRepeats+ times.
//   - "token_waste:near_dup:<hex8>" — the near-duplicate signature
//     when the exact path didn't fire but shingle-Jaccard found a
//     3+ cluster at >= 0.85 similarity.
//
// `payloads` is the ordered list of llm_call event payloads. The
// detector reads the `user_message` field on each; payloads without
// a usable user_message (system_prompt-only calls, malformed
// payloads) are skipped.
//
// Returns ("", false) when fewer than minRepeats payloads exist or
// when no signature crosses the threshold on either path.
//
// Preserved verbatim for backward compatibility with existing unit
// tests + non-handler call sites. The production execution-close
// path uses DetectTokenWasteWithThresholds.
func DetectTokenWaste(payloads []json.RawMessage) (signature string, detected bool) {
	return DetectTokenWasteWithThresholds(payloads, DefaultTokenWasteThresholds())
}

// DetectTokenWasteWithThresholds is the per-project-aware variant.
// Defensive: PrefixWindowChars < 64 OR MinRepeats < 2 fall back to
// defaults (validators registry rejects these at write time).
func DetectTokenWasteWithThresholds(
	payloads []json.RawMessage,
	t TokenWasteThresholds,
) (signature string, detected bool) {
	window := t.PrefixWindowChars
	if window < 64 {
		window = prefixWindowChars
	}
	threshold := t.MinRepeats
	if threshold < 2 {
		threshold = minRepeats
	}
	if len(payloads) < threshold {
		return "", false
	}
	// Extract + normalize each user_message once; reused by both the
	// exact-hash pass and the shingle fallback.
	normalized := make([]string, 0, len(payloads))
	for _, raw := range payloads {
		// Field name is `user_message` to match what the SDK's
		// instrument_anthropic ships (the LAST user-role message in
		// the conversation). The detector originally read
		// `user_prompt` which no SDK ever emitted, so the field was
		// always empty and token_waste silently no-op'd on every
		// real customer execution. Caught by the integration suite
		// (backend/test/integration/test_detectors.py::test_token_waste).
		var p struct {
			UserMessage string `json:"user_message"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		if p.UserMessage == "" {
			continue
		}
		text := stripVariablePrefixes(p.UserMessage)
		if len(text) > window {
			text = text[:window]
		}
		normalized = append(normalized, text)
	}
	if len(normalized) < threshold {
		return "", false
	}
	// Layer 1+2: exact-prefix SHA-256 on the normalized text.
	counts := map[string]int{}
	for _, text := range normalized {
		sum := sha256.Sum256([]byte(text))
		counts[hex.EncodeToString(sum[:])]++
	}
	// Find the highest-count hash; tie-break by lexicographic hash
	// for deterministic clustering.
	bestHash := ""
	bestCount := 0
	for h, c := range counts {
		if c < threshold {
			continue
		}
		if c > bestCount || (c == bestCount && h < bestHash) {
			bestHash = h
			bestCount = c
		}
	}
	if bestCount >= threshold {
		suffix := bestHash
		if len(suffix) > 8 {
			suffix = suffix[:8]
		}
		return fmt.Sprintf("token_waste:%s", suffix), true
	}
	// Layer 3: shingle-Jaccard fallback. Only runs when the exact
	// path found nothing — avoids dual-firing and saves CPU on the
	// common case.
	shingles := make([]map[string]struct{}, len(normalized))
	for i, text := range normalized {
		shingles[i] = kShingles(text, shingleSize)
	}
	return detectNearDuplicateCluster(normalized, shingles, threshold)
}
