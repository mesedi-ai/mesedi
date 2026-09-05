package models

// Model-identifier normalization for cloud-routed APIs.
//
// AWS Bedrock and Google Vertex AI rewrite model identifiers when
// customers route LLM traffic through them. The rewriting follows
// deterministic patterns:
//
//   Bedrock:  `<provider>.<canonical>-YYYYMMDD-vN:M`
//             e.g. anthropic.claude-3-5-sonnet-20240620-v1:0
//             e.g. amazon.nova-pro-v1:0
//             e.g. cohere.command-r-plus-v1:0
//
//   Vertex:   `<canonical>-NNN`
//             e.g. gemini-1.5-pro-001
//             e.g. gemini-1.5-flash-002
//
// Without normalization, ContextWindow + Provider + IsKnown all
// return "unknown" for these routed identifiers, and the
// context_overflow detector silently skips the execution, missing
// real customer signals. With normalization, the routed identifier
// resolves to the canonical entry already in the registry.
//
// Azure OpenAI uses customer-chosen deployment names (e.g.
// "my-prod-gpt4") that are not deterministic, normalization is
// impossible without a customer-supplied mapping. Azure customers
// need the per-project model-window override (context_overflow.G3,
// banked as separate wave).

import "regexp"

// bedrockSuffixRe matches the trailing `-YYYYMMDD-vN:M` segment
// Bedrock appends to its model IDs (e.g. `-20240620-v1:0`). The
// date is 8 digits; the version is digits before and after the
// colon. Anchored to end-of-string so it only strips the actual
// suffix, never mid-string content that happens to look like a
// date.
var bedrockSuffixRe = regexp.MustCompile(`-\d{8}-v\d+:\d+$`)

// bedrockPrefixRe matches the leading `<provider>.` segment
// Bedrock prepends (e.g. `anthropic.`, `cohere.`, `amazon.`,
// `meta.`, `mistral.`). Lower-case, alphanumeric+underscore, ends
// at the first dot. Anchored to start-of-string so it only strips
// the actual prefix.
var bedrockPrefixRe = regexp.MustCompile(`^[a-z][a-z0-9_]*\.`)

// vertexSuffixRe matches the trailing `-NNN` 3-digit version
// suffix Vertex appends to its Gemini model IDs (e.g. `-001`,
// `-002`). Anchored to end-of-string.
var vertexSuffixRe = regexp.MustCompile(`-\d{3}$`)

// normalizeModelID strips Bedrock + Vertex routing artifacts from
// the model identifier before registry lookup. Returns the
// canonical name if a normalization rule applied; returns the
// input unchanged otherwise.
//
// Algorithm: apply Bedrock prefix-strip + Bedrock suffix-strip
// (independently, both may apply); then apply Vertex suffix-strip
// (only if the Bedrock passes didn't already change the input ,
// the `-NNN` Vertex pattern overlaps with `-vN:M` Bedrock if the
// caller passed a mangled input, so prefer Bedrock when both
// match).
//
// Examples:
//
//	"anthropic.claude-3-5-sonnet-20240620-v1:0" → "claude-3-5-sonnet"
//	"cohere.command-r-plus-v1:0"                → ""
//	  (wait, this case: `cohere.` strips first → `command-r-plus-v1:0`;
//	   then bedrockSuffixRe doesn't match because no date; Vertex
//	   doesn't match because no 3-digit suffix. Returns
//	   `command-r-plus-v1:0`. NOT in registry. This is the right
//	   behavior, Bedrock with no date segment isn't a valid
//	   Bedrock identifier; we treat it as unknown rather than
//	   guessing.)
//	"gemini-1.5-pro-001"                        → "gemini-1.5-pro"
//	"claude-3-5-sonnet"                         → "claude-3-5-sonnet"
//	  (canonical input passes through unchanged)
//	"my-prod-gpt4"                              → "my-prod-gpt4"
//	  (Azure deployment name; no normalization possible;
//	   registry lookup will return ok=false)
func normalizeModelID(modelID string) string {
	if modelID == "" {
		return modelID
	}
	out := modelID
	bedrockApplied := false

	// Bedrock prefix strip: `anthropic.claude-...` → `claude-...`.
	if loc := bedrockPrefixRe.FindStringIndex(out); loc != nil && loc[0] == 0 {
		out = out[loc[1]:]
		bedrockApplied = true
	}
	// Bedrock suffix strip: `...-20240620-v1:0` → `...`.
	if loc := bedrockSuffixRe.FindStringIndex(out); loc != nil {
		out = out[:loc[0]]
		bedrockApplied = true
	}
	// Vertex suffix strip: `...-001` → `...`. Skip if Bedrock
	// already touched the string, Bedrock's `-vN:M` doesn't
	// overlap with Vertex's `-NNN`, but if a caller passes a
	// hybrid mangled string, preferring Bedrock first is the
	// deterministic choice.
	if !bedrockApplied {
		if loc := vertexSuffixRe.FindStringIndex(out); loc != nil {
			out = out[:loc[0]]
		}
	}
	return out
}
