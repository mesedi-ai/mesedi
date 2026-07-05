// Scanner implementation for the DLP package. A Scanner is a
// compiled, immutable view of a Rule slice that runs in O(n × len(s))
// against a target string, returning Hit records describing each
// match (rule id, severity, and the byte offset of the match for
// downstream redaction).
//
// Threading: a Scanner is safe for concurrent use. Internal state
// is the compiled *regexp.Regexp slice; Go's regexp.Regexp itself is
// concurrency-safe per docs.
//
// Design choices:
//
//  1. The scanner runs every enabled rule against the whole input,
//     not the first match. We need every hit so the redactor can
//     replace each independently and so the dashboard can report
//     "this run had AWS key + JWT" rather than just "the first
//     match was AWS key."
//
//  2. Redaction uses a stable token format `[REDACTED:rule_id]` so
//     downstream consumers (the SDK, the dashboard, the detector)
//     can recognize redacted content without inspecting the original
//     event payload. The token format intentionally preserves
//     character-level alignment with the original to the extent
//     possible — see Redact() for the tradeoffs.

package dlp

import (
	"fmt"
	"regexp"
	"sort"
	"strings"
)

// Hit is one detected secret in a scanned string. The Start/End
// offsets are byte offsets into the scanned input and are kept
// non-overlapping by the scanner's merge step.
type Hit struct {
	RuleID   string
	Label    string
	Severity Severity
	Start    int
	End      int
	// Match is the raw matched substring. NEVER persist this directly,
	// the whole point of DLP is that this string never enters durable
	// storage; the scanner returns it so the caller can compute a
	// content hash for clustering (length-stable, irreversible).
	Match string
}

// Scanner is a compiled, immutable rule set. Construct with
// NewScanner; do not modify after construction. Calls to Scan and
// Redact are safe for concurrent use.
type Scanner struct {
	rules    []Rule
	compiled []*regexp.Regexp
}

// NewScanner builds a Scanner from the given rules. Each rule's
// pattern is compiled once; a compilation error is returned with the
// offending rule's ID so the caller can surface a meaningful startup
// error.
//
// Pass nil to use the built-in default rule set. Callers wanting to
// disable specific rules should pass BuiltinRules() filtered to the
// rules they want, rather than mutating the result.
func NewScanner(rules []Rule) (*Scanner, error) {
	if rules == nil {
		rules = BuiltinRules()
	}
	s := &Scanner{
		rules:    make([]Rule, len(rules)),
		compiled: make([]*regexp.Regexp, len(rules)),
	}
	for i, r := range rules {
		if r.Pattern == "" {
			return nil, fmt.Errorf("dlp scanner: rule %q has empty pattern", r.ID)
		}
		re, err := regexp.Compile(r.Pattern)
		if err != nil {
			return nil, fmt.Errorf("dlp scanner: rule %q failed to compile: %w", r.ID, err)
		}
		s.rules[i] = r
		s.compiled[i] = re
	}
	return s, nil
}

// Scan runs every rule against the input and returns all matches.
// Hits are sorted by Start offset ascending so the redactor can walk
// them in order. Overlapping hits from different rules are merged
// into the longer one (rare in practice but matters for nested
// patterns like JWT-inside-Authorization-header).
func (s *Scanner) Scan(input string) []Hit {
	if input == "" {
		return nil
	}
	var hits []Hit
	for i, re := range s.compiled {
		rule := s.rules[i]
		for _, idx := range re.FindAllStringIndex(input, -1) {
			start, end := idx[0], idx[1]
			hits = append(hits, Hit{
				RuleID:   rule.ID,
				Label:    rule.Label,
				Severity: rule.Severity,
				Start:    start,
				End:      end,
				Match:    input[start:end],
			})
		}
	}
	if len(hits) <= 1 {
		return hits
	}
	sort.Slice(hits, func(i, j int) bool {
		if hits[i].Start != hits[j].Start {
			return hits[i].Start < hits[j].Start
		}
		// Longer match first when starts tie; the merge step will
		// drop the shorter overlapping one.
		return hits[i].End > hits[j].End
	})
	return mergeOverlapping(hits)
}

// mergeOverlapping collapses overlapping Hit ranges, keeping the
// longer / earlier match. Called only when len(hits) >= 2.
func mergeOverlapping(hits []Hit) []Hit {
	out := make([]Hit, 0, len(hits))
	out = append(out, hits[0])
	for _, h := range hits[1:] {
		last := &out[len(out)-1]
		if h.Start < last.End {
			// Overlaps the previous; keep the longer one. If
			// neither dominates (same range, different rule),
			// prefer the one already in `out` so iteration is
			// deterministic.
			if h.End > last.End {
				last.End = h.End
				last.Match = h.Match
				last.RuleID = h.RuleID
				last.Label = h.Label
				last.Severity = h.Severity
			}
			continue
		}
		out = append(out, h)
	}
	return out
}

// Redact returns a copy of input with each Hit replaced by a stable
// `[REDACTED:rule_id]` token. The token length is independent of the
// original match length, so callers SHOULD NOT rely on the redacted
// string having identical byte offsets to the original. When you need
// the original offsets, pass them through alongside the redacted
// string.
//
// Hits MUST be the result of a prior Scan on the same input (the
// function trusts the offsets unconditionally).
func (s *Scanner) Redact(input string, hits []Hit) string {
	if len(hits) == 0 {
		return input
	}
	var b strings.Builder
	b.Grow(len(input))
	cursor := 0
	for _, h := range hits {
		if h.Start < cursor {
			// Defensive: skip overlapping hits the caller didn't
			// merge. Should not happen after Scan().
			continue
		}
		b.WriteString(input[cursor:h.Start])
		b.WriteString("[REDACTED:")
		b.WriteString(h.RuleID)
		b.WriteString("]")
		cursor = h.End
	}
	if cursor < len(input) {
		b.WriteString(input[cursor:])
	}
	return b.String()
}

// ScanAndRedact is the all-in-one helper for the common path: scan,
// then redact, then return both the redacted string and a summary of
// the hits for downstream observability.
//
// The returned Hits keep their Start/End offsets pointing at the
// ORIGINAL input, not the redacted one. The Match field is filled
// from the original input as well, the caller is expected to discard
// it before the data leaves a security boundary (or to substitute a
// hash if they need to dedupe).
func (s *Scanner) ScanAndRedact(input string) (redacted string, hits []Hit) {
	hits = s.Scan(input)
	redacted = s.Redact(input, hits)
	return redacted, hits
}

// HitSummary is a per-rule rollup of the Hit slice, suitable for
// inclusion in a dlp_scan_result event payload. The Match field
// from each Hit is intentionally dropped, only the rule_id and the
// number of hits propagate to the event store.
type HitSummary struct {
	RuleID   string   `json:"rule_id"`
	Label    string   `json:"label"`
	Severity Severity `json:"severity"`
	Count    int      `json:"count"`
}

// Summarize rolls a Hit slice up by rule_id for inclusion in the
// dlp_scan_result event payload. The returned slice is sorted by
// rule_id alphabetically so the event payload is deterministic and
// equality-comparable across runs.
func Summarize(hits []Hit) []HitSummary {
	if len(hits) == 0 {
		return nil
	}
	byRule := map[string]*HitSummary{}
	for _, h := range hits {
		if s, ok := byRule[h.RuleID]; ok {
			s.Count++
			continue
		}
		byRule[h.RuleID] = &HitSummary{
			RuleID:   h.RuleID,
			Label:    h.Label,
			Severity: h.Severity,
			Count:    1,
		}
	}
	out := make([]HitSummary, 0, len(byRule))
	for _, s := range byRule {
		out = append(out, *s)
	}
	sort.Slice(out, func(i, j int) bool {
		return out[i].RuleID < out[j].RuleID
	})
	return out
}

// HighestSeverity returns the most severe Severity present in the
// hits slice. Returns "" when hits is empty. Severity ordering:
// critical > high > medium. Used by the detector to decide whether to
// fire the data_leakage failure_group at all (medium-only hits are
// recorded but don't cluster).
func HighestSeverity(hits []Hit) Severity {
	highest := Severity("")
	for _, h := range hits {
		switch h.Severity {
		case SeverityCritical:
			return SeverityCritical
		case SeverityHigh:
			highest = SeverityHigh
		case SeverityMedium:
			if highest == "" {
				highest = SeverityMedium
			}
		}
	}
	return highest
}
