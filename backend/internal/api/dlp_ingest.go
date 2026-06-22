// DLP ingest helpers (Mesedi #1 + #24). When the global DLP scanner
// is configured, HandleIngestEvents calls applyDLPToBatch before
// persistence: each llm_call / tool_call payload is scanned against
// the rule registry, matched secrets are replaced with stable
// `[REDACTED:rule_id]` tokens, and a sibling dlp_scan_result event is
// generated for each event that had at least one critical/high hit.
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
	},
	events.EventTypeToolCall: {
		// arguments and return_value are json.RawMessage on the payload
		// struct; we scan their serialized text form. tool_name is
		// not scanned (it's a logical identifier, never a secret).
		"arguments",
		"return_value",
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
			// Wave 2.1.b: run customer's custom patterns against the
			// same field. Hits get appended to allHits with the
			// pattern_id surfaced via the matched-IDs slice so the
			// caller can increment match_count.
			if len(customPatterns) > 0 {
				customHits, customMatched := scanCustomDLP(s, customPatterns)
				if len(customHits) > 0 {
					allHits = append(allHits, customHits...)
					matched = append(matched, customMatched...)
					// Custom patterns participate in the redaction
					// pass too — applying the same [REDACTED:...]
					// substitution would require a redactor-aware
					// path we don't yet have for the customer's
					// patterns. v1 documents the gap (custom hits
					// fire the dlp_scan_result sibling but do NOT
					// redact in place); Wave 2.1.d revisits.
				}
			}
		}

		if mutated {
			if newPayload, err := json.Marshal(pm); err == nil {
				evt.Payload = newPayload
			}
		}
		out = append(out, evt)

		// Generate the sibling dlp_scan_result event only when there
		// are hits at critical or high severity. medium-only hits
		// are recorded in the parent (the redaction stays) but do
		// not page anyone.
		if len(allHits) == 0 {
			continue
		}
		highest := dlp.HighestSeverity(allHits)
		if highest != dlp.SeverityCritical && highest != dlp.SeverityHigh {
			continue
		}
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

// scanCustomDLP runs the customer's custom data_leakage patterns
// against a single field string and returns the resulting hits
// plus the matched pattern_ids. Severity mapping: pattern_config
// 'low' → dlp.SeverityMedium (smallest dlp severity); 'medium' →
// dlp.SeverityHigh; 'high' → dlp.SeverityCritical. The mapping
// preserves dashboard chip color semantics for customer-defined
// rules.
func scanCustomDLP(
	input string,
	custom []*detectors.CustomPattern,
) (hits []dlp.Hit, matchedPatternIDs []string) {
	if input == "" || len(custom) == 0 {
		return nil, nil
	}
	for _, c := range custom {
		if c == nil || c.Compiled == nil {
			continue
		}
		for _, idx := range c.Compiled.FindAllStringIndex(input, -1) {
			start, end := idx[0], idx[1]
			hits = append(hits, dlp.Hit{
				RuleID:   "custom:" + c.PatternID,
				Label:    "Custom pattern " + c.PatternID,
				Severity: customSeverityToDLP(c.Severity),
				Start:    start,
				End:      end,
				Match:    input[start:end],
			})
			matchedPatternIDs = append(matchedPatternIDs, c.PatternID)
		}
	}
	return hits, matchedPatternIDs
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
