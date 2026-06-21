package detectors

// Fingerprint-stability tests for tool_schema_drift (#270.d
// closeout). The SDK ships typed-sentinel structures via the
// schema-preserving coercion shipped in #270.b — e.g. a datetime
// becomes `{"__type__":"datetime","value":"..."}`. These tests
// pin the contract between the SDK's coercion and the backend's
// ReturnShapeHash so a future SDK change can't silently produce
// drift signal where there is none, and vice versa.
//
// The tests cover three classes of guarantee:
//
//   1. STABILITY — the same logical shape produces the same hash
//      across:
//        - dict key order (sort_keys property)
//        - value-text changes (only structure matters)
//        - run-to-run determinism
//
//   2. DISTINCTNESS — shapes that DIFFER in any structural way
//      produce distinct hashes:
//        - adding/removing a key
//        - typed-sentinel vs plain string (the #270.b property)
//        - changing a value's type
//
//   3. SDK COMPATIBILITY — the typed-sentinel structures emitted
//      by `mesedi._structured_return_value` (Python) and
//      `structuredReturnValue` (TypeScript) hash to a deterministic
//      shape the detector understands.

import (
	"encoding/json"
	"testing"
)

// rawJSON is a small helper: marshal `v` to json.RawMessage so the
// test reads naturally as "hash of this Go literal."
func rawJSON(t *testing.T, v any) json.RawMessage {
	t.Helper()
	b, err := json.Marshal(v)
	if err != nil {
		t.Fatalf("marshal %v: %v", v, err)
	}
	return json.RawMessage(b)
}

// ──────────────────────────────────────────────────────────────────
// STABILITY — same shape → same hash
// ──────────────────────────────────────────────────────────────────

func TestReturnShapeHash_StableAcrossKeyOrder(t *testing.T) {
	a := rawJSON(t, map[string]any{"a": 1, "b": "x", "c": true})
	b := rawJSON(t, map[string]any{"c": true, "b": "x", "a": 1})
	if ReturnShapeHash(a) != ReturnShapeHash(b) {
		t.Errorf("hash differs by key order: %q vs %q",
			ReturnShapeHash(a), ReturnShapeHash(b))
	}
}

func TestReturnShapeHash_StableAcrossValueText(t *testing.T) {
	a := rawJSON(t, map[string]any{"name": "widget", "price": 1.99})
	b := rawJSON(t, map[string]any{"name": "completely different value", "price": 99999.99})
	if ReturnShapeHash(a) != ReturnShapeHash(b) {
		t.Errorf("hash differs by value text; should only depend on shape")
	}
}

func TestReturnShapeHash_DeterministicAcrossRuns(t *testing.T) {
	v := rawJSON(t, map[string]any{
		"id":    "x",
		"items": []any{map[string]any{"k": 1}},
	})
	first := ReturnShapeHash(v)
	for i := 0; i < 100; i++ {
		if ReturnShapeHash(v) != first {
			t.Errorf("non-deterministic hash at iter %d", i)
		}
	}
}

// ──────────────────────────────────────────────────────────────────
// DISTINCTNESS — structural change → different hash
// ──────────────────────────────────────────────────────────────────

func TestReturnShapeHash_DistinctByAddedKey(t *testing.T) {
	a := rawJSON(t, map[string]any{"id": "x"})
	b := rawJSON(t, map[string]any{"id": "x", "extra": 1})
	if ReturnShapeHash(a) == ReturnShapeHash(b) {
		t.Errorf("hash should differ when a key is added")
	}
}

func TestReturnShapeHash_DistinctByValueType(t *testing.T) {
	stringField := rawJSON(t, map[string]any{"v": "string-value"})
	numberField := rawJSON(t, map[string]any{"v": 42})
	boolField := rawJSON(t, map[string]any{"v": true})
	// All three must produce distinct hashes.
	s, n, b := ReturnShapeHash(stringField), ReturnShapeHash(numberField), ReturnShapeHash(boolField)
	if s == n || s == b || n == b {
		t.Errorf("hashes collided for distinct value types: string=%q number=%q bool=%q", s, n, b)
	}
}

// ──────────────────────────────────────────────────────────────────
// SDK COMPATIBILITY — typed sentinels from #270.b
// ──────────────────────────────────────────────────────────────────

func TestReturnShapeHash_TypedSentinelDistinctFromPlainString(t *testing.T) {
	// The whole point of #270.b: a real datetime field (shipped by
	// the SDK as {"__type__":"datetime","value":"..."}) must hash
	// to a DIFFERENT shape than a plain string field with the same
	// content. Pre-#270.b both collapsed to {key:string} and silently
	// masked schema drift.
	typedDatetime := rawJSON(t, map[string]any{
		"created_at": map[string]any{
			"__type__": "datetime",
			"value":    "2026-06-21T12:00:00",
		},
	})
	plainString := rawJSON(t, map[string]any{
		"created_at": "2026-06-21T12:00:00",
	})
	if ReturnShapeHash(typedDatetime) == ReturnShapeHash(plainString) {
		t.Errorf("typed-sentinel datetime field hashed identically to plain string; #270.b regression")
	}
}

func TestReturnShapeHash_TypedSentinelsByTypeAreDistinct(t *testing.T) {
	// datetime / uuid / decimal / bytes / object-class sentinels
	// must all hash distinctly. Otherwise a tool that switches from
	// returning a UUID to returning a Decimal would silently fail
	// to fire schema_drift.
	cases := []struct {
		name string
		val  any
	}{
		{"datetime", map[string]any{"__type__": "datetime", "value": "..."}},
		{"uuid", map[string]any{"__type__": "uuid", "value": "..."}},
		{"decimal", map[string]any{"__type__": "decimal", "value": "..."}},
		{"bytes", map[string]any{"__type__": "bytes", "value": "...", "size": 1}},
		{"path", map[string]any{"__type__": "path", "value": "..."}},
		{"bigint", map[string]any{"__type__": "bigint", "value": "..."}},
		{"object", map[string]any{"__type__": "object", "class": "User", "value": "..."}},
	}
	seen := map[string]string{}
	for _, c := range cases {
		hash := ReturnShapeHash(rawJSON(t, map[string]any{"v": c.val}))
		if prior, dup := seen[hash]; dup {
			t.Errorf("sentinel %q hashed identically to %q (hash %q)",
				c.name, prior, hash)
		}
		seen[hash] = c.name
	}
}

func TestReturnShapeHash_TypedSentinelObjectClassDistinguishesUserVsAdminUser(t *testing.T) {
	// The "returns User vs returns AdminUser" case from the #270
	// audit. Both have the same field names but distinct class
	// names; the object sentinel must surface that distinction.
	user := rawJSON(t, map[string]any{
		"item": map[string]any{
			"__type__": "object",
			"class":    "User",
			"value":    "User(id=1)",
		},
	})
	adminUser := rawJSON(t, map[string]any{
		"item": map[string]any{
			"__type__": "object",
			"class":    "AdminUser",
			"value":    "AdminUser(id=1)",
		},
	})
	if ReturnShapeHash(user) == ReturnShapeHash(adminUser) {
		t.Errorf("User vs AdminUser hashed identically; class tag in object sentinel isn't reaching the fingerprint")
	}
}

// ──────────────────────────────────────────────────────────────────
// EDGE CASES — empty input, malformed input, deeply nested
// ──────────────────────────────────────────────────────────────────

func TestReturnShapeHash_EmptyInputReturnsEmpty(t *testing.T) {
	if got := ReturnShapeHash(nil); got != "" {
		t.Errorf("nil input should hash to empty, got %q", got)
	}
	if got := ReturnShapeHash(json.RawMessage{}); got != "" {
		t.Errorf("empty input should hash to empty, got %q", got)
	}
}

func TestReturnShapeHash_MalformedJSONReturnsEmpty(t *testing.T) {
	if got := ReturnShapeHash(json.RawMessage(`{"not valid`)); got != "" {
		t.Errorf("malformed JSON should hash to empty, got %q", got)
	}
}

func TestReturnShapeHash_DeeplyNestedSurvives(t *testing.T) {
	// Build a {a: {a: {a: {...}}}} chain 50 levels deep. Detector
	// must hash deterministically without recursion-depth panics.
	var build func(depth int) any
	build = func(depth int) any {
		if depth == 0 {
			return "leaf"
		}
		return map[string]any{"a": build(depth - 1)}
	}
	v := rawJSON(t, build(50))
	if h := ReturnShapeHash(v); h == "" {
		t.Errorf("deeply nested input hashed to empty (panic / recursion limit?)")
	}
}
