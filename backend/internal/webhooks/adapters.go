// Receiver-specific payload adapters.
//
// Mesedi's canonical Payload is a generic, versioned JSON envelope
// designed for customer-side parsers (their own services consuming
// the webhook). For first-party chat / on-call receivers (Slack,
// Discord, PagerDuty), that generic shape doesn't render, each
// service requires its own body schema or the message either fails
// outright (400) or shows up as an unhelpful raw-JSON blob.
//
// The dispatcher detects known receiver URL patterns and reshapes
// the body before send. The HMAC signature (when we sign at all ,
// PagerDuty verifies via routing_key inside the body, so we skip
// the header there) is computed over the body actually sent, so the
// signing contract stays correct.
//
// Adapter ordering: URL detection is exact-prefix, order of checks
// is stable, and each adapter returns (body, true, err) to signal
// "I claim this URL; use my body." When no adapter matches, the
// dispatcher falls back to canonical Payload JSON.

package webhooks

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// isDiscordURL returns true if the URL is a Discord webhook endpoint.
// Discord uses several host names interchangeably; all are recognized.
//
// Note: this matches the canonical webhook path. Discord also offers
// a /slack-compatibility variant (URL + "/slack") that accepts Slack
// payloads; we don't shape for that here because the canonical
// embeds API gives us color, fields, and timestamps that the Slack
// shim drops.
func isDiscordURL(rawURL string) bool {
	return strings.HasPrefix(rawURL, "https://discord.com/api/webhooks/") ||
		strings.HasPrefix(rawURL, "https://discordapp.com/api/webhooks/") ||
		strings.HasPrefix(rawURL, "https://canary.discord.com/api/webhooks/") ||
		strings.HasPrefix(rawURL, "https://ptb.discord.com/api/webhooks/")
}

// isSlackURL returns true if the URL is a Slack incoming-webhook
// endpoint. Slack has shipped three URL shapes over time; we match
// the documented modern path plus two legacy variants.
func isSlackURL(rawURL string) bool {
	return strings.HasPrefix(rawURL, "https://hooks.slack.com/services/") ||
		strings.HasPrefix(rawURL, "https://hooks.slack.com/triggers/") ||
		strings.HasPrefix(rawURL, "https://hooks.slack.com/workflows/")
}

// IsPagerDutyReceiver returns true if the URL is a PagerDuty Events
// API v2 endpoint. PagerDuty publishes exactly one URL for this API
// (identical across every customer's integration); the customer's
// identity is carried inside the body as `routing_key`. See
// BuildPagerDutyBody for how the routing_key is sourced from the
// webhook's AuthToken field.
//
// PagerDuty does NOT verify HMAC signatures on inbound events; the
// dispatcher recognizes PagerDuty URLs and skips the X-Mesedi-
// Signature header for them (see AdapterSkipsHMAC).
//
// Exported so the API layer can enforce the routing_key requirement
// at webhook-create time (HandleCreateWebhook) rather than letting
// deliveries silently fail against PagerDuty's server.
func IsPagerDutyReceiver(rawURL string) bool {
	return strings.HasPrefix(rawURL, "https://events.pagerduty.com/v2/enqueue") ||
		strings.HasPrefix(rawURL, "https://events.eu.pagerduty.com/v2/enqueue")
}

// isPagerDutyURL is retained as the package-internal alias for
// callers within adapters.go. Kept separate from IsPagerDutyReceiver
// so an accidental rename of the internal shape doesn't break the
// exported API layer.
func isPagerDutyURL(rawURL string) bool {
	return IsPagerDutyReceiver(rawURL)
}

// AdapterSkipsHMAC reports whether the receiver at rawURL uses its
// own authentication scheme instead of Mesedi's HMAC-SHA256
// signature. Used by the dispatcher to skip the X-Mesedi-Signature
// header when it would be meaningless or actively confusing.
//
// PagerDuty: the customer's integration key (routing_key) travels
// in the body itself; PagerDuty authenticates the event based on
// that value and ignores any headers we set. Sending an HMAC would
// just leak our webhook Secret field's value to PagerDuty's logs
// (they proxy through their infra), so we skip it.
//
// Slack + Discord: they don't verify signatures either, but they
// also don't leak them back to any third party, so we keep the
// header, it lets customers who front their own Slack app with a
// receiver still verify the payload.
func AdapterSkipsHMAC(rawURL string) bool {
	return isPagerDutyURL(rawURL)
}

// severityHexColor returns the hex color (with leading #) that the
// dashboard uses for a given severity. Used by Slack and Discord
// adapters so on-screen and in-chat rendering match; higher severity
// = warmer color. When severity is missing or unrecognized, falls
// back to the failure-class color for backward compatibility with
// pre-severity webhooks.
func severityHexColor(severityValue, failureClass string) string {
	switch strings.ToLower(severityValue) {
	case "critical":
		return "#EF4444" // red
	case "warning":
		return "#F59E0B" // amber
	case "info":
		return "#60A5FA" // blue
	}
	// Legacy fallback: color by failure_class if the payload doesn't
	// carry a severity (should not happen post-#281 but guards
	// against old test fixtures / partial rollouts).
	return failureClassHexColor(failureClass)
}

// failureClassHexColor returns the pre-severity per-class color
// palette. Retained as the fallback for severityHexColor and as the
// truth for the discordEmbedColor decimal conversion below.
//
// Bug history: two entries previously omitted the leading '#', which
// Slack silently rejected (falling back to grey) and Discord parsed
// as garbage. Fixed here; adapter tests pin the shape.
func failureClassHexColor(failureClass string) string {
	switch failureClass {
	case "crashes", "validator_failures", "tool_failures":
		return "#EF4444" // red
	case "time_budget", "step_count", "cost_velocity":
		return "#F59E0B" // amber
	case "drift":
		return "#60A5FA" // blue
	case "prompt_injection":
		return "#FF8C42" // mesedi orange
	default:
		return "#6B7280" // muted gray
	}
}

// discordEmbedColor returns the Discord embed accent color (decimal
// int; Discord rejects strings here) matched to failureClassHexColor.
// Kept as a separate function because Discord's numeric API is
// specific to that adapter, Slack expects strings.
func discordEmbedColor(severityValue, failureClass string) int {
	hex := severityHexColor(severityValue, failureClass)
	// Strip leading '#' and parse as decimal int. Guaranteed safe
	// because every code path above returns a properly-formatted
	// 6-char hex string with the '#' prefix.
	var n int
	_, _ = fmt.Sscanf(strings.TrimPrefix(hex, "#"), "%x", &n)
	return n
}

// eventHumanKind returns a short customer-facing label describing
// what happened. The canonical Payload.Event slug (dot-notation) is
// good for programmatic routing but too terse for a Slack/Discord
// header. Kept as a helper so all three adapters agree on wording.
func eventHumanKind(event string) string {
	switch event {
	case "failure_group.recurred":
		return "recurred"
	case "failure_group.created":
		return "new failure group"
	case "failure_group.test":
		// Deliberately NOT "test delivery". Every adapter already
		// prepends "Mesedi test" when Payload.Test is set, so saying
		// "test" here too produced "Mesedi test test delivery" in the
		// customer's Discord/Slack channel, and "Mesedi test: Mesedi
		// test delivery: ..." in PagerDuty. The word "test" belongs to
		// the Test flag, not to the event label.
		return "delivery"
	default:
		return event
	}
}

// BuildDiscordBody returns a JSON body shaped for Discord's webhook
// API. One embed per delivery. Fields:
//   - title = "Mesedi <kind> · <failure_class>"
//   - description = severity chip + monospace signature
//   - color = severity-driven (falls back to class palette)
//   - embed fields = sample execution + playbook deep-links
//   - footer = delivery id
//
// Test deliveries get a "Mesedi test" title prefix so the receiving
// channel can tell setup pings apart from real alerts.
func BuildDiscordBody(p Payload) ([]byte, error) {
	titlePrefix := "Mesedi"
	if p.Test {
		titlePrefix = "Mesedi test"
	}

	// Description carries severity and signature. Discord renders
	// **bold** markdown in embed descriptions and `code` for inline
	// monospace; both are used here for scan-ability at a glance.
	description := fmt.Sprintf("**severity: %s** · `%s` · `%s`",
		orDefault(p.Severity, "info"),
		p.FailureClass,
		p.Signature,
	)

	type embedField struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Inline bool   `json:"inline,omitempty"`
	}
	var fields []embedField

	if p.SampleExecutionID != "" && p.DashboardURL != "" {
		execURL := dashboardExecutionURL(p.DashboardURL, p.SampleExecutionID)
		fields = append(fields, embedField{
			Name:  "Sample execution",
			Value: fmt.Sprintf("[`%s`](%s)", p.SampleExecutionID, execURL),
		})
	}
	if p.PlaybookURL != "" {
		fields = append(fields, embedField{
			Name:  "Playbook",
			Value: fmt.Sprintf("[Open recommended remediation](%s)", p.PlaybookURL),
		})
	}

	type embedFooter struct {
		Text string `json:"text"`
	}
	type embed struct {
		Title       string       `json:"title"`
		Description string       `json:"description"`
		Color       int          `json:"color"`
		Fields      []embedField `json:"fields,omitempty"`
		Footer      *embedFooter `json:"footer,omitempty"`
		Timestamp   string       `json:"timestamp,omitempty"`
	}
	type body struct {
		Username string  `json:"username"`
		Embeds   []embed `json:"embeds"`
	}

	e := embed{
		Title: fmt.Sprintf("%s %s · %s",
			titlePrefix, eventHumanKind(p.Event), p.FailureClass),
		Description: description,
		Color:       discordEmbedColor(p.Severity, p.FailureClass),
		Fields:      fields,
		Footer:      &embedFooter{Text: "delivery " + p.DeliveryID},
	}
	if !p.Timestamp.IsZero() {
		e.Timestamp = p.Timestamp.UTC().Format(time.RFC3339)
	}

	return json.Marshal(body{
		Username: "Mesedi",
		Embeds:   []embed{e},
	})
}

// BuildSlackBody returns a JSON body shaped for Slack's incoming-
// webhook API using Block Kit. Modern Slack blocks render with
// consistent typography, are copy-pasteable, and support inline
// button actions, none of which the legacy attachments API did well.
//
// Block layout:
//
//	header    "Mesedi <kind> · <failure_class>"
//	section   fields grid: severity, signature, dashboard link, playbook link
//	context   "delivery <id>"
//
// The legacy attachments API supported a colored border stripe;
// Block Kit dropped that in favor of emoji indicators in the header.
// Severity is surfaced textually inside the section, which is more
// accessible for color-blind receivers anyway.
func BuildSlackBody(p Payload) ([]byte, error) {
	kind := eventHumanKind(p.Event)
	headerText := fmt.Sprintf("Mesedi %s · %s", kind, p.FailureClass)
	if p.Test {
		headerText = fmt.Sprintf("Mesedi test %s · %s", kind, p.FailureClass)
	}

	sev := orDefault(p.Severity, "info")

	// Section fields render as a 2-column grid in Slack when total
	// count is even. Keep it 4 fields (severity, class, signature,
	// project) for consistent layout across desktop + mobile.
	type slackText struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	fields := []slackText{
		{Type: "mrkdwn", Text: fmt.Sprintf("*Severity*\n%s", sev)},
		{Type: "mrkdwn", Text: fmt.Sprintf("*Class*\n`%s`", p.FailureClass)},
		{Type: "mrkdwn", Text: fmt.Sprintf("*Signature*\n`%s`", p.Signature)},
	}
	if p.SampleExecutionID != "" && p.DashboardURL != "" {
		execURL := dashboardExecutionURL(p.DashboardURL, p.SampleExecutionID)
		fields = append(fields, slackText{
			Type: "mrkdwn",
			Text: fmt.Sprintf("*Sample execution*\n<%s|%s>", execURL, p.SampleExecutionID),
		})
	}

	type slackBlock struct {
		Type     string      `json:"type"`
		Text     *slackText  `json:"text,omitempty"`
		Fields   []slackText `json:"fields,omitempty"`
		Elements []slackText `json:"elements,omitempty"`
	}
	blocks := []slackBlock{
		{
			Type: "header",
			Text: &slackText{Type: "plain_text", Text: headerText},
		},
		{
			Type:   "section",
			Fields: fields,
		},
	}
	if p.PlaybookURL != "" {
		blocks = append(blocks, slackBlock{
			Type: "section",
			Text: &slackText{
				Type: "mrkdwn",
				Text: fmt.Sprintf("<%s|Open recommended remediation>", p.PlaybookURL),
			},
		})
	}
	blocks = append(blocks, slackBlock{
		Type: "context",
		Elements: []slackText{
			{Type: "mrkdwn", Text: fmt.Sprintf("Mesedi · delivery `%s`", p.DeliveryID)},
		},
	})

	type slackBody struct {
		// text is Slack's required fallback string for notifications
		// (mobile lockscreen + a11y). Provide the same header text so
		// there's no truncated JSON blob in the notification preview.
		Text   string       `json:"text"`
		Blocks []slackBlock `json:"blocks"`
	}
	return json.Marshal(slackBody{
		Text:   headerText,
		Blocks: blocks,
	})
}

// BuildPagerDutyBody returns a JSON body shaped for PagerDuty Events
// API v2. This is a "trigger" event; PagerDuty deduplicates on
// dedup_key so recurrences update the same incident rather than
// creating new ones. The dedup_key is (project_id + group_id) ,
// stable for the lifetime of the failure group, unique across
// projects, and long enough to satisfy PagerDuty's requirements.
//
// The routing_key is sourced from the webhook's Secret field. The
// dispatcher passes it in via the pagerDutyRoutingKey parameter; the
// Secret column is the natural home for this because it's already
// nullable, per-webhook, and never appears in outbound logs.
//
// Severity mapping mirrors PagerDuty's own convention:
//
//	mesedi "critical" -> pd "critical"  (page)
//	mesedi "warning"  -> pd "warning"   (ticket)
//	mesedi "info"     -> pd "info"      (log-only)
//
// Test deliveries force severity to "info" so setup pings don't
// wake the on-call engineer.
func BuildPagerDutyBody(p Payload, routingKey string) ([]byte, error) {
	sev := orDefault(p.Severity, "info")
	if p.Test {
		sev = "info"
	}
	// PagerDuty accepts "critical" | "error" | "warning" | "info".
	// Map defensively so an unrecognized value doesn't 400 the event.
	pdSeverity := sev
	switch strings.ToLower(sev) {
	case "critical", "error", "warning", "info":
		// pass-through (already valid)
	default:
		pdSeverity = "info"
	}

	// Prefix is built in one place rather than wrapping an already-
	// built summary, which is what produced "Mesedi test: Mesedi
	// delivery: ...", the product name appearing twice in a single
	// PagerDuty incident title.
	prefix := "Mesedi"
	if p.Test {
		prefix = "Mesedi test"
	}
	summary := fmt.Sprintf("%s %s: %s (%s)",
		prefix, eventHumanKind(p.Event), p.FailureClass, p.Signature)

	// Real events: dedup by (project, group) so recurrences update
	// the same PagerDuty incident. Test events: always use the
	// delivery_id, a test must never share dedup with a real
	// incident, even if the payload happens to carry a group_id, or
	// the "Test Webhook" button on the dashboard could quietly merge
	// into an active on-call incident.
	var dedupKey string
	switch {
	case p.Test:
		dedupKey = p.ProjectID + ":test:" + p.DeliveryID
	case p.GroupID != "":
		dedupKey = p.ProjectID + ":" + p.GroupID
	default:
		dedupKey = p.ProjectID + ":test:" + p.DeliveryID
	}

	// custom_details is a free-form object PagerDuty renders on the
	// incident detail page. Everything we'd want a responder to see
	// goes here.
	details := map[string]any{
		"failure_class":       p.FailureClass,
		"signature":           p.Signature,
		"severity":            sev,
		"event":               p.Event,
		"project_id":          p.ProjectID,
		"group_id":            p.GroupID,
		"sample_execution_id": p.SampleExecutionID,
		"dashboard_url":       p.DashboardURL,
		"playbook_url":        p.PlaybookURL,
		"delivery_id":         p.DeliveryID,
	}

	// Links appear as clickable buttons on the incident page. Only
	// include the ones we actually have URLs for; PagerDuty rejects
	// empty href fields.
	type pdLink struct {
		Href string `json:"href"`
		Text string `json:"text"`
	}
	var links []pdLink
	if p.SampleExecutionID != "" && p.DashboardURL != "" {
		links = append(links, pdLink{
			Href: dashboardExecutionURL(p.DashboardURL, p.SampleExecutionID),
			Text: "Sample execution",
		})
	}
	if p.PlaybookURL != "" {
		links = append(links, pdLink{
			Href: p.PlaybookURL,
			Text: "Playbook",
		})
	}

	type pdPayload struct {
		Summary       string         `json:"summary"`
		Source        string         `json:"source"`
		Severity      string         `json:"severity"`
		Timestamp     string         `json:"timestamp,omitempty"`
		Component     string         `json:"component,omitempty"`
		Group         string         `json:"group,omitempty"`
		Class         string         `json:"class,omitempty"`
		CustomDetails map[string]any `json:"custom_details,omitempty"`
	}
	type pdEvent struct {
		RoutingKey  string    `json:"routing_key"`
		EventAction string    `json:"event_action"`
		DedupKey    string    `json:"dedup_key"`
		Client      string    `json:"client"`
		ClientURL   string    `json:"client_url,omitempty"`
		Payload     pdPayload `json:"payload"`
		Links       []pdLink  `json:"links,omitempty"`
	}

	e := pdEvent{
		RoutingKey:  routingKey,
		EventAction: "trigger",
		DedupKey:    dedupKey,
		Client:      "Mesedi",
		ClientURL:   p.DashboardURL,
		Payload: pdPayload{
			Summary:       summary,
			Source:        "mesedi:" + p.ProjectID,
			Severity:      pdSeverity,
			Component:     p.FailureClass,
			Group:         "agent-failures",
			Class:         p.FailureClass,
			CustomDetails: details,
		},
		Links: links,
	}
	if !p.Timestamp.IsZero() {
		e.Payload.Timestamp = p.Timestamp.UTC().Format(time.RFC3339)
	}

	return json.Marshal(e)
}

// dashboardExecutionURL builds the React-dashboard execution detail
// URL from the DashboardURL (root, no path) and an execution ID. The
// /app/executions/{id} route lives in the dispatcher's knowledge,
// not the receiver's, receivers consuming the raw payload get just
// the base and can build their own deep links.
func dashboardExecutionURL(dashboardURL, executionID string) string {
	base := strings.TrimRight(dashboardURL, "/")
	return base + "/app/executions/" + executionID
}

// orDefault returns v if non-empty, otherwise def. Small helper so
// the template-rendering paths above don't have to nest three-line
// if-blocks around every optional field.
func orDefault(v, def string) string {
	if v == "" {
		return def
	}
	return v
}

// adaptedBody applies any receiver-specific payload reshape. Returns
// (body, true) when an adapter matched; (nil, false) otherwise ,
// the caller falls back to the canonical JSON marshal of Payload.
//
// PagerDuty requires the routing_key sourced from the webhook
// AuthToken field, so this signature takes it explicitly rather
// than reading the whole webhook. Slack + Discord don't need the
// token at adaptation time (they use HMAC via the Secret field,
// which the dispatcher wires separately).
func adaptedBody(rawURL, authToken string, p Payload) ([]byte, bool, error) {
	if isDiscordURL(rawURL) {
		b, err := BuildDiscordBody(p)
		return b, true, err
	}
	if isSlackURL(rawURL) {
		b, err := BuildSlackBody(p)
		return b, true, err
	}
	if isPagerDutyURL(rawURL) {
		b, err := BuildPagerDutyBody(p, authToken)
		return b, true, err
	}
	return nil, false, nil
}
