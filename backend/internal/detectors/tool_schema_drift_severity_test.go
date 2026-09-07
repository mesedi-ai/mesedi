package detectors

import (
	"encoding/json"
	"strings"
	"testing"
)

// The distinction this file exists to hold: a field appearing and a
// field disappearing are not the same event, and a detector that
// reports them identically gets muted.
//
// The 2026-09-07 radar's mcp-pin item makes the case concretely. A
// crawl found seventeen changed tool definitions in one day across 248
// servers. Seventeen indistinguishable alerts is not a signal, it is a
// reason to turn the detector off. What makes one of those seventeen
// matter is whether a consumer written against the old shape still
// works.

func shapeOf(t *testing.T, js string) string {
	t.Helper()
	s := ReturnShape(json.RawMessage(js))
	if s == "" {
		t.Fatalf("ReturnShape returned empty for %s", js)
	}
	return s
}

func classify(t *testing.T, before, after string) ShapeChange {
	t.Helper()
	return ClassifyShapeChange(shapeOf(t, before), shapeOf(t, after))
}

// A removed field is the case the whole detector exists for: the call
// still returns 200, no validator complains, and every consumer reading
// that field is now reading nothing.
func TestARemovedFieldIsBreaking(t *testing.T) {
	c := classify(t, `{"price":12,"total":30}`, `{"price":12}`)
	if c.Kind != ShapeBreaking {
		t.Fatalf("removing a field ranked %q, want breaking. Reasons: %v", c.Kind, c.Reasons)
	}
	if !strings.Contains(strings.Join(c.Reasons, "; "), "total: removed") {
		t.Errorf("the reason does not name the removed field: %v", c.Reasons)
	}
}

// A type change under the same name is worse than a removal, because
// the field is still there and still reads.
func TestATypeChangeUnderTheSameNameIsBreaking(t *testing.T) {
	c := classify(t, `{"price":12}`, `{"price":"12"}`)
	if c.Kind != ShapeBreaking {
		t.Fatalf("number to string ranked %q, want breaking", c.Kind)
	}
	if !strings.Contains(strings.Join(c.Reasons, "; "), "number -> string") {
		t.Errorf("the reason does not say what changed: %v", c.Reasons)
	}
}

// The other half. If additions ranked as breaking, every ordinary API
// version bump would page someone, which is the same muting problem
// from the opposite direction.
func TestAnAddedFieldIsCompatible(t *testing.T) {
	c := classify(t, `{"price":12}`, `{"price":12,"currency":"USD"}`)
	if c.Kind != ShapeCompatible {
		t.Fatalf("adding a field ranked %q, want compatible. Reasons: %v", c.Kind, c.Reasons)
	}
	if !strings.Contains(strings.Join(c.Reasons, "; "), "currency: added") {
		t.Errorf("the reason does not name the added field: %v", c.Reasons)
	}
}

// A change that both adds and removes must rank on the removal. Ranking
// on the addition would report a breaking change as safe, which is the
// one direction that must never happen.
func TestAMixedChangeRanksOnTheBreakingHalf(t *testing.T) {
	c := classify(t, `{"price":12,"total":30}`, `{"price":12,"currency":"USD"}`)
	if c.Kind != ShapeBreaking {
		t.Fatalf("a change that removed a field and added one ranked %q, want breaking",
			c.Kind)
	}
	joined := strings.Join(c.Reasons, "; ")
	if !strings.Contains(joined, "total: removed") || !strings.Contains(joined, "currency: added") {
		t.Errorf("both halves should be reported, most severe first: %v", c.Reasons)
	}
}

func TestNestedAndArrayChangesAreFound(t *testing.T) {
	nested := classify(t, `{"result":{"id":1}}`, `{"result":{"id":"1"}}`)
	if nested.Kind != ShapeBreaking {
		t.Errorf("a type change one level down ranked %q, want breaking", nested.Kind)
	}
	if !strings.Contains(strings.Join(nested.Reasons, "; "), "result.id") {
		t.Errorf("the reason does not carry the path: %v", nested.Reasons)
	}

	arr := classify(t, `{"rows":[{"a":1}]}`, `{"rows":[{"a":"1"}]}`)
	if arr.Kind != ShapeBreaking {
		t.Errorf("a type change inside an array element ranked %q, want breaking", arr.Kind)
	}
}

func TestAnIdenticalShapeIsUnchanged(t *testing.T) {
	c := classify(t, `{"price":12,"total":30}`, `{"total":99,"price":1}`)
	if c.Kind != ShapeUnchanged {
		t.Errorf("the same structure with different values ranked %q, want unchanged. "+
			"The shape fingerprint drops values by design", c.Kind)
	}
}

// Indeterminate must stay its own answer. Folding it into compatible
// would be a false reassurance and into breaking a false accusation,
// which is the three-outcome discipline the verifier already uses.
func TestUnparseableInputIsIndeterminateNotGuessed(t *testing.T) {
	for name, c := range map[string]ShapeChange{
		"empty baseline": ClassifyShapeChange("", "{price:number}"),
		"empty current":  ClassifyShapeChange("{price:number}", ""),
		"malformed":      ClassifyShapeChange("{price:number}", "{price:"),
	} {
		if c.Kind != ShapeIndeterminate {
			t.Errorf("%s ranked %q, want indeterminate", name, c.Kind)
		}
	}
}

// The parser must survive the shapes the existing writer emits, or the
// classifier silently degrades to indeterminate on real traffic and
// nobody notices because indeterminate looks like caution.
func TestTheParserRoundTripsWhatWriteShapeEmits(t *testing.T) {
	for _, js := range []string{
		`{"a":1}`,
		`{"a":{"b":[{"c":"x"}]}}`,
		`[]`,
		`[1,2,3]`,
		`{"empty":{},"emptyArr":[],"n":null,"t":true}`,
		`"bare string"`,
		`42`,
	} {
		shape := ReturnShape(json.RawMessage(js))
		if shape == "" {
			t.Errorf("ReturnShape produced nothing for %s", js)
			continue
		}
		if got := ClassifyShapeChange(shape, shape); got.Kind != ShapeUnchanged {
			t.Errorf("%s: comparing a shape against itself ranked %q, so the parser "+
				"cannot read what the writer emits: shape=%q reasons=%v",
				js, got.Kind, shape, got.Reasons)
		}
	}
}

// ReturnShape and ReturnShapeHash must agree about what they refuse, or
// a caller switching between them gets different answers on edge input.
func TestReturnShapeRefusesExactlyWhatTheHashRefuses(t *testing.T) {
	for _, js := range []string{``, `not json`, `{"unterminated":`} {
		shape := ReturnShape(json.RawMessage(js))
		hash := ReturnShapeHash(json.RawMessage(js))
		if (shape == "") != (hash == "") {
			t.Errorf("input %q: ReturnShape=%q but ReturnShapeHash=%q; they must "+
				"accept and refuse the same inputs", js, shape, hash)
		}
	}
}
