// Unit tests for receiver-specific payload adapters.
//
// Coverage:
//   - URL detection (isSlackURL, isDiscordURL, isPagerDutyURL).
//   - HMAC-skip policy (AdapterSkipsHMAC).
//   - Slack Block Kit body shape (header + section + context blocks;
//     severity + signature + class rendered as fields; playbook only
//     appears when the payload carries a URL; header text reflects
//     event kind and test-vs-real).
//   - Discord embed body shape (title reflects event kind; description
//     carries severity; color is decimal int; fields only include
//     what the payload actually has).
//   - PagerDuty Events API v2 body shape (routing_key sourced from
//     the webhook Secret; dedup_key is stable per group; severity
//     maps defensively; test deliveries force info severity so setup
//     pings never wake on-call).
//   - Color-selection fallback (severity wins when present; failure-
//     class palette is the fallback; no more missing '#' bug).
//   - eventHumanKind translates every canonical event slug the
//     dispatcher can emit.

package webhooks

import (
	"encoding/json"
	"strings"
	"testing"
	"time"
)

func TestURLDetection(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name        string
		url         string
		wantSlack   bool
		wantDiscord bool
		wantPD      bool
	}{
		{"modern slack services", "https://hooks.slack.com/services/T0/B0/xxx", true, false, false},
		{"slack triggers", "https://hooks.slack.com/triggers/xxx", true, false, false},
		{"slack workflows", "https://hooks.slack.com/workflows/xxx", true, false, false},
		{"discord canonical", "https://discord.com/api/webhooks/1/xxx", false, true, false},
		{"discord ptb", "https://ptb.discord.com/api/webhooks/1/xxx", false, true, false},
		{"pagerduty US", "https://events.pagerduty.com/v2/enqueue", false, false, true},
		{"pagerduty EU", "https://events.eu.pagerduty.com/v2/enqueue", false, false, true},
		{"unrelated https", "https://example.com/webhook", false, false, false},
		{"slack lookalike (must not match)", "https://hooks.slack.com.evil.com/services/x", false, false, false},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			if got := isSlackURL(tc.url); got != tc.wantSlack {
				t.Errorf("isSlackURL(%q) = %v, want %v", tc.url, got, tc.wantSlack)
			}
			if got := isDiscordURL(tc.url); got != tc.wantDiscord {
				t.Errorf("isDiscordURL(%q) = %v, want %v", tc.url, got, tc.wantDiscord)
			}
			if got := isPagerDutyURL(tc.url); got != tc.wantPD {
				t.Errorf("isPagerDutyURL(%q) = %v, want %v", tc.url, got, tc.wantPD)
			}
		})
	}
}

func TestAdapterSkipsHMAC(t *testing.T) {
	t.Parallel()
	// PagerDuty: skipped (routing_key is in the body; sending an
	// HMAC would leak the Secret value into PagerDuty's logs).
	if !AdapterSkipsHMAC("https://events.pagerduty.com/v2/enqueue") {
		t.Error("PagerDuty URL should skip HMAC")
	}
	// Slack + Discord: kept (customers fronting their own receiver
	// still benefit from being able to verify).
	if AdapterSkipsHMAC("https://hooks.slack.com/services/T/B/xxx") {
		t.Error("Slack URL should NOT skip HMAC")
	}
	if AdapterSkipsHMAC("https://discord.com/api/webhooks/1/xxx") {
		t.Error("Discord URL should NOT skip HMAC")
	}
	// Unknown URL: signed with HMAC (customer-owned generic receiver).
	if AdapterSkipsHMAC("https://example.com/hook") {
		t.Error("unknown URL should NOT skip HMAC")
	}
}

func sampleCreatedPayload() Payload {
	return Payload{
		Version:           "1",
		Event:             "failure_group.created",
		ProjectID:         "proj_1",
		WebhookID:         "wh_1",
		GroupID:           "grp_1",
		FailureClass:      "tool_failures",
		Severity:          "critical",
		Signature:         "sig_abc",
		SampleExecutionID: "exec_1",
		DashboardURL:      "https://app.example.com",
		PlaybookURL:       "https://app.example.com/app/playbooks?class=tool_failures",
		DeliveryID:        "del-1",
		Timestamp:         time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC),
	}
}

func TestBuildSlackBody_BlockKitShape(t *testing.T) {
	t.Parallel()
	body, err := BuildSlackBody(sampleCreatedPayload())
	if err != nil {
		t.Fatalf("BuildSlackBody: %v", err)
	}
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// Slack REQUIRES a top-level text field for notifications; missing
	// it breaks mobile lockscreen + a11y previews.
	if _, ok := parsed["text"].(string); !ok {
		t.Error("missing top-level text field (Slack notification fallback)")
	}
	blocks, ok := parsed["blocks"].([]any)
	if !ok {
		t.Fatal("missing blocks array")
	}
	// header + section + section (playbook) + context = 4
	if len(blocks) < 3 {
		t.Errorf("expected at least 3 blocks, got %d", len(blocks))
	}
	// Assert header text.
	header, _ := blocks[0].(map[string]any)
	if header["type"] != "header" {
		t.Errorf("first block type: got %v want header", header["type"])
	}
	// Assert section fields carry severity + class + signature.
	section, _ := blocks[1].(map[string]any)
	fields, _ := section["fields"].([]any)
	joined := ""
	for _, f := range fields {
		fm, _ := f.(map[string]any)
		joined += fm["text"].(string) + "|"
	}
	if !strings.Contains(joined, "critical") {
		t.Errorf("section fields missing severity: %s", joined)
	}
	if !strings.Contains(joined, "sig_abc") {
		t.Errorf("section fields missing signature: %s", joined)
	}
	if !strings.Contains(joined, "tool_failures") {
		t.Errorf("section fields missing failure_class: %s", joined)
	}
}

func TestBuildSlackBody_RecurrenceLabel(t *testing.T) {
	t.Parallel()
	p := sampleCreatedPayload()
	p.Event = "failure_group.recurred"
	body, err := BuildSlackBody(p)
	if err != nil {
		t.Fatalf("BuildSlackBody: %v", err)
	}
	// Header text must reflect recurrence — previously the copy said
	// "First occurrence" even on recurrences.
	if !strings.Contains(string(body), "recurred") {
		t.Errorf("body missing 'recurred' label: %s", body)
	}
	if strings.Contains(string(body), "First occurrence") {
		t.Errorf("body still contains stale 'First occurrence' copy")
	}
}

func TestBuildSlackBody_TestPrefix(t *testing.T) {
	t.Parallel()
	p := sampleCreatedPayload()
	p.Test = true
	p.Event = "failure_group.test"
	body, err := BuildSlackBody(p)
	if err != nil {
		t.Fatalf("BuildSlackBody: %v", err)
	}
	if !strings.Contains(string(body), "Mesedi test") {
		t.Errorf("test delivery missing 'Mesedi test' prefix: %s", body)
	}
	// A Contains check alone passed while the header actually read
	// "Mesedi test test delivery" — the prefix and the event label
	// each contributed a "test". Pin the exact header instead.
	assertNoDuplicatedTestWord(t, string(body))
}

// assertNoDuplicatedTestWord guards every adapter against the
// prefix/label collision: Payload.Test adds "test" to the title, and
// an event label that also says "test" doubles it. Shipped once; the
// customer's Discord read "Mesedi test test delivery · crashes".
func assertNoDuplicatedTestWord(t *testing.T, body string) {
	t.Helper()
	for _, bad := range []string{"test test", "Mesedi test: Mesedi"} {
		if strings.Contains(body, bad) {
			t.Errorf("duplicated wording %q in rendered body: %s", bad, body)
		}
	}
}

// Discord is the channel this bug was actually seen in, so pin the
// full title rather than a substring.
func TestBuildDiscordBody_TestTitleExact(t *testing.T) {
	t.Parallel()
	p := sampleCreatedPayload()
	p.Test = true
	p.Event = "failure_group.test"
	body, err := BuildDiscordBody(p)
	if err != nil {
		t.Fatalf("BuildDiscordBody: %v", err)
	}
	var parsed struct {
		Embeds []struct {
			Title string `json:"title"`
		} `json:"embeds"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Embeds) != 1 {
		t.Fatalf("want 1 embed, got %d", len(parsed.Embeds))
	}
	const want = "Mesedi test delivery · tool_failures"
	if parsed.Embeds[0].Title != want {
		t.Errorf("embed title = %q, want %q", parsed.Embeds[0].Title, want)
	}
	assertNoDuplicatedTestWord(t, string(body))
}

func TestBuildDiscordBody_EmbedShape(t *testing.T) {
	t.Parallel()
	body, err := BuildDiscordBody(sampleCreatedPayload())
	if err != nil {
		t.Fatalf("BuildDiscordBody: %v", err)
	}
	var parsed struct {
		Username string `json:"username"`
		Embeds   []struct {
			Title       string `json:"title"`
			Description string `json:"description"`
			Color       int    `json:"color"`
			Fields      []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"fields"`
		} `json:"embeds"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Embeds) != 1 {
		t.Fatalf("expected 1 embed, got %d", len(parsed.Embeds))
	}
	e := parsed.Embeds[0]
	if !strings.Contains(e.Title, "new failure group") {
		t.Errorf("title missing event kind: %s", e.Title)
	}
	if !strings.Contains(e.Description, "critical") {
		t.Errorf("description missing severity: %s", e.Description)
	}
	// Color: severity=critical maps to 0xEF4444 = 15680580 decimal.
	if e.Color != 0xEF4444 {
		t.Errorf("expected color=0xEF4444, got %#x", e.Color)
	}
	// Fields should carry the sample execution + playbook links.
	if len(e.Fields) < 2 {
		t.Errorf("expected 2 fields, got %d", len(e.Fields))
	}
}

func TestBuildDiscordBody_ColorFallsBackToClassPalette(t *testing.T) {
	t.Parallel()
	// Payload with NO severity should fall back to failure-class
	// palette. This is the pre-#281 legacy path we still support.
	p := sampleCreatedPayload()
	p.Severity = ""
	body, err := BuildDiscordBody(p)
	if err != nil {
		t.Fatalf("BuildDiscordBody: %v", err)
	}
	var parsed struct {
		Embeds []struct {
			Color int `json:"color"`
		} `json:"embeds"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	// tool_failures maps to red in the failure-class palette.
	if parsed.Embeds[0].Color != 0xEF4444 {
		t.Errorf("fallback color: expected 0xEF4444, got %#x", parsed.Embeds[0].Color)
	}
}

func TestBuildPagerDutyBody_Shape(t *testing.T) {
	t.Parallel()
	body, err := BuildPagerDutyBody(sampleCreatedPayload(), "routing_key_1234")
	if err != nil {
		t.Fatalf("BuildPagerDutyBody: %v", err)
	}
	var parsed struct {
		RoutingKey  string `json:"routing_key"`
		EventAction string `json:"event_action"`
		DedupKey    string `json:"dedup_key"`
		Client      string `json:"client"`
		Payload     struct {
			Summary       string         `json:"summary"`
			Source        string         `json:"source"`
			Severity      string         `json:"severity"`
			Component     string         `json:"component"`
			CustomDetails map[string]any `json:"custom_details"`
		} `json:"payload"`
		Links []struct {
			Href string `json:"href"`
			Text string `json:"text"`
		} `json:"links"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.RoutingKey != "routing_key_1234" {
		t.Errorf("routing_key: got %q want routing_key_1234", parsed.RoutingKey)
	}
	if parsed.EventAction != "trigger" {
		t.Errorf("event_action: got %q want trigger", parsed.EventAction)
	}
	// Dedup key must be stable per (project, group) so recurrences
	// update the same incident instead of spawning new ones.
	if parsed.DedupKey != "proj_1:grp_1" {
		t.Errorf("dedup_key: got %q want proj_1:grp_1", parsed.DedupKey)
	}
	if parsed.Payload.Severity != "critical" {
		t.Errorf("severity: got %q want critical", parsed.Payload.Severity)
	}
	if parsed.Payload.Component != "tool_failures" {
		t.Errorf("component: got %q want tool_failures", parsed.Payload.Component)
	}
	// custom_details should carry the full context so responders can
	// pivot without opening the dashboard.
	if parsed.Payload.CustomDetails["failure_class"] != "tool_failures" {
		t.Errorf("custom_details missing failure_class")
	}
	// Links: sample execution + playbook = 2.
	if len(parsed.Links) != 2 {
		t.Errorf("expected 2 links, got %d", len(parsed.Links))
	}
}

func TestBuildPagerDutyBody_TestDeliveryForcesInfoSeverity(t *testing.T) {
	t.Parallel()
	// Test deliveries must NEVER page the on-call engineer even if
	// the payload's severity says "critical". Forcing info is the
	// only defensive posture — anything else risks a real page every
	// time a customer clicks "Test Webhook" on the dashboard.
	p := sampleCreatedPayload()
	p.Test = true
	p.Severity = "critical"
	body, err := BuildPagerDutyBody(p, "routing_key_1234")
	if err != nil {
		t.Fatalf("BuildPagerDutyBody: %v", err)
	}
	var parsed struct {
		Payload struct {
			Severity string `json:"severity"`
			Summary  string `json:"summary"`
		} `json:"payload"`
		DedupKey string `json:"dedup_key"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed.Payload.Severity != "info" {
		t.Errorf("test delivery severity: got %q want info (must not page on-call)",
			parsed.Payload.Severity)
	}
	assertNoDuplicatedTestWord(t, parsed.Payload.Summary)
	if !strings.Contains(parsed.Payload.Summary, "Mesedi test") {
		t.Errorf("summary missing 'Mesedi test' prefix: %s", parsed.Payload.Summary)
	}
	// Test deliveries without a group_id must synthesize a unique
	// dedup_key so back-to-back tests don't collapse into one incident.
	if !strings.Contains(parsed.DedupKey, "test:") {
		t.Errorf("test dedup_key should contain 'test:' marker: %s", parsed.DedupKey)
	}
}

func TestBuildPagerDutyBody_SeverityMappingDefensive(t *testing.T) {
	t.Parallel()
	// Unrecognized severity strings map defensively to info instead
	// of 400-ing the PagerDuty API. Guards against a future
	// severity-taxonomy change or a stale seed.
	p := sampleCreatedPayload()
	p.Severity = "mysterious"
	body, err := BuildPagerDutyBody(p, "rk")
	if err != nil {
		t.Fatalf("BuildPagerDutyBody: %v", err)
	}
	if !strings.Contains(string(body), `"severity":"info"`) {
		t.Errorf("unrecognized severity should map to info, got: %s", body)
	}
}

func TestAdaptedBody_Routing(t *testing.T) {
	t.Parallel()
	p := sampleCreatedPayload()
	// Slack path — authToken unused, pass empty
	b, ok, err := adaptedBody("https://hooks.slack.com/services/T/B/x", "", p)
	if err != nil || !ok || len(b) == 0 {
		t.Errorf("slack route: ok=%v err=%v len=%d", ok, err, len(b))
	}
	// Discord path — authToken unused, pass empty
	b, ok, err = adaptedBody("https://discord.com/api/webhooks/1/x", "", p)
	if err != nil || !ok || len(b) == 0 {
		t.Errorf("discord route: ok=%v err=%v len=%d", ok, err, len(b))
	}
	// PagerDuty path — authToken IS the routing_key and must appear in body
	b, ok, err = adaptedBody("https://events.pagerduty.com/v2/enqueue", "rk_pd", p)
	if err != nil || !ok || !strings.Contains(string(b), "rk_pd") {
		t.Errorf("pagerduty route: ok=%v err=%v body-has-rk=%v",
			ok, err, strings.Contains(string(b), "rk_pd"))
	}
	// Unknown URL: no adapter matched
	b, ok, _ = adaptedBody("https://example.com/hook", "", p)
	if ok || b != nil {
		t.Errorf("unknown url: expected no adapter (ok=false, nil body), got ok=%v len=%d",
			ok, len(b))
	}
}

func TestSeverityHexColor(t *testing.T) {
	t.Parallel()
	// Severity wins when present.
	if got := severityHexColor("critical", "drift"); got != "#EF4444" {
		t.Errorf("critical: got %q want #EF4444", got)
	}
	if got := severityHexColor("warning", "crashes"); got != "#F59E0B" {
		t.Errorf("warning: got %q want #F59E0B", got)
	}
	if got := severityHexColor("info", "crashes"); got != "#60A5FA" {
		t.Errorf("info: got %q want #60A5FA", got)
	}
	// Fallback to failure-class palette when severity is empty.
	if got := severityHexColor("", "drift"); got != "#60A5FA" {
		t.Errorf("empty severity + drift class: got %q want #60A5FA", got)
	}
	// Bug regression: every class-palette entry must include the
	// leading '#'. Previously two entries were missing it.
	classes := []string{
		"crashes", "validator_failures", "tool_failures",
		"time_budget", "step_count", "cost_velocity",
		"drift", "prompt_injection", "unknown_class",
	}
	for _, c := range classes {
		got := failureClassHexColor(c)
		if !strings.HasPrefix(got, "#") {
			t.Errorf("failureClassHexColor(%q) missing '#' prefix: %q", c, got)
		}
		if len(got) != 7 {
			t.Errorf("failureClassHexColor(%q) not 7 chars: %q", c, got)
		}
	}
}

func TestEventHumanKind(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"failure_group.created":  "new failure group",
		"failure_group.recurred": "recurred",
		// "delivery", not "test delivery": the adapters own the word
		// "test" via Payload.Test. See eventHumanKind's comment.
		"failure_group.test": "delivery",
		"unrecognized":           "unrecognized",
	}
	for input, want := range cases {
		if got := eventHumanKind(input); got != want {
			t.Errorf("eventHumanKind(%q) = %q, want %q", input, got, want)
		}
	}
}
