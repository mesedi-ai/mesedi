// Tests for the token_waste detector. Coverage targets:
//   - Three identical user_messages fire detection.
//   - Two identical user_messages (under threshold) do NOT fire.
//   - Different short suffixes on the same long prefix still cluster
//     (this is the actual production accumulation pattern).
//   - Empty / malformed payloads no-op cleanly.
//   - Signatures are deterministic across iteration order.
package detectors

import (
	"encoding/json"
	"strings"
	"testing"
)

func userPrompt(text string) json.RawMessage {
	// Field name is `user_message` to match what the SDK ships;
	// changed alongside the detector when the integration suite
	// caught the user_prompt/user_message mismatch. Helper name kept
	// as userPrompt for callsite churn-minimization.
	return json.RawMessage(`{"user_message":` + jsonString(text) + `}`)
}

// jsonString escapes a Go string into a JSON string literal.
// Minimal, only the test inputs we care about. Saves importing
// encoding/json's Marshal in test helpers.
func jsonString(s string) string {
	out := []byte{'"'}
	for _, r := range s {
		switch r {
		case '"':
			out = append(out, '\\', '"')
		case '\\':
			out = append(out, '\\', '\\')
		case '\n':
			out = append(out, '\\', 'n')
		default:
			out = append(out, []byte(string(r))...)
		}
	}
	out = append(out, '"')
	return string(out)
}

func Test_TokenWaste_FiresAtThreshold(t *testing.T) {
	payloads := []json.RawMessage{
		userPrompt("Summarize this article: ..."),
		userPrompt("Summarize this article: ..."),
		userPrompt("Summarize this article: ..."),
	}
	sig, detected := DetectTokenWaste(payloads)
	if !detected {
		t.Fatalf("expected detection at 3 identical prompts, got none")
	}
	if !strings.HasPrefix(sig, "token_waste:") {
		t.Errorf("signature missing canonical prefix: %q", sig)
	}
}

func Test_TokenWaste_DoesNotFireBelowThreshold(t *testing.T) {
	payloads := []json.RawMessage{
		userPrompt("Summarize this article: ..."),
		userPrompt("Summarize this article: ..."),
	}
	if _, detected := DetectTokenWaste(payloads); detected {
		t.Errorf("did not expect detection at 2 identical prompts")
	}
}

func Test_TokenWaste_AccumulationPattern(t *testing.T) {
	// The real production case: same leading context, different
	// trailing question. All three should cluster as token_waste
	// because their leading 2048 chars are identical.
	prefix := strings.Repeat("System context that the agent re-sends every turn. ", 50)
	payloads := []json.RawMessage{
		userPrompt(prefix + "Turn 1: please do X."),
		userPrompt(prefix + "Turn 2: please do Y."),
		userPrompt(prefix + "Turn 3: please do Z."),
	}
	sig, detected := DetectTokenWaste(payloads)
	if !detected {
		t.Fatalf("expected detection on accumulation pattern, got none. sig=%q", sig)
	}
}

func Test_TokenWaste_DistinctPromptsDoNotFire(t *testing.T) {
	payloads := []json.RawMessage{
		userPrompt("What is the weather in NYC?"),
		userPrompt("Translate 'hello' to French."),
		userPrompt("Suggest a baby name starting with R."),
	}
	if _, detected := DetectTokenWaste(payloads); detected {
		t.Errorf("did not expect detection across distinct prompts")
	}
}

func Test_TokenWaste_MixedSequence(t *testing.T) {
	payloads := []json.RawMessage{
		userPrompt("Wasted prompt body content goes here."),
		userPrompt("Different prompt 1"),
		userPrompt("Wasted prompt body content goes here."),
		userPrompt("Different prompt 2"),
		userPrompt("Wasted prompt body content goes here."),
	}
	sig, detected := DetectTokenWaste(payloads)
	if !detected {
		t.Fatalf("expected detection on 3-of-5 wasted, got none")
	}
	if !strings.HasPrefix(sig, "token_waste:") {
		t.Errorf("signature shape: %q", sig)
	}
}

func Test_TokenWaste_DeterministicSignature(t *testing.T) {
	payloads := []json.RawMessage{
		userPrompt("repeating content"),
		userPrompt("repeating content"),
		userPrompt("repeating content"),
	}
	first, _ := DetectTokenWaste(payloads)
	for i := 0; i < 50; i++ {
		got, _ := DetectTokenWaste(payloads)
		if got != first {
			t.Fatalf("signature drift on iteration %d: %q vs %q", i, got, first)
		}
	}
}

func Test_TokenWaste_EmptyInputs(t *testing.T) {
	cases := []struct {
		name     string
		payloads []json.RawMessage
	}{
		{"nil", nil},
		{"empty", []json.RawMessage{}},
		{"single", []json.RawMessage{userPrompt("solo prompt")}},
		{"two only", []json.RawMessage{
			userPrompt("only twice"),
			userPrompt("only twice"),
		}},
		{"no user_message field", []json.RawMessage{
			json.RawMessage(`{"system_prompt":"foo"}`),
			json.RawMessage(`{"system_prompt":"foo"}`),
			json.RawMessage(`{"system_prompt":"foo"}`),
		}},
		{"malformed", []json.RawMessage{
			json.RawMessage(`{bad`),
			json.RawMessage(`{bad`),
			json.RawMessage(`{bad`),
		}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, detected := DetectTokenWaste(tc.payloads); detected {
				t.Errorf("expected no detection on %s input", tc.name)
			}
		})
	}
}

// ─────────────────────────────────────────────────────────────────
// G1: near-duplicate detection (three-layer strip + exact + shingle)
// ─────────────────────────────────────────────────────────────────

// Test_TokenWaste_TimestampPrefixStripCatches verifies that prompts
// with per-turn ISO-8601 timestamps at the leading edge, which
// would previously hash distinctly and slip past the detector ,
// now cluster via the strip-then-exact-hash path. Signature uses
// the canonical `token_waste:<hex8>` shape (no `near_dup` prefix)
// because the strip path resolved to identical normalized text.
func Test_TokenWaste_TimestampPrefixStripCatches(t *testing.T) {
	body := strings.Repeat("System context the agent re-sends every turn. ", 50)
	payloads := []json.RawMessage{
		userPrompt("2026-06-23T14:23:00Z " + body + "Turn 1 question."),
		userPrompt("2026-06-23T14:23:05Z " + body + "Turn 2 question."),
		userPrompt("2026-06-23T14:23:10Z " + body + "Turn 3 question."),
	}
	sig, detected := DetectTokenWaste(payloads)
	if !detected {
		t.Fatalf("expected timestamp-stripped accumulation to cluster, got none. sig=%q", sig)
	}
	if !strings.HasPrefix(sig, "token_waste:") {
		t.Fatalf("expected canonical signature prefix, got %q", sig)
	}
	if strings.HasPrefix(sig, "token_waste:near_dup:") {
		t.Errorf("expected canonical signature (exact-hash after strip), got near_dup: %q", sig)
	}
}

// Test_TokenWaste_UUIDPrefixStripCatches verifies the same end-to-end
// behavior for hex-UUID request-id prefixes. Same canonical signature
// shape, strip resolves the variable material; exact-hash on the
// normalized text fires.
func Test_TokenWaste_UUIDPrefixStripCatches(t *testing.T) {
	body := strings.Repeat("Wasted context body. ", 100)
	payloads := []json.RawMessage{
		userPrompt("550e8400-e29b-41d4-a716-446655440000 " + body + "Turn A."),
		userPrompt("6ba7b810-9dad-11d1-80b4-00c04fd430c8 " + body + "Turn B."),
		userPrompt("6ba7b811-9dad-11d1-80b4-00c04fd430c8 " + body + "Turn C."),
	}
	sig, detected := DetectTokenWaste(payloads)
	if !detected {
		t.Fatalf("expected UUID-stripped accumulation to cluster, got none. sig=%q", sig)
	}
	if strings.HasPrefix(sig, "token_waste:near_dup:") {
		t.Errorf("expected canonical signature (exact-hash after strip), got near_dup: %q", sig)
	}
}

// Test_TokenWaste_ShingleFallbackPositive verifies that prompts the
// exact-hash path cannot resolve, variable material EMBEDDED in
// the prefix rather than at the leading edge, still cluster via
// the shingle-Jaccard fallback. Signature uses the new
// `token_waste:near_dup:<hex8>` shape so it doesn't pollute the
// canonical cluster space.
//
// Fixture uses a VARIED-content body that approximates real
// production prompt shape (system instructions, examples, RAG
// chunks). A repeating-unit body would compress the shingle set
// and inflate the impact of small differing regions, that's an
// artifact of the fixture, not the detector. Real production
// agents accumulate varied content where ~2000 chars yields
// ~2000 distinct shingles.
func Test_TokenWaste_ShingleFallbackPositive(t *testing.T) {
	// ~1800 chars of varied English. Each 8-char window is
	// effectively unique, so the shingle set size approximates the
	// length. Three prompts share this full body but have distinct
	// short IDs embedded after a shared header, strip can't reach
	// the embedded ID (it's not at the leading edge); exact-hash
	// treats them as distinct; shingle fallback should catch them.
	body := "You are a customer-support agent for an enterprise SaaS " +
		"product. Be concise, accurate, and never invent product " +
		"features. If you don't know an answer, say so and offer to " +
		"escalate. Reference the knowledge base when relevant. " +
		"Example interaction one: a customer reports their dashboard " +
		"loads slowly. Confirm the symptom, ask about browser and " +
		"network, suggest clearing cache, then check the status page. " +
		"Example interaction two: a customer asks about pricing tiers. " +
		"Direct them to the pricing page and offer to connect them with " +
		"a sales representative if they want a custom quote. Example " +
		"interaction three: a customer reports billing confusion. " +
		"Pull up their account, walk through the most recent invoice " +
		"line by line, and offer to refund any item they didn't expect. " +
		"Tone guidelines: warm but professional, never condescending, " +
		"always assume the customer has the right to be confused about " +
		"something the product team hasn't explained well yet. End " +
		"every conversation by asking if there's anything else you can " +
		"help with. Do not promise specific timelines for features that " +
		"are not yet released. Do not share information about other " +
		"customers' accounts. If asked about competitors, acknowledge " +
		"them by name and stick to factual comparisons. Refer all " +
		"security or vulnerability reports immediately to the security " +
		"contact. Refer all legal questions to the legal contact. " +
		"Tools available to you: knowledge_base_search, " +
		"account_lookup, refund_initiate, escalate_to_human."
	payloads := []json.RawMessage{
		userPrompt("Customer ID: cust_aaa111 then " + body + " first question."),
		userPrompt("Customer ID: cust_bbb222 then " + body + " second question."),
		userPrompt("Customer ID: cust_ccc333 then " + body + " third question."),
	}
	sig, detected := DetectTokenWaste(payloads)
	if !detected {
		t.Fatalf("expected shingle-fallback to catch embedded-ID near-duplicates, got none. sig=%q", sig)
	}
	if !strings.HasPrefix(sig, "token_waste:near_dup:") {
		t.Errorf("expected near_dup signature, got %q", sig)
	}
}

// Test_TokenWaste_ShingleFallbackNegative verifies the false-positive
// guard: three legitimately distinct prompts that share only short
// boilerplate vocabulary must NOT trip the shingle path. Pins the
// 0.85 threshold against drift.
func Test_TokenWaste_ShingleFallbackNegative(t *testing.T) {
	payloads := []json.RawMessage{
		userPrompt("What is the weather in New York City tomorrow morning?"),
		userPrompt("Translate this paragraph from English to French please."),
		userPrompt("Suggest five baby names starting with the letter R for a girl."),
	}
	sig, detected := DetectTokenWaste(payloads)
	if detected {
		t.Errorf("expected no detection on distinct prompts, got %q", sig)
	}
}

// Test_TokenWaste_StripDoesNotBreakDistinctPrompts pins that the
// normalization step doesn't accidentally collapse legitimately
// distinct prompts that happen to start with variable material.
// (Defensive: e.g. if all three prompts start with a timestamp AND
// have completely distinct bodies, the detector must still say no.)
func Test_TokenWaste_StripDoesNotBreakDistinctPrompts(t *testing.T) {
	payloads := []json.RawMessage{
		userPrompt("2026-06-23T14:23:00Z What is the population of Tokyo?"),
		userPrompt("2026-06-23T14:23:05Z List five renewable energy sources."),
		userPrompt("2026-06-23T14:23:10Z Explain the difference between TCP and UDP."),
	}
	if sig, detected := DetectTokenWaste(payloads); detected {
		t.Errorf("expected no detection on distinct prompts with shared timestamp prefix, got %q", sig)
	}
}

// Test_TokenWaste_MultiLayerStrip pins that stripVariablePrefixes
// walks through stacked variable material (timestamp + UUID +
// "Turn N:") in a single call so the wasteful body normalizes
// across turns.
func Test_TokenWaste_MultiLayerStrip(t *testing.T) {
	body := strings.Repeat("Identical wasteful body content. ", 80)
	payloads := []json.RawMessage{
		userPrompt(
			"2026-06-23T14:23:00Z " +
				"550e8400-e29b-41d4-a716-446655440000 " +
				"Turn 1: " + body,
		),
		userPrompt(
			"2026-06-23T14:23:05Z " +
				"6ba7b810-9dad-11d1-80b4-00c04fd430c8 " +
				"Turn 2: " + body,
		),
		userPrompt(
			"2026-06-23T14:23:10Z " +
				"6ba7b811-9dad-11d1-80b4-00c04fd430c8 " +
				"Turn 3: " + body,
		),
	}
	sig, detected := DetectTokenWaste(payloads)
	if !detected {
		t.Fatalf("expected multi-layer-strip to resolve to canonical cluster, got none. sig=%q", sig)
	}
	if strings.HasPrefix(sig, "token_waste:near_dup:") {
		t.Errorf("expected canonical signature after multi-layer strip, got %q", sig)
	}
}
