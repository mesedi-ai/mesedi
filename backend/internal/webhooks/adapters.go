// Receiver-specific payload adapters.
//
// Mesedi's canonical Payload is a generic, versioned JSON envelope
// designed for customer-side parsers (their own services consuming
// the webhook). For first-party chat receivers (Discord today; Slack
// is the obvious next adapter), that generic shape doesn't render —
// Discord requires a {content, embeds, file} body or it returns 400.
//
// Rather than ask customers to stand up a transformer, the dispatcher
// detects known receiver URL patterns and reshapes the body before
// send. The HMAC signature is still computed over the body actually
// sent, so the signing contract stays correct (Discord ignores the
// header, but other adapters in the same shape would not).
package webhooks

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// isDiscordURL returns true if the URL is a Discord webhook endpoint.
// Discord uses two host names interchangeably; both are recognized.
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

// discordEmbedColor returns the Discord embed accent color (decimal
// int — Discord rejects strings here) for a failure class. Mirrors
// the dashboard's failure-class color map so on-screen and in-Discord
// rendering match.
func discordEmbedColor(failureClass string) int {
	switch failureClass {
	case "crashes", "validator_failures", "tool_failures":
		return 0xEF4444 // red
	case "time_budget", "step_count", "cost_velocity":
		return 0xF59E0B // amber
	case "drift":
		return 0x60A5FA // blue
	case "prompt_injection":
		return 0xFF8C42 // mesedi orange
	default:
		return 0x6B7280 // muted gray
	}
}

// BuildDiscordBody returns a JSON body shaped for Discord's webhook
// API. One embed per delivery; failure_class drives title + color,
// signature appears in the description, and the sample-execution /
// playbook deep-links become embed fields with proper markdown links.
//
// Test deliveries get a "Mesedi test" prefix so the receiving channel
// can tell setup pings apart from real alerts.
func BuildDiscordBody(p Payload) ([]byte, error) {
	titlePrefix := "Mesedi alert"
	if p.Test {
		titlePrefix = "Mesedi test"
	}

	// `code` ticks around the signature for monospace rendering. Long
	// signatures (lexical_drift hashes etc.) wrap naturally inside a
	// Discord embed description so no truncation needed.
	description := fmt.Sprintf("`%s` · `%s`", p.FailureClass, p.Signature)

	type embedField struct {
		Name   string `json:"name"`
		Value  string `json:"value"`
		Inline bool   `json:"inline,omitempty"`
	}
	var fields []embedField

	// Execution deep link. DashboardURL is the React dashboard root
	// (no path); the executions route is /app/executions/{id}.
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
		Title:       fmt.Sprintf("%s · %s", titlePrefix, p.FailureClass),
		Description: description,
		Color:       discordEmbedColor(p.FailureClass),
		Fields:      fields,
		Footer: &embedFooter{
			Text: fmt.Sprintf("delivery %s", p.DeliveryID),
		},
	}
	if !p.Timestamp.IsZero() {
		e.Timestamp = p.Timestamp.UTC().Format(time.RFC3339)
	}

	return json.Marshal(body{
		Username: "Mesedi",
		Embeds:   []embed{e},
	})
}

// dashboardExecutionURL builds the React-dashboard execution detail
// URL from the DashboardURL (root, no path) and an execution ID. The
// /app/executions/{id} route lives in the dispatcher's knowledge,
// not the receiver's — receivers consuming the raw payload get just
// the base and can build their own deep links.
func dashboardExecutionURL(dashboardURL, executionID string) string {
	base := strings.TrimRight(dashboardURL, "/")
	return base + "/app/executions/" + executionID
}

// adaptedBody applies any receiver-specific payload reshape. Returns
// (body, true) when an adapter matched; (nil, false) otherwise — the
// caller should fall back to the canonical JSON marshal of Payload.
func adaptedBody(rawURL string, p Payload) ([]byte, bool, error) {
	if isDiscordURL(rawURL) {
		b, err := BuildDiscordBody(p)
		return b, true, err
	}
	if isSlackURL(rawURL) {
		b, err := BuildSlackBody(p)
		return b, true, err
	}
	return nil, false, nil
}

// isSlackURL returns true if the URL is a Slack incoming-webhook
// endpoint. Slack has shipped three URL shapes over time; we match
// the documented modern path and the legacy variant.
func isSlackURL(rawURL string) bool {
	return strings.HasPrefix(rawURL, "https://hooks.slack.com/services/") ||
		strings.HasPrefix(rawURL, "https://hooks.slack.com/triggers/") ||
		strings.HasPrefix(rawURL, "https://hooks.slack.com/workflows/")
}

// slackAttachmentColor returns the Slack attachment "color" value
// (hex with leading #, NOT decimal — Slack and Discord disagree on
// this) for a failure class. Mirrors the dashboard / Discord palette
// so on-screen, in-Discord, and in-Slack rendering match.
func slackAttachmentColor(failureClass string) string {
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

// BuildSlackBody returns a JSON body shaped for Slack's incoming-
// webhook API. One attachment per delivery; failure_class drives the
// pretext (above the attachment) and the color bar; signature
// appears in the title. The sample-execution and playbook deep-links
// become attachment fields with Slack-flavored <URL|label> markup.
//
// Test deliveries get a "Mesedi test" pretext so the receiving
// channel can tell setup pings apart from real alerts.
func BuildSlackBody(p Payload) ([]byte, error) {
	pretext := "*Mesedi alert*"
	if p.Test {
		pretext = "*Mesedi test*"
	}

	type slackField struct {
		Title string `json:"title"`
		Value string `json:"value"`
		Short bool   `json:"short,omitempty"`
	}
	var fields []slackField

	if p.SampleExecutionID != "" && p.DashboardURL != "" {
		execURL := dashboardExecutionURL(p.DashboardURL, p.SampleExecutionID)
		fields = append(fields, slackField{
			Title: "Sample execution",
			Value: fmt.Sprintf("<%s|%s>", execURL, p.SampleExecutionID),
			Short: true,
		})
	}
	if p.PlaybookURL != "" {
		fields = append(fields, slackField{
			Title: "Playbook",
			Value: fmt.Sprintf("<%s|Open recommended remediation>", p.PlaybookURL),
			Short: true,
		})
	}

	type slackAttachment struct {
		Color      string       `json:"color"`
		Pretext    string       `json:"pretext"`
		Title      string       `json:"title"`
		Text       string       `json:"text"`
		Fields     []slackField `json:"fields,omitempty"`
		Footer     string       `json:"footer,omitempty"`
		Timestamp  int64        `json:"ts,omitempty"`
		MarkdownIn []string     `json:"mrkdwn_in,omitempty"`
	}
	type slackBody struct {
		Username    string            `json:"username"`
		Attachments []slackAttachment `json:"attachments"`
	}

	att := slackAttachment{
		Color:   slackAttachmentColor(p.FailureClass),
		Pretext: pretext,
		Title:   fmt.Sprintf("%s · %s", p.FailureClass, p.Signature),
		Text:    fmt.Sprintf("First occurrence of `%s` in project.", p.FailureClass),
		Fields:  fields,
		Footer:  fmt.Sprintf("Mesedi · delivery %s", p.DeliveryID),
		// mrkdwn_in tells Slack which fields to parse for *bold* /
		// `code` markup. Without this, the pretext stars render
		// literally instead of as bold.
		MarkdownIn: []string{"pretext", "text"},
	}
	if !p.Timestamp.IsZero() {
		att.Timestamp = p.Timestamp.UTC().Unix()
	}

	return json.Marshal(slackBody{
		Username:    "Mesedi",
		Attachments: []slackAttachment{att},
	})
}
