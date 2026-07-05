// Tool schema-drift detector.
//
// Catches the production case where a tool's return-value SHAPE
// changes silently between runs. The most common driver is a
// third-party API the customer's tool wraps version-bumping its
// response (e.g. adding a new top-level field, renaming a key,
// changing a string field to an object). Downstream agent reasoning
// continues against the new shape but produces subtly wrong answers
// because the LLM's prompt assumed the old shape.
//
// Distinct from `tool_failures`: the call SUCCEEDED, no error fired,
// no validator complained. The only signal is that the shape is
// different from what this tool used to return.
//
// Implementation pieces:
//
//  1. ReturnShapeHash is the pure function that produces a stable
//     fingerprint of a JSON value's STRUCTURE: sorted keys + value
//     type. Values themselves are dropped (a tool returning
//     {"price":12} and {"price":99} hash identically; both are
//     ObjectWithKey("price")->Number).
//
//  2. DetectSchemaDrift compares the current execution's per-tool
//     shape against a historical roll-up. The roll-up is built by
//     the caller (the handler) from a query of recent successful
//     tool_call events on the same project, same tool. Drift fires
//     when:
//     - At least minHistoryCalls historical calls exist (so we
//     have a baseline; ignore brand-new tools)
//     - The single most-common historical shape covers more than
//     majorityThreshold of those calls (so we have a STABLE
//     baseline; ignore tools that legitimately return
//     heterogeneous shapes)
//     - The current shape differs from that majority
//
//  3. The signature is "<tool_name>:<current_shape_hash[:8]>". One
//     failure_group per (tool, new shape) per project so SREs see
//     "tool foo returned a new shape" once per shape rollover, not
//     once per agent run that hit the new shape.
package detectors

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
)

const (
	// minHistoryCalls is the minimum number of historical successful
	// tool_call events required before the detector considers any
	// shape "established." Without this guard, every new tool that
	// returns a different shape on calls 2 and 3 would fire drift on
	// call 4. Ten calls is a conservative threshold for SaaS
	// production volume.
	minHistoryCalls = 10
	// majorityThreshold is the fraction of historical calls that
	// must share the dominant shape for the baseline to count as
	// "stable." Two-thirds is conservative enough to ignore tools
	// that legitimately return varied shapes (e.g. a generic web
	// fetcher) while still firing on tools with predominantly-stable
	// returns that suddenly change.
	majorityThreshold = 2.0 / 3.0
)

// ReturnShapeHash produces a stable SHA-256 fingerprint of a JSON
// value's structure. Values are stripped; only the type-level
// information (object key set + value types) survives.
//
// The output is hex-encoded SHA-256. The first 8 chars are used as
// the signature suffix (see DetectSchemaDrift), giving 32 bits of
// entropy which is enough to disambiguate shapes within a single
// tool's history.
//
// Returns "" when the input can't be parsed as JSON.
func ReturnShapeHash(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var v any
	if err := json.Unmarshal(raw, &v); err != nil {
		return ""
	}
	var b strings.Builder
	if err := writeShape(&b, v); err != nil {
		return ""
	}
	sum := sha256.Sum256([]byte(b.String()))
	return hex.EncodeToString(sum[:])
}

// writeShape walks the decoded JSON value and emits a structural
// fingerprint. Atomic types render as their type name; arrays render
// as "[<element-shape>]" (using the FIRST element's shape so
// heterogeneous arrays produce a stable hash); objects render as
// "{key:shape,key:shape,...}" with keys sorted alphabetically.
func writeShape(b *strings.Builder, v any) error {
	switch x := v.(type) {
	case nil:
		b.WriteString("null")
	case bool:
		b.WriteString("bool")
	case float64:
		// json.Unmarshal decodes all JSON numbers as float64; we
		// keep them homogeneous in the shape rather than splitting
		// int vs float. The point is the shape, not the magnitude.
		b.WriteString("number")
	case string:
		b.WriteString("string")
	case []any:
		b.WriteByte('[')
		if len(x) > 0 {
			// Use the first element as the array's element-shape.
			// Heterogeneous arrays (rare in well-defined APIs) still
			// hash deterministically.
			if err := writeShape(b, x[0]); err != nil {
				return err
			}
		}
		b.WriteByte(']')
	case map[string]any:
		// convention: the SDK ships typed sentinels for non-
		// JSON-native values, e.g.:
		//   {"__type__": "datetime", "value": "..."}
		//   {"__type__": "object", "class": "User", "value": "..."}
		// When the object carries a string __type__ marker, honor
		// the convention by emitting the LITERAL type value (and
		// `class` for object sentinels) into the shape. Without
		// this, every typed sentinel would collapse to the same
		// {__type__:string,value:string} shape and silently mask
		// the drift signal is designed to surface.
		if typeName, ok := x["__type__"].(string); ok {
			b.WriteString("<typed:")
			b.WriteString(typeName)
			if cls, hasCls := x["class"].(string); hasCls {
				b.WriteByte(':')
				b.WriteString(cls)
			}
			b.WriteByte('>')
			return nil
		}
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
			b.WriteString(k)
			b.WriteByte(':')
			if err := writeShape(b, x[k]); err != nil {
				return err
			}
		}
		b.WriteByte('}')
	default:
		return fmt.Errorf("tool_schema_drift: unsupported JSON node type %T", v)
	}
	return nil
}

// DetectSchemaDrift compares the current tool-call return shape
// against a historical roll-up. Returns (signature, true) when the
// current shape differs from a stable majority baseline. The caller
// is responsible for providing the historical map.
//
// historicalShapes is a map from shape_hash → number of times that
// shape has appeared in recent successful tool_call events for this
// (project, tool). The caller builds it by hashing
// payload.return_value from each historical row.
//
// Returns ("", false) when:
//   - currentShape is empty (couldn't hash, treat as inconclusive)
//   - historicalShapes is empty or below minHistoryCalls (no
//     baseline yet)
//   - no single historical shape exceeds majorityThreshold (the
//     tool's normal behavior is heterogeneous; drift is meaningless)
//   - the current shape matches the majority baseline
func DetectSchemaDrift(
	toolName string,
	currentShape string,
	historicalShapes map[string]int,
) (signature string, detected bool) {
	return DetectSchemaDriftWithThresholds(
		toolName, currentShape, historicalShapes,
		DefaultToolSchemaDriftThresholds(),
	)
}

// ToolSchemaDriftThresholds carries the per-project tunable values
// for this detector. MinHistoryCalls defaults to the
// hardcoded minHistoryCalls (10) for customers who don't tune.
type ToolSchemaDriftThresholds struct {
	MinHistoryCalls int
}

// DefaultToolSchemaDriftThresholds returns the historical hardcoded
// default. Used by legacy call sites and tests.
func DefaultToolSchemaDriftThresholds() ToolSchemaDriftThresholds {
	return ToolSchemaDriftThresholds{MinHistoryCalls: minHistoryCalls}
}

// DetectSchemaDriftWithThresholds is the per-project-aware variant.
// Defensive: MinHistoryCalls < 2 reverts to default (a single-call
// baseline cannot establish a majority; the validators registry
// rejects this at write time).
func DetectSchemaDriftWithThresholds(
	toolName string,
	currentShape string,
	historicalShapes map[string]int,
	t ToolSchemaDriftThresholds,
) (signature string, detected bool) {
	minHistory := t.MinHistoryCalls
	if minHistory < 2 {
		minHistory = minHistoryCalls
	}
	if currentShape == "" || toolName == "" {
		return "", false
	}
	total := 0
	dominantShape := ""
	dominantCount := 0
	for shape, count := range historicalShapes {
		total += count
		if count > dominantCount || (count == dominantCount && shape < dominantShape) {
			dominantShape = shape
			dominantCount = count
		}
	}
	if total < minHistory {
		return "", false
	}
	if float64(dominantCount)/float64(total) < majorityThreshold {
		return "", false
	}
	if dominantShape == currentShape {
		return "", false
	}
	// Fire: the current shape differs from a stable majority
	// baseline. Signature embeds the tool name AND the new shape's
	// prefix so different drift events (same tool, new shape A
	// today, new shape B tomorrow) cluster into separate groups.
	suffix := currentShape
	if len(suffix) > 8 {
		suffix = suffix[:8]
	}
	return fmt.Sprintf("%s:%s", toolName, suffix), true
}
