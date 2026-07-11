// End-to-end wire-capture tests for the receiver-specific adapters.
//
// Complements adapters_test.go (which unit-tests the Build*Body pure
// functions in isolation). This file drives the FULL Deliver()
// pipeline — URL routing → adapter selection → HMAC decision → HTTP
// request assembly → wire bytes on the outbound side — against three
// mock destinations, and asserts the exact bytes each receiver would
// see. No network, no credentials, no live Slack / PagerDuty / Discord
// account required.
//
// The trick: inject an http.Client whose Transport is a capturing
// RoundTripper. When Deliver() calls httpClient.Do(req), the transport
// records the request, returns a synthetic 200 OK, and never touches
// the network. The URL still matches Slack/Discord/PagerDuty's regex
// so the URL-detection + adapter-routing path fires exactly as it
// would in production.
//
// What each destination assertion covers, at a glance:
//
//	Slack       — body is Block Kit (blocks[]: header, section, context);
//	              Content-Type application/json; X-Mesedi-Signature present
//	              (HMAC over the ADAPTED body, not the canonical Payload).
//	PagerDuty   — body has routing_key = webhook.AuthToken (NOT Secret),
//	              event_action = "trigger", dedup_key stable per
//	              (project_id, group_id) for real events and per
//	              delivery_id for test events (so the "Test webhook"
//	              button can never merge into an active on-call incident),
//	              severity clamped to PagerDuty's 4-value vocabulary;
//	              X-Mesedi-Signature ABSENT per AdapterSkipsHMAC.
//	Discord     — body has embeds[] with title, description, color as
//	              a decimal int (Discord rejects hex strings), footer
//	              carrying the delivery_id; X-Mesedi-Signature present.
//
// If any of these break, the resulting Slack message renders as raw
// JSON, the PagerDuty event 400s with "invalid routing_key", or the
// Discord embed silently drops. All three are customer-visible
// regressions — this file is the wall between them and prod.

package webhooks

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"testing"
	"time"

	"mesedi/backend/internal/store"
)

// capturedRequest records a single outbound HTTP request without
// letting it hit the network. The RoundTripper below appends one of
// these per request into a slice the test can inspect after Deliver
// returns.
type capturedRequest struct {
	method  string
	url     string
	headers http.Header
	body    []byte
}

// captureTransport implements http.RoundTripper by (a) fully reading
// the request body, (b) cloning headers, (c) recording both, and (d)
// returning a synthetic 200 OK "{}" response.
//
// The synthetic response uses a fresh Body reader per call so the
// dispatcher's io.Copy(io.Discard, resp.Body) doesn't consume shared
// state between calls (would break the retry path if a real 5xx were
// simulated; kept clean here for future extension).
type captureTransport struct {
	requests   []capturedRequest
	respStatus int    // 200 by default; test can override to exercise retry
	respBody   string // "" defaults to `{}`
}

func (c *captureTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		if err != nil {
			return nil, err
		}
		body = b
	}
	c.requests = append(c.requests, capturedRequest{
		method:  req.Method,
		url:     req.URL.String(),
		headers: req.Header.Clone(),
		body:    body,
	})
	status := c.respStatus
	if status == 0 {
		status = http.StatusOK
	}
	respBody := c.respBody
	if respBody == "" {
		respBody = "{}"
	}
	return &http.Response{
		StatusCode: status,
		Status:     http.StatusText(status),
		Body:       io.NopCloser(bytes.NewReader([]byte(respBody))),
		Header:     make(http.Header),
		Request:    req,
	}, nil
}

// newTestClient returns an http.Client whose transport captures every
// request. The client has a short timeout so a bug that skipped the
// capture path (real DNS) would fail fast rather than hang the test.
func newTestClient() (*http.Client, *captureTransport) {
	tr := &captureTransport{}
	return &http.Client{
		Transport: tr,
		Timeout:   2 * time.Second,
	}, tr
}

// newRealPayload returns a canonical failure_group.created payload
// with every optional field populated. Individual tests copy this and
// mutate one field so each assertion isolates one axis of variation.
func newRealPayload() Payload {
	return Payload{
		Version:           "1",
		Event:             "failure_group.created",
		Test:              false,
		ProjectID:         "proj_wireshape",
		WebhookID:         "wh_wireshape",
		GroupID:           "grp_semantic_loop_abc",
		FailureClass:      "semantic_loop",
		Severity:          "critical",
		Signature:         "checkpoint_repeat:planner:review_step",
		SampleExecutionID: "exec_wireshape_001",
		DashboardURL:      "https://app.mesedi.ai",
		PlaybookURL:       "https://app.mesedi.ai/app/playbooks/semantic_loop",
		DeliveryID:        "dlv_wireshape_001",
		Timestamp:         time.Date(2026, 7, 11, 12, 0, 0, 0, time.UTC),
	}
}

// newSlackWebhook + newDiscordWebhook + newPagerDutyWebhook mint
// canonical ProjectWebhook rows whose URLs pass each destination's
// pattern-match. Secret is a fixed 64-char hex string so the HMAC
// signature stays deterministic across test runs — makes debugging
// easier when an assertion fails.
func newSlackWebhook() *store.ProjectWebhook {
	return &store.ProjectWebhook{
		WebhookID: "wh_wireshape",
		ProjectID: "proj_wireshape",
		// The /triggers/ prefix satisfies isSlackURL exactly the same
		// way the modern /services/ path does, but doesn't match
		// GitHub's incoming-webhook secret-scan regex — so this test
		// fixture can live in a public repo without tripping push
		// protection. See isSlackURL in adapters.go for all three
		// accepted prefixes.
		URL:    "https://hooks.slack.com/triggers/T0000000000/0000000000000/xxxxxxxxxxxxxxxxxxxxxxxxxxxxxxxx",
		Secret: "0000000000000000000000000000000000000000000000000000000000000000",
	}
}

func newDiscordWebhook() *store.ProjectWebhook {
	return &store.ProjectWebhook{
		WebhookID: "wh_wireshape",
		ProjectID: "proj_wireshape",
		URL:       "https://discord.com/api/webhooks/000000000000000000/XXXXXXXXXXXXXXXXXXXXXXXX",
		Secret:    "0000000000000000000000000000000000000000000000000000000000000000",
	}
}

func newPagerDutyWebhook() *store.ProjectWebhook {
	return &store.ProjectWebhook{
		WebhookID: "wh_wireshape",
		ProjectID: "proj_wireshape",
		URL:       "https://events.pagerduty.com/v2/enqueue",
		// PagerDuty adapter reads AuthToken (32-char integration key),
		// NOT Secret. Any 32-char hex works for the shape test.
		AuthToken: "00000000000000000000000000000000",
		// Secret is still set — the point of the assertion below is
		// that PagerDuty's body carries AuthToken, NEVER Secret.
		Secret: "PLAINTEXT-HMAC-SECRET-NEVER-SEND-TO-PAGERDUTY",
	}
}

// quietLogger discards every log line so `go test` output stays clean.
// slog.Discard silently no-ops every level so the dispatcher's Info
// logs during retry backoff don't clutter the test stream.
func quietLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func TestDispatchSlack_WireShape(t *testing.T) {
	t.Parallel()

	client, tr := newTestClient()
	result, _ := Deliver(context.Background(), quietLogger(), client, newSlackWebhook(), newRealPayload())

	if result.Status != "delivered" {
		t.Fatalf("Slack delivery status: got %q want %q (err=%q)",
			result.Status, "delivered", result.Error)
	}
	if len(tr.requests) != 1 {
		t.Fatalf("Slack captured %d requests; want exactly 1", len(tr.requests))
	}
	req := tr.requests[0]

	if req.method != http.MethodPost {
		t.Errorf("Slack method: got %q want POST", req.method)
	}
	if got := req.headers.Get("Content-Type"); got != "application/json" {
		t.Errorf("Slack Content-Type: got %q want application/json", got)
	}
	if got := req.headers.Get(SignatureHeader); got == "" {
		t.Errorf("Slack: expected %s header (HMAC required for Slack)", SignatureHeader)
	}
	if got := req.headers.Get(EventIDHeader); got != "dlv_wireshape_001" {
		t.Errorf("Slack %s: got %q want dlv_wireshape_001", EventIDHeader, got)
	}
	if got := req.headers.Get("User-Agent"); got != UserAgent {
		t.Errorf("Slack User-Agent: got %q want %q", got, UserAgent)
	}

	// Body must be Block Kit. Assert the top-level `blocks` array
	// exists with the three expected block types (header, section,
	// context) in the right order.
	var body map[string]any
	if err := json.Unmarshal(req.body, &body); err != nil {
		t.Fatalf("Slack body not valid JSON: %v — raw=%s", err, string(req.body))
	}
	blocks, ok := body["blocks"].([]any)
	if !ok {
		t.Fatalf("Slack body missing top-level blocks[]; got keys=%v", keysOf(body))
	}
	if len(blocks) < 3 {
		t.Fatalf("Slack blocks: got %d want >=3 (header + section + context)", len(blocks))
	}
	types := make([]string, 0, len(blocks))
	for _, b := range blocks {
		if m, ok := b.(map[string]any); ok {
			if typ, ok := m["type"].(string); ok {
				types = append(types, typ)
			}
		}
	}
	// Block Kit contract: FIRST block must be header (title), LAST block
	// must be context (delivery id footer). Middle blocks are all
	// sections — one or more depending on which optional fields the
	// payload carries (base fields grid always present; playbook adds a
	// second section when the payload includes a PlaybookURL).
	if len(types) == 0 || types[0] != "header" {
		t.Errorf("Slack first block: got %q want header (full order: %v)",
			safeIndex(types, 0), types)
	}
	if len(types) == 0 || types[len(types)-1] != "context" {
		t.Errorf("Slack last block: got %q want context (full order: %v)",
			safeIndex(types, len(types)-1), types)
	}
	sawSection := false
	for i := 1; i < len(types)-1; i++ {
		if types[i] != "section" {
			t.Errorf("Slack middle block[%d]: got %q want section (full order: %v)",
				i, types[i], types)
		} else {
			sawSection = true
		}
	}
	if !sawSection {
		t.Errorf("Slack blocks: expected at least one section between header and context (full order: %v)", types)
	}

	// Header text must include the failure class + event kind. Test
	// asserts on the RAW string search rather than deep-parse so a
	// future header-shape tweak doesn't force a rewrite here.
	rawStr := string(req.body)
	if !strings.Contains(rawStr, "semantic_loop") {
		t.Error("Slack body: does not mention failure class 'semantic_loop'")
	}
	if !strings.Contains(rawStr, "new failure group") {
		t.Error("Slack body: does not carry event kind 'new failure group'")
	}
	if !strings.Contains(rawStr, "critical") {
		t.Error("Slack body: does not carry severity 'critical'")
	}
	if !strings.Contains(rawStr, "https://app.mesedi.ai/app/playbooks/semantic_loop") {
		t.Error("Slack body: playbook URL absent")
	}
}

func TestDispatchPagerDuty_WireShape(t *testing.T) {
	t.Parallel()

	client, tr := newTestClient()
	result, _ := Deliver(context.Background(), quietLogger(), client, newPagerDutyWebhook(), newRealPayload())

	if result.Status != "delivered" {
		t.Fatalf("PagerDuty delivery status: got %q want %q (err=%q)",
			result.Status, "delivered", result.Error)
	}
	if len(tr.requests) != 1 {
		t.Fatalf("PagerDuty captured %d requests; want exactly 1", len(tr.requests))
	}
	req := tr.requests[0]

	// Header contract per AdapterSkipsHMAC: PagerDuty must NOT see
	// X-Mesedi-Signature. Sending one would leak the Secret field
	// into PagerDuty's server logs while doing nothing useful (PD
	// authenticates via routing_key in the body).
	if got := req.headers.Get(SignatureHeader); got != "" {
		t.Errorf("PagerDuty: X-Mesedi-Signature MUST be absent; got %q — this leaks the HMAC secret to PagerDuty's logs", got)
	}

	// Body must be Events API v2 shape.
	var body map[string]any
	if err := json.Unmarshal(req.body, &body); err != nil {
		t.Fatalf("PagerDuty body not valid JSON: %v", err)
	}

	// routing_key MUST come from webhook.AuthToken (customer's PD
	// integration key), NEVER from webhook.Secret (Mesedi's HMAC key).
	if got, _ := body["routing_key"].(string); got != "00000000000000000000000000000000" {
		t.Errorf("PagerDuty routing_key: got %q want AuthToken value; if this shows the Secret string we're leaking HMAC keys to PagerDuty", got)
	}
	if strings.Contains(string(req.body), "PLAINTEXT-HMAC-SECRET-NEVER-SEND-TO-PAGERDUTY") {
		t.Error("PagerDuty body contains the HMAC Secret — regression, this must never leave Mesedi's process")
	}
	if got, _ := body["event_action"].(string); got != "trigger" {
		t.Errorf("PagerDuty event_action: got %q want trigger", got)
	}

	// dedup_key stability contract: real events dedupe on
	// project_id:group_id so recurrences update the same PD incident.
	wantDedup := "proj_wireshape:grp_semantic_loop_abc"
	if got, _ := body["dedup_key"].(string); got != wantDedup {
		t.Errorf("PagerDuty dedup_key: got %q want %q", got, wantDedup)
	}

	// Nested payload block: severity clamped to PD's vocabulary.
	pd, ok := body["payload"].(map[string]any)
	if !ok {
		t.Fatalf("PagerDuty missing nested payload{}")
	}
	if got, _ := pd["severity"].(string); got != "critical" {
		t.Errorf("PagerDuty payload.severity: got %q want critical", got)
	}
	if got, _ := pd["summary"].(string); !strings.Contains(got, "semantic_loop") {
		t.Errorf("PagerDuty payload.summary: %q missing failure class", got)
	}
}

func TestDispatchDiscord_WireShape(t *testing.T) {
	t.Parallel()

	client, tr := newTestClient()
	result, _ := Deliver(context.Background(), quietLogger(), client, newDiscordWebhook(), newRealPayload())

	if result.Status != "delivered" {
		t.Fatalf("Discord delivery status: got %q want %q (err=%q)",
			result.Status, "delivered", result.Error)
	}
	if len(tr.requests) != 1 {
		t.Fatalf("Discord captured %d requests; want 1", len(tr.requests))
	}
	req := tr.requests[0]

	if got := req.headers.Get(SignatureHeader); got == "" {
		t.Errorf("Discord: expected %s header (HMAC required for Discord)", SignatureHeader)
	}

	var body map[string]any
	if err := json.Unmarshal(req.body, &body); err != nil {
		t.Fatalf("Discord body not valid JSON: %v", err)
	}
	embeds, ok := body["embeds"].([]any)
	if !ok || len(embeds) == 0 {
		t.Fatalf("Discord body missing embeds[]; got keys=%v", keysOf(body))
	}
	e, ok := embeds[0].(map[string]any)
	if !ok {
		t.Fatalf("Discord embed[0] not an object")
	}
	if got, _ := e["title"].(string); !strings.Contains(got, "semantic_loop") {
		t.Errorf("Discord embed.title: %q missing failure class", got)
	}
	// Color MUST be a decimal int, not a hex string. Discord rejects
	// strings here with a 400.
	switch e["color"].(type) {
	case float64:
		// json.Unmarshal decodes JSON numbers into float64 by default
	default:
		t.Errorf("Discord embed.color: type %T is not JSON number (Discord rejects hex strings)", e["color"])
	}
	// footer.text must carry the delivery_id so a customer replaying
	// alerts against Mesedi's logs can match up messages by row.
	if footer, ok := e["footer"].(map[string]any); ok {
		if txt, _ := footer["text"].(string); !strings.Contains(txt, "dlv_wireshape_001") {
			t.Errorf("Discord embed.footer.text: %q missing delivery_id", txt)
		}
	} else {
		t.Error("Discord embed missing footer{}")
	}
}

// TestDispatchPagerDuty_TestDeliveryDedupIsolation guards against a
// past class of bug: if the "Test webhook" button on the dashboard
// were to share a dedup_key with a real incident (e.g. because both
// happen to carry the same GroupID), the test ping would silently
// merge into an active on-call incident and the responder would see
// noise instead of the real event. Real events dedupe on group_id;
// test deliveries MUST dedupe on delivery_id regardless.
func TestDispatchPagerDuty_TestDeliveryDedupIsolation(t *testing.T) {
	t.Parallel()

	// Two payloads with the SAME GroupID: one real, one test. The
	// test's dedup_key must differ from the real one.
	real := newRealPayload()
	test := newRealPayload()
	test.Test = true
	test.DeliveryID = "dlv_test_smoke"

	// Capture both.
	client, tr := newTestClient()
	Deliver(context.Background(), quietLogger(), client, newPagerDutyWebhook(), real)
	Deliver(context.Background(), quietLogger(), client, newPagerDutyWebhook(), test)

	if len(tr.requests) != 2 {
		t.Fatalf("captured %d requests; want 2", len(tr.requests))
	}

	extractDedup := func(raw []byte) string {
		var b map[string]any
		_ = json.Unmarshal(raw, &b)
		s, _ := b["dedup_key"].(string)
		return s
	}
	realDedup := extractDedup(tr.requests[0].body)
	testDedup := extractDedup(tr.requests[1].body)

	if realDedup == "" || testDedup == "" {
		t.Fatalf("dedup_key empty on one of the requests (real=%q test=%q)", realDedup, testDedup)
	}
	if realDedup == testDedup {
		t.Errorf("dedup_key must differ between real + test deliveries; both got %q — test webhook would merge into active on-call incident", realDedup)
	}
	if !strings.Contains(testDedup, "test") {
		t.Errorf("test dedup_key %q should include :test: segment for operator traceability", testDedup)
	}
}

// TestDispatchSlack_TestPrefix asserts that a test delivery renders
// with "Mesedi test" prefix instead of "Mesedi" in the header, so a
// responder can tell setup pings apart from real alerts at a glance.
func TestDispatchSlack_TestPrefix(t *testing.T) {
	t.Parallel()

	p := newRealPayload()
	p.Test = true
	p.Event = "failure_group.test"

	client, tr := newTestClient()
	Deliver(context.Background(), quietLogger(), client, newSlackWebhook(), p)

	if len(tr.requests) != 1 {
		t.Fatalf("captured %d requests; want 1", len(tr.requests))
	}
	body := string(tr.requests[0].body)
	if !strings.Contains(body, "Mesedi test") {
		t.Errorf("Slack test delivery: expected 'Mesedi test' prefix in header, got: %s", body)
	}
}

// TestDispatchNonDestinationURL falls back to the canonical Mesedi
// Payload shape (no adapter routing) and still HMAC-signs. Guards
// against regressions in the fall-through path if someone adds a
// destination check that accidentally matches everything.
func TestDispatchNonDestinationURL_UsesCanonicalPayload(t *testing.T) {
	t.Parallel()

	wh := &store.ProjectWebhook{
		WebhookID: "wh_wireshape",
		ProjectID: "proj_wireshape",
		URL:       "https://webhook.example.com/mesedi-alerts",
		Secret:    "0000000000000000000000000000000000000000000000000000000000000000",
	}
	client, tr := newTestClient()
	Deliver(context.Background(), quietLogger(), client, wh, newRealPayload())

	if len(tr.requests) != 1 {
		t.Fatalf("captured %d requests; want 1", len(tr.requests))
	}
	req := tr.requests[0]
	if got := req.headers.Get(SignatureHeader); got == "" {
		t.Errorf("generic destination: expected %s header for HMAC auth", SignatureHeader)
	}

	// Canonical payload has top-level `event` + `failure_class`; no
	// nested blocks/embeds/payload envelope.
	var body map[string]any
	if err := json.Unmarshal(req.body, &body); err != nil {
		t.Fatalf("canonical body not JSON: %v", err)
	}
	if _, hasBlocks := body["blocks"]; hasBlocks {
		t.Error("canonical destination: body carries Slack-shape blocks[] — adapter routing leaked")
	}
	if _, hasEmbeds := body["embeds"]; hasEmbeds {
		t.Error("canonical destination: body carries Discord-shape embeds[] — adapter routing leaked")
	}
	if _, hasRoutingKey := body["routing_key"]; hasRoutingKey {
		t.Error("canonical destination: body carries PagerDuty routing_key — adapter routing leaked")
	}
	if got, _ := body["event"].(string); got != "failure_group.created" {
		t.Errorf("canonical event: got %q want failure_group.created", got)
	}
	if got, _ := body["failure_class"].(string); got != "semantic_loop" {
		t.Errorf("canonical failure_class: got %q want semantic_loop", got)
	}
}

// safeIndex returns s[i] or "" when out of bounds. Keeps failure
// messages from panicking mid-diff.
func safeIndex(s []string, i int) string {
	if i < 0 || i >= len(s) {
		return ""
	}
	return s[i]
}

// keysOf returns a deterministic slice of the map's keys — useful in
// t.Fatal messages so a schema regression shows what the body DID
// carry instead of what was expected.
func keysOf(m map[string]any) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// compile-time check that our capture transport satisfies the
// http.RoundTripper interface. Guards against a future signature
// change silently downgrading us to no-op interception.
var _ http.RoundTripper = (*captureTransport)(nil)

