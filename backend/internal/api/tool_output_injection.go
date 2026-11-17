// Injected instructions arriving through tool output.
//
// The prompt scan (scanForInjection) reads what the customer's code
// sends the model: the user message and the system prompt. The more
// common attack today arrives the other way, inside something a tool
// fetched: a web page, a document, an email, an MCP server's answer,
// or the tool's own description. The model reads that text as part of
// its context, so instructions planted in it can steer the agent.
//
// At ingest, after secrets are redacted, each tool_call and mcp_call
// has its return value, error text and description scanned with the
// same injection rules (built-ins plus the project's own). A match
// writes an injection_scan_result event beside the call: which rule,
// which tool, which part, and a short passage around the match. The
// event type is reserved (events.IsBackendMinted), so a customer can
// neither forge one nor have a genuine one excluded from checks.
//
// When the run completes, scanForInjection reads those events in
// sequence order with the prompt matches, and the first one opens a
// prompt_injection group with signature "tool_output:<rule>" or
// "tool_description:<rule>", so the playbook can say where the attack
// came in.
//
// The per-project setting prompt_injection.scan_tool_output (default
// on) turns the scan off for a project whose agent routinely reads
// trusted text that quotes attack phrases.

package api

import (
	"context"
	"encoding/json"
	"time"
	"unicode/utf8"

	"github.com/mesedi-ai/mesedi/backend/attest/events"
	"mesedi/backend/internal/detectors"
)

// Scan layers written on injection_scan_result events.
const (
	injectionLayerToolReturn      = "tool_return"
	injectionLayerToolDescription = "tool_description"
)

// Signature prefixes for prompt_injection groups opened from tool
// output, keyed by scan layer.
var injectionLayerSignaturePrefix = map[string]string{
	injectionLayerToolReturn:      "tool_output:",
	injectionLayerToolDescription: "tool_description:",
}

// injectionExcerptRunes is how much text is kept on each side of the
// match. Enough to read the planted instruction in context, small
// enough that a whole fetched page is never copied into the alert.
const injectionExcerptRunes = 100

// toolOutputScanFields lists the parts of a call scanned, in order,
// with the layer each one is recorded under. The return value comes
// first because it is where fetched content arrives.
var toolOutputScanFields = []struct {
	key   string
	layer string
}{
	{"return_value", injectionLayerToolReturn},
	{"error", injectionLayerToolReturn},
	{"exception_message", injectionLayerToolReturn},
	{"tool_description", injectionLayerToolDescription},
}

// scanToolOutputForInjection is the ingest pass. It returns the batch
// with an injection_scan_result event after each call that matched,
// plus the pattern_ids of custom rules that fired, for match counts.
// Calls with no match, and every other event type, pass through
// unchanged and in order.
func (h *Handlers) scanToolOutputForInjection(
	ctx context.Context,
	projectID string,
	batch []events.Event,
) (newBatch []events.Event, customMatchedPatternIDs []string) {
	if !batchHasToolCalls(batch) {
		return batch, nil
	}
	if !h.toolOutputScanEnabled(ctx, projectID) {
		return batch, nil
	}
	custom, _ := h.loadCustomPatternsForDetector(ctx, projectID, "prompt_injection")

	var matched []string
	out := make([]events.Event, 0, len(batch))
	for i := range batch {
		evt := batch[i]
		out = append(out, evt)
		finding, ok := findInjectionInToolEvent(evt, custom)
		if !ok {
			continue
		}
		if finding.patternID != "" {
			matched = append(matched, finding.patternID)
		}
		sibling, err := buildInjectionScanResultEvent(evt, finding)
		if err != nil {
			h.Logger.Warn("tool output injection: build event failed",
				"parent_event_id", evt.EventID,
				"error", err.Error())
			continue
		}
		out = append(out, sibling)
	}
	return out, matched
}

func batchHasToolCalls(batch []events.Event) bool {
	for i := range batch {
		switch batch[i].EventType {
		case events.EventTypeToolCall, events.EventTypeMCPCall:
			return true
		}
	}
	return false
}

// toolOutputScanEnabled reads the one setting this pass needs, rather
// than every detector's, because it runs on every ingest batch. A
// read or parse failure keeps the default (on): failing open here
// would silently stop a security check.
func (h *Handlers) toolOutputScanEnabled(ctx context.Context, projectID string) bool {
	enabled := detectors.DefaultPromptInjectionThresholds().ScanToolOutput
	rows, err := h.Store.ListProjectDetectorThresholds(ctx, projectID, "prompt_injection")
	if err != nil {
		h.Logger.Warn("tool output injection: setting read failed; scanning",
			"project_id", projectID, "error", err.Error())
		return enabled
	}
	for _, row := range rows {
		if row.ThresholdKey != "scan_tool_output" {
			continue
		}
		spec, ok := LookupDetectorThresholdSpec("prompt_injection", row.ThresholdKey)
		if !ok {
			continue
		}
		v, perr := spec.Parse(row.ValueJSON, "")
		if perr != nil {
			h.Logger.Warn("tool output injection: setting unreadable; scanning",
				"project_id", projectID, "error", perr.Error())
			continue
		}
		if b, ok := v.(bool); ok {
			enabled = b
		}
	}
	return enabled
}

type toolInjectionFinding struct {
	layer     string
	signature string
	patternID string
	source    string
	excerpt   string
}

// findInjectionInToolEvent scans one tool_call or mcp_call. The first
// field in toolOutputScanFields order with a match wins.
func findInjectionInToolEvent(
	evt events.Event,
	custom []*detectors.CustomPattern,
) (toolInjectionFinding, bool) {
	if evt.EventType != events.EventTypeToolCall && evt.EventType != events.EventTypeMCPCall {
		return toolInjectionFinding{}, false
	}
	if len(evt.Payload) == 0 {
		return toolInjectionFinding{}, false
	}
	var pm map[string]any
	if err := json.Unmarshal(evt.Payload, &pm); err != nil {
		return toolInjectionFinding{}, false
	}
	for _, f := range toolOutputScanFields {
		text := fieldText(pm[f.key])
		sig, pid, start, end, fired := detectors.DetectInjectionSpan(text, custom)
		if !fired {
			continue
		}
		return toolInjectionFinding{
			layer:     f.layer,
			signature: sig,
			patternID: pid,
			source:    toolSourceName(evt.EventType, pm),
			excerpt:   excerptAround(text, start, end, injectionExcerptRunes),
		}, true
	}
	return toolInjectionFinding{}, false
}

// fieldText turns a payload value into the text the model would have
// read: strings as they are, structured values as their JSON.
func fieldText(v any) string { // any: a decoded JSON value of whatever shape the payload held
	switch t := v.(type) {
	case nil:
		return ""
	case string:
		return t
	default:
		b, err := json.Marshal(t)
		if err != nil {
			return ""
		}
		return string(b)
	}
}

// toolSourceName names the tool the way every tool check does: the
// tool name, or "server/method" for an MCP call (see the tool_events
// view, migration 065).
func toolSourceName(t events.EventType, pm map[string]any) string {
	str := func(k string) string {
		s, _ := pm[k].(string)
		return s
	}
	if t == events.EventTypeMCPCall {
		return str("server_name") + "/" + str("method")
	}
	return str("tool_name")
}

// excerptAround returns the match with up to n runes either side,
// marked with "..." where text was cut. Byte offsets from the regexp
// are moved to rune boundaries so the excerpt is always valid UTF-8.
func excerptAround(text string, start, end, n int) string {
	if start < 0 || end > len(text) || start > end {
		return ""
	}
	from := start
	for i := 0; i < n && from > 0; i++ {
		_, size := utf8.DecodeLastRuneInString(text[:from])
		from -= size
	}
	to := end
	for i := 0; i < n && to < len(text); i++ {
		_, size := utf8.DecodeRuneInString(text[to:])
		to += size
	}
	out := text[from:to]
	if from > 0 {
		out = "..." + out
	}
	if to < len(text) {
		out += "..."
	}
	return out
}

// buildInjectionScanResultEvent writes the finding beside its call,
// the same way the DLP sibling is written: same execution and
// sequence, a microsecond later, a fresh event_id.
func buildInjectionScanResultEvent(parent events.Event, f toolInjectionFinding) (events.Event, error) {
	raw, err := json.Marshal(events.InjectionAlertPayload{
		ScanLayer:      f.layer,
		SignatureMatch: f.signature,
		Confidence:     1,
		Action:         "alerted",
		Excerpt:        f.excerpt,
		Source:         f.source,
		ParentEventID:  parent.EventID,
	})
	if err != nil {
		return events.Event{}, err
	}
	return events.Event{
		EventID:     newDLPEventID(),
		ExecutionID: parent.ExecutionID,
		EventType:   events.EventTypeInjectionScanResult,
		Sequence:    parent.Sequence,
		Timestamp:   parent.Timestamp.Add(2 * time.Microsecond),
		Payload:     raw,
	}, nil
}

// toolOutputInjectionSignature turns a recorded injection_scan_result
// into a prompt_injection group signature. ok is false for an event
// that is not one, or carries no rule name.
func toolOutputInjectionSignature(e *events.Event) (signature string, ok bool) {
	if e == nil || e.EventType != events.EventTypeInjectionScanResult || len(e.Payload) == 0 {
		return "", false
	}
	var p events.InjectionAlertPayload
	if err := json.Unmarshal(e.Payload, &p); err != nil || p.SignatureMatch == "" {
		return "", false
	}
	prefix, known := injectionLayerSignaturePrefix[p.ScanLayer]
	if !known {
		prefix = injectionLayerSignaturePrefix[injectionLayerToolReturn]
	}
	return prefix + p.SignatureMatch, true
}
