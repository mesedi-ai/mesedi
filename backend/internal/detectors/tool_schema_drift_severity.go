package detectors

import (
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

// Ranking for tool_schema_drift.
//
// THE PROBLEM THIS SOLVES
//
// The detector fires when a tool's return shape stops matching its
// historical majority, and every firing looks identical. The 2026-09-07
// incident radar put it exactly right: a drift detector without a
// breaking-change classifier is a diff tool with an alert channel
// attached, and an operator who gets seventeen indistinguishable alerts
// in a day mutes the detector inside a week.
//
// A widened enum and a removed required field are not the same event. A
// field appearing is almost always survivable: the agent's prompt did
// not mention it and nothing downstream reads it. A field disappearing,
// or changing type under the same name, breaks every consumer that was
// reading it, silently, while the call still returns 200 and no
// validator complains. That is the whole reason this detector exists.
//
// WHY THIS COULD NOT REUSE ReturnShapeHash
//
// ReturnShapeHash builds the shape string and then throws it away,
// returning only its SHA-256. A hash can tell you two shapes differ. It
// can never tell you how. Ranking needs the shape itself, so ReturnShape
// exposes the string that was previously discarded, and the classifier
// parses two of them.
//
// WHAT THIS DOES NOT DO, AND IT IS THE RADAR'S REAL POINT
//
// This ranks drift in what a tool RETURNS. It does not rank, or notice,
// changes to a tool's declared DEFINITION: its input schema, its
// annotations, its description. Mesedi never sees a tool definition,
// because neither ToolCallPayload nor MCPCallPayload carries one. The
// mcp-pin crawl the radar describes found seventeen definitions whose
// input schema changed while the description stayed byte-identical, and
// Mesedi would notice none of them. That is a coverage gap needing an
// event-schema and SDK change, tracked separately, and this file does
// not close it.

// ShapeChangeKind ranks one drift event.
type ShapeChangeKind string

const (
	// ShapeUnchanged means the two shapes are identical.
	ShapeUnchanged ShapeChangeKind = "unchanged"

	// ShapeCompatible means everything the baseline had is still
	// present with the same type. Fields were added and nothing else.
	// A consumer written against the baseline keeps working.
	ShapeCompatible ShapeChangeKind = "compatible"

	// ShapeBreaking means something the baseline had is gone, or has
	// changed type under the same name. A consumer written against the
	// baseline is now reading a missing or wrong-typed value, and the
	// call still succeeds, which is why nothing else catches it.
	ShapeBreaking ShapeChangeKind = "breaking"

	// ShapeIndeterminate means one of the shapes could not be parsed,
	// so no honest comparison is possible. Deliberately NOT folded
	// into any of the above: reporting an unparseable shape as
	// compatible would be a false reassurance, and reporting it as
	// breaking would be a false accusation. It is neither.
	ShapeIndeterminate ShapeChangeKind = "indeterminate"
)

// ShapeChange is the ranked result, with the specific reasons.
type ShapeChange struct {
	Kind ShapeChangeKind

	// Reasons name each individual difference, most severe first,
	// in dotted-path form, for example "result.price: number -> string"
	// or "result.total: removed". Empty when Kind is ShapeUnchanged.
	//
	// Carried because "breaking" on its own tells an operator to go
	// and diff two JSON blobs by hand, which is the work this is
	// meant to save.
	Reasons []string
}

// ReturnShape is ReturnShapeHash without the hashing step: the
// structural fingerprint as a readable string. Returns "" on the same
// inputs ReturnShapeHash returns "" on, so callers can treat an empty
// result identically in both.
func ReturnShape(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any // the return value's structure is unknown by definition; that is what this discovers
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	var b strings.Builder
	if err := writeShape(&b, v); err != nil {
		return ""
	}
	return b.String()
}

// ClassifyShapeChange ranks the move from baselineShape to
// currentShape. Both are strings produced by ReturnShape.
func ClassifyShapeChange(baselineShape, currentShape string) ShapeChange {
	if baselineShape == "" || currentShape == "" {
		return ShapeChange{Kind: ShapeIndeterminate,
			Reasons: []string{"one of the two shapes is empty, so nothing can be compared"}}
	}
	if baselineShape == currentShape {
		return ShapeChange{Kind: ShapeUnchanged}
	}
	base, err1 := parseShape(baselineShape)
	cur, err2 := parseShape(currentShape)
	if err1 != nil || err2 != nil {
		return ShapeChange{Kind: ShapeIndeterminate,
			Reasons: []string{"a shape string could not be parsed, so the difference " +
				"cannot be ranked without guessing"}}
	}

	var breaking, compatible []string
	diffShape("", base, cur, &breaking, &compatible)

	switch {
	case len(breaking) > 0:
		return ShapeChange{Kind: ShapeBreaking, Reasons: append(breaking, compatible...)}
	case len(compatible) > 0:
		return ShapeChange{Kind: ShapeCompatible, Reasons: compatible}
	default:
		// The strings differed but no structural difference was found.
		// Reachable through array element ordering. Honest answer is
		// that it changed and we cannot say how.
		return ShapeChange{Kind: ShapeIndeterminate,
			Reasons: []string{"the shapes differ but no field-level difference was found"}}
	}
}

// diffShape walks both trees together, accumulating reasons.
func diffShape(path string, base, cur *shapeNode, breaking, compatible *[]string) {
	at := func(k string) string {
		if path == "" {
			return k
		}
		return path + "." + k
	}
	label := path
	if label == "" {
		label = "(root)"
	}

	if base.kind != cur.kind {
		*breaking = append(*breaking,
			fmt.Sprintf("%s: %s -> %s", label, base.kind, cur.kind))
		return
	}

	switch base.kind {
	case "object":
		for _, k := range sortedKeys(base.fields) {
			cf, ok := cur.fields[k]
			if !ok {
				*breaking = append(*breaking, fmt.Sprintf("%s: removed", at(k)))
				continue
			}
			diffShape(at(k), base.fields[k], cf, breaking, compatible)
		}
		for _, k := range sortedKeys(cur.fields) {
			if _, ok := base.fields[k]; !ok {
				*compatible = append(*compatible, fmt.Sprintf("%s: added", at(k)))
			}
		}
	case "array":
		switch {
		case base.elem == nil && cur.elem == nil:
		case base.elem == nil:
			*compatible = append(*compatible, fmt.Sprintf("%s: was empty, now populated", label))
		case cur.elem == nil:
			// An array that used to carry elements and is now always
			// empty is a real regression, not a shape addition.
			*breaking = append(*breaking, fmt.Sprintf("%s: elements gone", label))
		default:
			diffShape(label+"[]", base.elem, cur.elem, breaking, compatible)
		}
	}
}

func sortedKeys(m map[string]*shapeNode) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

// shapeNode is a parsed shape string. kind is one of the atom names
// writeShape emits ("null", "bool", "number", "string"), or "object",
// or "array".
type shapeNode struct {
	kind   string
	fields map[string]*shapeNode // kind == "object"
	elem   *shapeNode            // kind == "array", nil when the array was empty
}

// parseShape reads the grammar writeShape emits:
//
//	shape  := "null" | "bool" | "number" | "string" | array | object
//	array  := "[" [shape] "]"
//	object := "{" [ key ":" shape { "," key ":" shape } ] "}"
//
// Keys are raw and unquoted, exactly as writeShape wrote them, so a key
// containing a comma, colon or brace is ambiguous. That ambiguity is
// inherited from the existing shape format rather than introduced here,
// and it fails closed: a shape that will not parse is reported as
// indeterminate rather than mis-ranked.
func parseShape(s string) (*shapeNode, error) {
	p := &shapeParser{s: s}
	n, err := p.parseNode()
	if err != nil {
		return nil, err
	}
	if p.i != len(p.s) {
		return nil, fmt.Errorf("trailing input at %d", p.i)
	}
	return n, nil
}

type shapeParser struct {
	s string
	i int
}

func (p *shapeParser) parseNode() (*shapeNode, error) {
	if p.i >= len(p.s) {
		return nil, fmt.Errorf("unexpected end of shape")
	}
	switch p.s[p.i] {
	case '{':
		return p.parseObject()
	case '[':
		return p.parseArray()
	}
	for _, atom := range []string{"null", "bool", "number", "string"} {
		if strings.HasPrefix(p.s[p.i:], atom) {
			p.i += len(atom)
			return &shapeNode{kind: atom}, nil
		}
	}
	return nil, fmt.Errorf("unrecognised atom at %d", p.i)
}

func (p *shapeParser) parseArray() (*shapeNode, error) {
	p.i++ // consume '['
	n := &shapeNode{kind: "array"}
	if p.i < len(p.s) && p.s[p.i] == ']' {
		p.i++
		return n, nil
	}
	elem, err := p.parseNode()
	if err != nil {
		return nil, err
	}
	if p.i >= len(p.s) || p.s[p.i] != ']' {
		return nil, fmt.Errorf("unterminated array at %d", p.i)
	}
	p.i++
	n.elem = elem
	return n, nil
}

func (p *shapeParser) parseObject() (*shapeNode, error) {
	p.i++ // consume '{'
	n := &shapeNode{kind: "object", fields: map[string]*shapeNode{}}
	if p.i < len(p.s) && p.s[p.i] == '}' {
		p.i++
		return n, nil
	}
	for {
		colon := strings.IndexByte(p.s[p.i:], ':')
		if colon < 0 {
			return nil, fmt.Errorf("object key without a colon at %d", p.i)
		}
		key := p.s[p.i : p.i+colon]
		p.i += colon + 1
		val, err := p.parseNode()
		if err != nil {
			return nil, err
		}
		n.fields[key] = val
		if p.i >= len(p.s) {
			return nil, fmt.Errorf("unterminated object")
		}
		switch p.s[p.i] {
		case ',':
			p.i++
		case '}':
			p.i++
			return n, nil
		default:
			return nil, fmt.Errorf("unexpected byte %q in object at %d", p.s[p.i], p.i)
		}
	}
}
