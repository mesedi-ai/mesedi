// Token-waste detector (Mesedi #4).
//
// Catches the production case from the marketing page: a $500/month
// POC becomes $847,000/month once it ships, because the agent
// re-sends the entire conversation history on every retry. By step
// 20 the customer is paying for the same system prompt 20 times.
//
// The signal: identical or near-identical leading prefix across N
// consecutive llm_call.user_prompt fields. When the same prefix
// recurs minRepeats+ times in an execution, fire. The detector ships
// the cluster signature as "token_waste:<hex8>" using the SHA-256
// of the repeating prefix; same prefix on different runs collapses
// into the same group so SREs see "X executions waste tokens this
// way" once, not once per run.
//
// Implementation notes:
//
//  1. We hash the LEADING prefixWindowChars characters, not the
//     entire prompt. The accumulation pattern has the same prefix
//     and a growing suffix; truncating means turn-3 and turn-12
//     prompts hash identically as long as their leading material
//     matches.
//
//  2. The threshold is the same value used by the semantic_loop
//     detector (minRepeats = 3). Three is the established "agent
//     is in a loop, not just retrying once" boundary across the
//     loops family.
//
//  3. We use the raw prompt bytes, not a canonical-state
//     normalization. The marketing-case pattern is
//     character-identical; if a customer's accumulation pattern
//     differs in trivial whitespace they'd already be caught by
//     semantic_loop on checkpoints. This detector covers the
//     llm_call-level surface.
package detectors

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
)

const (
	// prefixWindowChars is how many leading characters of each
	// user_prompt to hash. Big enough that distinct prompts produce
	// distinct hashes (avoiding false positives on agents that
	// legitimately share a short system header), small enough that
	// the accumulation pattern (same prefix, growing suffix) still
	// matches.
	prefixWindowChars = 2048
	// minRepeats is the threshold at which a recurring prefix flips
	// from "noise" to "wasteful accumulation." Same value as the
	// semantic_loop detector for consistency across the loops
	// family.
	minRepeats = 3
)

// DetectTokenWaste scans the supplied llm_call payloads in sequence
// order and reports the most-repeated prompt-prefix hash if it
// crossed the threshold. Returns (signature, true) on detection.
//
// `payloads` is the ordered list of llm_call event payloads. The
// detector reads the `user_prompt` field on each; payloads without
// a usable user_prompt (system_prompt-only calls, malformed
// payloads) are skipped.
//
// Returns ("", false) when fewer than minRepeats payloads exist or
// when no single prefix repeats minRepeats+ times.
func DetectTokenWaste(payloads []json.RawMessage) (signature string, detected bool) {
	if len(payloads) < minRepeats {
		return "", false
	}
	counts := map[string]int{}
	for _, raw := range payloads {
		var p struct {
			UserPrompt string `json:"user_prompt"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		if p.UserPrompt == "" {
			continue
		}
		prefix := p.UserPrompt
		if len(prefix) > prefixWindowChars {
			prefix = prefix[:prefixWindowChars]
		}
		sum := sha256.Sum256([]byte(prefix))
		counts[hex.EncodeToString(sum[:])]++
	}
	// Find the highest-count hash; tie-break by lexicographic hash
	// for deterministic clustering.
	bestHash := ""
	bestCount := 0
	for h, c := range counts {
		if c < minRepeats {
			continue
		}
		if c > bestCount || (c == bestCount && h < bestHash) {
			bestHash = h
			bestCount = c
		}
	}
	if bestCount == 0 {
		return "", false
	}
	suffix := bestHash
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	return fmt.Sprintf("token_waste:%s", suffix), true
}
