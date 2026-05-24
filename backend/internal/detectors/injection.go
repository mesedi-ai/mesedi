// Package detectors holds detection logic that doesn't fit cleanly in
// the storage layer (where SQL handles the predicate-pushable bits) or
// the handler layer (which orchestrates but doesn't classify).
//
// Today's only detector is prompt-injection, regex pattern matching
// against LLM user messages. Future detectors planned for this package:
//
//   - Identical-call loop detector (the 3rd Phase-4 sub-detector)
//   - Similar-call loop detector (cosine similarity over embeddings)
//   - Drift detector (semantic drift over time)
//   - Cost-velocity detector (cost > baseline × N)
//
// All detectors should be PURE FUNCTIONS, given input text/events,
// return a classification. Side effects (DB writes) live in handlers.
package detectors

import "regexp"

// InjectionPattern is one rule in the prompt-injection rulebook: a
// short canonical name + a compiled regex that matches the pattern.
// Multiple regexes can share a name (e.g. several variants of "role
// override") so all of them roll up into the same failure_group.
type InjectionPattern struct {
	Name    string
	Pattern *regexp.Regexp
}

// injectionPatterns is the ordered list of patterns. Order matters:
// DetectInjection returns the FIRST match, so the most-specific
// patterns must come first.
//
// Tier 1 (literal sentinels, high precision, low recall): patterns
// that match literal strings rarely found in benign text. If we see
// "[INST]" or "<<SYS>>" in user input, it's almost certainly an attack
// trying to spoof a chat-template control token.
//
// Tier 2 (named jailbreak / mode phrases): "DAN", "developer mode" , 
// well-known persona overrides.
//
// Tier 3 (semantic role override): "you are now", "from now on you".
// Could appear in benign roleplay; still worth flagging.
//
// Tier 4 (broad ignore/disregard catch-all): the lowest-precision
// patterns last, so a prompt that's already matched something more
// specific doesn't fall through into this generic bucket.
//
// Naming convention: snake_case signatures. New patterns: insert at
// the tier where they belong, keep the `(?i)` flag unless the literal
// case IS the attack vector.
var injectionPatterns = []InjectionPattern{
	// ── Tier 1: literal sentinels ────────────────────────────────────
	{
		Name:    "instruction_tag",
		Pattern: regexp.MustCompile(`\[INST\]|\[/INST\]|<<SYS>>|<</SYS>>`),
	},
	{
		Name:    "system_prompt_inject",
		Pattern: regexp.MustCompile(`(?i)<\s*system\s*>|<\|system\|>|^\s*system\s*:`),
	},
	// ── Tier 2: named jailbreaks ─────────────────────────────────────
	{
		Name:    "jailbreak_dan",
		Pattern: regexp.MustCompile(`(?i)do\s+anything\s+now|^DAN\s*$|act\s+as\s+DAN`),
	},
	{
		Name:    "developer_mode",
		Pattern: regexp.MustCompile(`(?i)developer\s+mode|jailbreak\s+mode|admin\s+mode`),
	},
	// ── Tier 3: role override ────────────────────────────────────────
	{
		Name:    "role_override",
		Pattern: regexp.MustCompile(`(?i)you\s+are\s+now\s+`),
	},
	{
		Name:    "role_override",
		Pattern: regexp.MustCompile(`(?i)from\s+now\s+on,?\s+you`),
	},
	// ── Tier 4: broad ignore/disregard catch-alls ────────────────────
	{
		Name:    "ignore_instructions",
		Pattern: regexp.MustCompile(`(?i)ignore\s+(the\s+)?(previous|above|prior|earlier|all)`),
	},
	{
		Name:    "ignore_instructions",
		Pattern: regexp.MustCompile(`(?i)disregard\s+(the|your|all|previous|above)`),
	},
}

// DetectInjection scans the input for known prompt-injection patterns
// and returns the first match's signature name. Returns ("", false)
// when no patterns match.
//
// This is a low-recall / high-precision detector by design, false
// positives are far worse than false negatives for an alerting
// surface. Customers will tune their own patterns once the dashboard
// supports per-project rules (Phase 7+).
//
// Empty input returns ("", false) without scanning.
func DetectInjection(text string) (string, bool) {
	if text == "" {
		return "", false
	}
	for _, p := range injectionPatterns {
		if p.Pattern.MatchString(text) {
			return p.Name, true
		}
	}
	return "", false
}
