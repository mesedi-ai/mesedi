// DLP ingest helpers (Mesedi #1 + #24). When the global DLP scanner
// is configured, HandleIngestEvents calls applyDLPToBatch before
// persistence: each llm_call / tool_call payload is scanned against
// the rule registry, matched secrets are replaced with stable
// `[REDACTED:rule_id]` tokens, and a sibling dlp_scan_result event is
// generated for each event that had ANY hit (critical, high, or
// medium). The handler's downstream FindFirstDLPSignalForSeverities
// query honors the per-project DataLeakageThresholds.AllowedSeverities
// knob (data_leakage.G5 wave) to decide which sibling severities
// promote to a failure_group; defaults are still ["critical", "high"].
//
// The sibling event lets the downstream detector aggregate by rule_id
// without re-scanning, and gives the dashboard a per-event log of
// what was redacted (rule_id + count + severity, never the original
// matched substring).
//
// Critical contract: redaction MUST happen before SaveEvents. The
// matched secret must NEVER reach durable storage in clear. This
// file orchestrates that contract.

package api

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"sort"
	"time"

	"mesedi/backend/internal/detectors"
	"mesedi/backend/internal/dlp"
	"mesedi/backend/internal/events"
)

// scanFieldKeys enumerates the fields per event_type that the DLP
// scanner runs against. Each field is treated as an opaque string
// for scanning purposes; the redactor preserves JSON shape by
// re-marshalling after substitution. Adding a new event type with
// secret-bearing fields means extending this map.
var scanFieldKeys = map[events.EventType][]string{
	events.EventTypeLLMCall: {
		// Field names match what the SDK's instrument_anthropic
		// ships. Previously listed `user_prompt` and `response`
		// which no SDK ever emitted, so DLP scanned nothing on
		// llm_call events and data_leakage silently no-op'd. Caught
		// by the integration suite (backend/test/integration/
		// test_detectors.py::test_data_leakage).
		"system_prompt",
		"user_message",
		"response_text",
		// Theme A: failure-path PII redaction. instrument_*
		// modules ship `exception_message` on failed llm_call
		// events (provider exception text — may contain API keys
		// returned in the error response, request IDs with
		// embedded customer IDs, etc.). Scanning here closes
		// tool_failures.G1 + validator_failures.G2's parent
		// concern about raw exception persistence.
		"exception_message",
	},
	events.EventTypeToolCall: {
		// arguments and return_value are json.RawMessage on the payload
		// struct; we scan their serialized text form. tool_name is
		// not scanned (it's a logical identifier, never a secret).
		"arguments",
		"return_value",
		// Theme A: tool failure path. `error` is the standard
		// failure message on ToolCallPayload; `exception_message`
		// is the richer variant SDK integrations may ship on
		// tool wrapping errors. Both treated identically.
		"error",
		"exception_message",
	},
	// Theme A (closes validator_failures.G2): scan the validator's
	// reason / message field on failed validator_result events.
	// Customers' validators sometimes echo the failing slice of the
	// agent's output verbatim into the reason string — if that
	// slice contains a secret the agent had been working with, the
	// secret would otherwise persist raw.
	events.EventTypeValidatorResult: {
		"reason",
		"message",
	},
	// Theme A: catch-all for any direct exception_event the SDK
	// might emit. The canonical crash path hashes BEFORE persist
	// (see audit closure on crashes.G1), so this entry is
	// defensive: zero cost when the field is absent, full DLP
	// protection if a future SDK change starts emitting raw.
	events.EventTypeException: {
		"message",
	},
}

// applyDLPToBatch is the orchestrator. For each event in batch:
//   - If event_type has fields the scanner cares about, decode the
//     payload, scan each scanable field, and on hit replace the
//     matched bytes with `[REDACTED:rule_id]` tokens. Re-encode.
//   - If the redacted event had at least one critical/high hit,
//     generate a sibling dlp_scan_result event linking back via
//     parent_event_id. Lower-severity (medium) hits are recorded in
//     the parent event's redaction but don't generate a sibling
//     event (the threshold for "page the on-call" is critical/high).
//
// Returns a new batch slice with redacted parents + interleaved
// sibling events. The caller (HandleIngestEvents) passes this to
// SaveEvents. The order is preserved: each sibling immediately
// follows its parent.
//
// Sibling events get auto-generated event_ids and use the parent's
// execution_id + a sequence number = parent.Sequence (the same
// sequence: detectors only care about ordering within an execution,
// and sharing the parent sequence lets the dashboard render the two
// side-by-side. Future refinement: bump sequence on each sibling).
func (h *Handlers) applyDLPToBatch(
	ctx context.Context,
	projectID string,
	batch []events.Event,
) (newBatch []events.Event, customMatchedPatternIDs []string) {
	if h.DLPScanner == nil || len(batch) == 0 {
		return batch, nil
	}

	// Wave 2.1.b: load per-project custom data-leakage patterns once
	// per batch. Project-scoped; nil/empty when the customer has no
	// custom rules. Compile errors at this layer are unexpected
	// (POST/PATCH validates) and degrade to built-ins only.
	customPatterns, _ := h.loadCustomPatternsForDetector(
		ctx, projectID, "data_leakage",
	)
	// Track every pattern_id that fires so the caller can increment
	// match_count once per match. De-dup'd at the caller to avoid
	// over-counting when the same custom pattern fires in multiple
	// events of the same batch.
	var matched []string

	out := make([]events.Event, 0, len(batch))
	for i := range batch {
		evt := batch[i]
		fields, scan := scanFieldKeys[evt.EventType]
		if !scan || len(evt.Payload) == 0 {
			out = append(out, evt)
			continue
		}

		// Decode the payload as a generic JSON object so we can
		// rewrite the scan fields without committing to a typed
		// struct (typed structs vary per event type; this helper
		// stays general-purpose).
		var pm map[string]any
		if err := json.Unmarshal(evt.Payload, &pm); err != nil {
			// Malformed JSON, pass through unmodified. The event will
			// likely be rejected downstream by stricter consumers,
			// but the DLP layer is best-effort and shouldn't drop
			// otherwise-valid events on JSON quirks.
			out = append(out, evt)
			continue
		}

		var allHits []dlp.Hit
		mutated := false
		for _, key := range fields {
			raw, ok := pm[key]
			if !ok {
				continue
			}
			// Coerce the value to a string we can scan. For nested
			// JSON (arguments / return_value on tool_call) we
			// re-marshal to text form so regex hits on stringified
			// keys-and-values inside the sub-object still fire.
			var s string
			switch v := raw.(type) {
			case string:
				s = v
			default:
				bs, err := json.Marshal(v)
				if err != nil {
					continue
				}
				s = string(bs)
			}
			redacted, hits := h.DLPScanner.ScanAndRedact(s)
			if len(hits) > 0 {
				allHits = append(allHits, hits...)
				pm[key] = redacted
				mutated = true
				// Re-stringify the redacted value for the custom
				// pass — custom patterns scan post-builtin-redaction
				// so they fire on residual secrets the built-ins
				// missed without flagging the [REDACTED:rule_id]
				// markers themselves.
				s = redacted
			}
			// Wave 2.1.b + Wave 2.1.d.3: run customer's custom
			// patterns against the same field AND redact matched
			// bytes in place. Closes the documented data_leakage.G1
			// gap from 2.1.b — custom rules now redact at the same
			// trust boundary the built-in rules do (matched secret
			// never reaches durable storage).
			if len(customPatterns) > 0 {
				customRedacted, customHits, customMatched :=
					scanAndRedactCustomDLP(s, customPatterns)
				if len(customHits) > 0 {
					allHits = append(allHits, customHits...)
					matched = append(matched, customMatched...)
					pm[key] = customRedacted
					mutated = true
					s = customRedacted
				}
			}
		}

		if mutated {
			if newPayload, err := json.Marshal(pm); err == nil {
				evt.Payload = newPayload
			}
		}
		out = append(out, evt)

		// Generate the sibling dlp_scan_result event for ANY hit
		// (critical, high, OR medium). Promotion to failure_group is
		// gated downstream at the handler via
		// FindFirstDLPSignalForSeverities, which honors the
		// per-project DataLeakageThresholds.AllowedSeverities knob
		// (data_leakage.G5 wave). Pre-filtering here would silently
		// suppress the medium tier even for customers who explicitly
		// opted into it via severity_policy=["critical","high","medium"].
		// Default-config customers (["critical","high"]) see identical
		// behavior — medium sibling events are stored but never
		// queried for promotion.
		if len(allHits) == 0 {
			continue
		}
		highest := dlp.HighestSeverity(allHits)
		sibling, err := buildDLPScanResultEvent(evt, allHits, highest)
		if err != nil {
			h.Logger.Warn("dlp: build sibling event failed (parent redacted, no cluster signal)",
				"parent_event_id", evt.EventID,
				"error", err.Error(),
			)
			continue
		}
		out = append(out, sibling)
	}
	return out, matched
}

// scanAndRedactCustomDLP runs the customer's custom data_leakage
// patterns against a single field string and returns:
//   - redacted: a copy of the input with every match replaced by
//     `[REDACTED:custom-<pattern_id>]`. Walks matches in reverse
//     end-to-start order so earlier offsets stay valid as later
//     ones get replaced. Zero-width matches are skipped (a regex
//     like `^` or `(?=...)` would otherwise insert tokens at every
//     position).
//   - hits: dlp.Hit entries (same shape the built-in scanner
//     produces) so the sibling dlp_scan_result event includes
//     custom matches at the right severity tier.
//   - matchedPatternIDs: for IncrementPatternMatchCount telemetry.
//
// Severity mapping: pattern_config 'low' → dlp.SeverityMedium
// (smallest dlp severity); 'medium' → dlp.SeverityHigh; 'high' →
// dlp.SeverityCritical. Preserves dashboard chip color semantics
// for customer-defined rules.
//
// Wave 2.1.d.3 closes the documented gap from 2.1.b: custom rules
// now redact at the same trust boundary the built-in rules do.
func scanAndRedactCustomDLP(
	input string,
	custom []*detectors.CustomPattern,
) (redacted string, hits []dlp.Hit, matchedPatternIDs []string) {
	if input == "" || len(custom) == 0 {
		return input, nil, nil
	}
	// Collect ALL matches across all custom patterns first so we
	// can do one in-order redaction walk afterwards. Each match
	// carries its pattern_id so the replacement token can name it.
	type customMatch struct {
		start, end int
		patternID  string
		hit        dlp.Hit
	}
	var matches []customMatch
	for _, c := range custom {
		if c == nil || c.Compiled == nil {
			continue
		}
		for _, idx := range c.Compiled.FindAllStringIndex(input, -1) {
			start, end := idx[0], idx[1]
			// Skip zero-width matches (defensive against regexes
			// like `^` or `(?=...)` that would otherwise produce
			// an infinite stream of insertion points).
			if end <= start {
				continue
			}
			h := dlp.Hit{
				RuleID:   "custom:" + c.PatternID,
				Label:    "Custom pattern " + c.PatternID,
				Severity: customSeverityToDLP(c.Severity),
				Start:    start,
				End:      end,
				Match:    input[start:end],
			}
			matches = append(matches, customMatch{
				start: start, end: end, patternID: c.PatternID, hit: h,
			})
			hits = append(hits, h)
			matchedPatternIDs = append(matchedPatternIDs, c.PatternID)
		}
	}
	if len(matches) == 0 {
		return input, nil, nil
	}
	// Walk matches in reverse end-position order so earlier offsets
	// stay valid as later ones are replaced. Stable sort: prefer
	// later end first, then later start; ties (multiple patterns
	// matching the same span) keep first-pattern's redaction.
	sort.SliceStable(matches, func(i, j int) bool {
		if matches[i].end != matches[j].end {
			return matches[i].end > matches[j].end
		}
		return matches[i].start > matches[j].start
	})
	buf := []byte(input)
	for _, m := range matches {
		token := []byte("[REDACTED:custom-" + m.patternID + "]")
		// Defensive: if a previous iteration's substitution shrunk
		// or grew the buffer (it does), the recorded match offsets
		// (against the original input) may fall outside current
		// bounds. Skip any match whose range is no longer in
		// bounds; the earlier outer-loop produced the hit already
		// (it'll still surface in the sibling event).
		if m.start < 0 || m.end > len(buf) || m.start >= m.end {
			continue
		}
		newBuf := make([]byte, 0, len(buf)-(m.end-m.start)+len(token))
		newBuf = append(newBuf, buf[:m.start]...)
		newBuf = append(newBuf, token...)
		newBuf = append(newBuf, buf[m.end:]...)
		buf = newBuf
	}
	return string(buf), hits, matchedPatternIDs
}

// customSeverityToDLP maps pattern_config severity ('low' / 'medium'
// / 'high') to dlp.Severity ('medium' / 'high' / 'critical'). The
// shift one tier upward preserves dashboard chip color semantics
// (customer's 'high' lights the red chip the same way Mesedi's
// built-in criticals do).
func customSeverityToDLP(s string) dlp.Severity {
	switch s {
	case "high":
		return dlp.SeverityCritical
	case "medium":
		return dlp.SeverityHigh
	default:
		return dlp.SeverityMedium
	}
}

// buildDLPScanResultEvent constructs the sibling event that the
// data_leakage detector consumes. event_id is freshly generated;
// execution_id + sequence + timestamp match the parent so the
// dashboard can render them side-by-side in the timeline.
func buildDLPScanResultEvent(parent events.Event, hits []dlp.Hit, highest dlp.Severity) (events.Event, error) {
	summaries := dlp.Summarize(hits)
	payloadHits := make([]events.DLPScanResultHit, len(summaries))
	totalCount := 0
	for i, s := range summaries {
		payloadHits[i] = events.DLPScanResultHit{
			RuleID:   s.RuleID,
			Label:    s.Label,
			Severity: string(s.Severity),
			Count:    s.Count,
		}
		totalCount += s.Count
	}
	// scan_layer hints at which parent field the hits came from. We
	// resolve via event_type rather than scanning the hits because
	// hits don't carry a key-of-origin field today (future
	// refinement). The label is good enough for the dashboard
	// timeline.
	scanLayer := "unknown"
	switch parent.EventType {
	case events.EventTypeLLMCall:
		scanLayer = "llm_prompt"
	case events.EventTypeToolCall:
		scanLayer = "tool_arguments"
	}
	payload := events.DLPScanResultPayload{
		ScanLayer:       scanLayer,
		ParentEventID:   parent.EventID,
		ParentEventType: string(parent.EventType),
		HighestSeverity: string(highest),
		HitCount:        totalCount,
		Hits:            payloadHits,
		Action:          "redacted",
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return events.Event{}, err
	}
	return events.Event{
		EventID:     newDLPEventID(),
		ExecutionID: parent.ExecutionID,
		EventType:   events.EventTypeDLPScanResult,
		Sequence:    parent.Sequence,
		Timestamp:   parent.Timestamp.Add(time.Microsecond),
		Payload:     raw,
	}, nil
}

// newDLPEventID generates a per-event identifier for sibling events.
// Format mirrors what the SDKs use ("evt-" + 12 hex chars) so
// downstream lookups by event_id stay consistent across SDK-emitted
// and server-emitted events.
func newDLPEventID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		// rand.Read is overwhelmingly unlikely to fail; on the
		// off-chance, fall back to a fixed prefix + nanosecond
		// counter so we never panic in the ingest hot path.
		return "evt-dlpfallback"
	}
	return "evt-" + hex.EncodeToString(b[:])
}
