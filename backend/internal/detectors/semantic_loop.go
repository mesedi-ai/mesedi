// Semantic-loop detector.
//
// Distinct from the existing `loops` family (time_budget, step_count,
// identical_call), which catches the cases where an agent repeats the
// EXACT same call or burns the EXACT same wall-clock budget. Semantic
// loops are subtler: the agent's reasoning revisits the same logical
// state under different surface text. A research agent queries a web
// search, then a vector DB, then a sql tool, all to obtain
// conceptually identical information; the raw event payloads look
// distinct, so step_count / identical_call signatures don't cluster
// them, and the agent burns through its token budget unchecked.
//
// This detector works by hashing the canonicalized state at each
// checkpoint and reporting any hash that appears 3+ times within the
// execution. Canonicalization is deterministic across SDK versions:
// JSON object keys are sorted, string values are lowercased, integers
// are kept as-is, floats are rounded to 2 decimals, and whitespace is
// stripped. The result is a stable byte-fingerprint of the agent's
// logical state independent of presentation churn.
//
// Implementation notes:
//
//  1. The 3-occurrence threshold and the 8-char signature prefix are
//     both stable: the same execution will always produce the same
//     decision and the same group_id signature when re-run with the
//     same checkpoints. Detector idempotency mirrors the other
//     detectors in this package.
//
//  2. Canonicalization deliberately does NOT strip number values
//     entirely (e.g. step counters). The premise is that agents
//     stuck in semantic loops produce the SAME numbers as well:
//     they're not making progress against their internal state
//     either. If a downstream variant catches agents that increment
//     a counter while revisiting the same content, that's its own
//     detector (Tier 3).
//
//  3. The detector is pure: no I/O, no clock, no concurrency. Same
//     inputs always produce the same outputs. The handler calls it
//     once per terminal-status update.
package detectors

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strconv"
	"strings"
)

// minRevisits is the threshold at which a hash repeating in
// checkpoints flips from "noise" to "semantic loop." Three was chosen
// because it matches the established pattern from
// identical_call_loops (which fires at 3 identical tool calls); two
// looks legitimate (an agent re-checking work), three is suspicious.
const minRevisits = 3

// signatureLen controls the human-readable prefix length used in the
// failure_group signature. 8 hex chars give 32 bits of entropy which
// is enough to disambiguate distinct semantic loops within a project
// while keeping the signature scan-friendly.
const signatureLen = 8

// SemanticLoopThresholds carries the per-project tunable values for
// this detector. RevisitThreshold defaults to minRevisits
// (3) for customers who don't tune.
type SemanticLoopThresholds struct {
	RevisitThreshold int
}

// DefaultSemanticLoopThresholds returns the values that match the
// detector's historical hardcoded behavior. Used by call sites that
// don't have per-project config available (legacy unit tests,
// synthetic executions in test harnesses).
func DefaultSemanticLoopThresholds() SemanticLoopThresholds {
	return SemanticLoopThresholds{RevisitThreshold: minRevisits}
}

// DetectSemanticLoop scans the supplied checkpoint payloads in
// sequence order and reports the most-revisited canonical-state hash
// if it crossed the threshold. Returns (signature, true) on detection,
// or ("", false) when no hash repeated enough times.
//
// `payloads` is the ordered list of checkpoint event payloads, each a
// json.RawMessage (the value of events.Event.Payload). Non-JSON
// payloads, payloads with no `state` field, and zero-length input all
// gracefully no-op.
//
// The returned signature has the format "semantic_loop:<hex8>" where
// <hex8> is the first 8 hex chars of the SHA-256 over the canonical
// state. This pattern lets the handler treat the signature as opaque
// (same as TimeBudgetSignature / StepCountSignature) while still
// giving on-call responders a stable, copy-pasteable cluster
// identifier.
//
// Preserved verbatim for backward compatibility with existing unit
// tests + non-handler call sites. The production execution-close
// path uses DetectSemanticLoopWithThresholds.
func DetectSemanticLoop(payloads []json.RawMessage) (signature string, detected bool) {
	return DetectSemanticLoopWithThresholds(payloads, DefaultSemanticLoopThresholds())
}

// DetectSemanticLoopWithThresholds is the per-project-aware variant
// of DetectSemanticLoop. RevisitThreshold from t overrides the
// hardcoded minRevisits. Defensive: values below 2 fall back to the
// default (a 1-revisit threshold would fire on every single repeat;
// the validators registry rejects this at write time but we defend
// on read regardless).
func DetectSemanticLoopWithThresholds(
	payloads []json.RawMessage,
	t SemanticLoopThresholds,
) (signature string, detected bool) {
	threshold := t.RevisitThreshold
	if threshold < 2 {
		threshold = minRevisits
	}
	if len(payloads) < threshold {
		return "", false
	}
	counts := make(map[string]int, len(payloads))
	for _, p := range payloads {
		state := extractState(p)
		if len(state) == 0 {
			continue
		}
		h := canonicalHash(state)
		if h == "" {
			continue
		}
		counts[h]++
	}
	// Find the highest-count hash; tie-break by lexicographic hash
	// so the same input always picks the same winner.
	type entry struct {
		hash  string
		count int
	}
	best := entry{}
	for h, c := range counts {
		if c < threshold {
			continue
		}
		if c > best.count || (c == best.count && h < best.hash) {
			best = entry{hash: h, count: c}
		}
	}
	if best.count == 0 {
		return "", false
	}
	if len(best.hash) > signatureLen {
		return "semantic_loop:" + best.hash[:signatureLen], true
	}
	return "semantic_loop:" + best.hash, true
}

// extractState pulls the `metadata` field from a checkpoint payload.
// Returns nil when the payload can't be parsed or the field is absent.
//
// Field name is `metadata` to match what the SDK's checkpoint()
// helper ships (mesedi.checkpoint(name, **kwargs) serializes the
// kwargs under "metadata"). The detector originally read "state"
// which no SDK ever emitted, so extractState always returned nil
// and semantic_loop silently no-op'd on every real customer
// execution. Caught by the integration suite (backend/test/
// integration/test_detectors.py::test_semantic_loop).
func extractState(payload json.RawMessage) json.RawMessage {
	if len(payload) == 0 {
		return nil
	}
	var pm map[string]json.RawMessage
	if err := json.Unmarshal(payload, &pm); err != nil {
		return nil
	}
	return pm["metadata"]
}

// canonicalHash produces a stable SHA-256 hex string for the given
// JSON value. Returns "" when canonicalization fails. The
// canonicalization is structure-preserving: JSON null/true/false
// stay as-is, numbers round to 2 decimals, strings lowercase and
// trim, arrays preserve order, objects sort keys alphabetically.
func canonicalHash(raw json.RawMessage) string {
	canon, err := canonicalize(raw)
	if err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(canon))
	return hex.EncodeToString(sum[:])
}

// canonicalize is the core normalization function. The serialized
// output is byte-stable: the same logical JSON value always produces
// the same output regardless of the producer's whitespace, key
// ordering, or capitalization choices.
func canonicalize(raw json.RawMessage) (string, error) {
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return "", err
	}
	var b strings.Builder
	if err := writeCanonical(&b, v); err != nil {
		return "", err
	}
	return b.String(), nil
}

// writeCanonical walks the decoded JSON value and writes its
// canonical form to b. Depth is implicitly bounded by Go's
// json.Unmarshal recursion limit.
func writeCanonical(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		if x {
			b.WriteString("true")
		} else {
			b.WriteString("false")
		}
	case float64:
		// json.Unmarshal decodes all JSON numbers as float64.
		// Integer-valued floats render without a decimal so e.g.
		// step_number=5 and step_number=5.0 hash identically.
		if x == float64(int64(x)) {
			b.WriteString(strconv.FormatInt(int64(x), 10))
		} else {
			// Round to 2 decimals so trivial floating-point noise
			// across runs (5.000000001 vs 5.000000002) doesn't
			// disrupt clustering.
			b.WriteString(strconv.FormatFloat(roundTo(x, 2), 'f', -1, 64))
		}
	case string:
		// Lowercase + trim whitespace so trivial casing / formatting
		// variants share a fingerprint.
		s := strings.ToLower(strings.TrimSpace(x))
		bs, err := json.Marshal(s)
		if err != nil {
			return err
		}
		b.Write(bs)
	case []any:
		b.WriteByte('[')
		for i, item := range x {
			if i > 0 {
				b.WriteByte(',')
			}
			if err := writeCanonical(b, item); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		keys := make([]string, 0, len(x))
		for k := range x {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		b.WriteByte('{')
		for i, k := range keys {
			if i > 0 {
				b.WriteByte(',')
			}
			bs, err := json.Marshal(strings.ToLower(k))
			if err != nil {
				return err
			}
			b.Write(bs)
			b.WriteByte(':')
			if err := writeCanonical(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("semantic-loop: unsupported JSON node type %T", v)
	}
	return nil
}

// roundTo rounds a float to n decimal places using banker's rounding.
// Stable across Go versions; doesn't depend on float printing quirks.
func roundTo(x float64, n int) float64 {
	mult := 1.0
	for i := 0; i < n; i++ {
		mult *= 10
	}
	rounded := float64(int64(x*mult + 0.5))
	if x < 0 {
		rounded = float64(int64(x*mult - 0.5))
	}
	return rounded / mult
}
