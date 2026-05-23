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

	// Execution deep link. Dashboard route is /app/executions/{id};
	// DashboardURL today is "<base>/ui/" because the backend doesn't
	// know about the Next.js dashboard host. Strip the trailing /ui/
	// and replace with /app so the link lands on the React dashboard.
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

// dashboardExecutionURL converts the backend-known DashboardURL
// ("<base>/ui/") into the React-dashboard execution detail URL
// ("<base-without-/ui>/app/executions/{id}"). When DashboardURL
// doesn't end in /ui/, we fall back to appending /app/executions/{id}
// to the given base.
//
// This conversion is here (in the Discord adapter) rather than in the
// dispatcher because the dispatcher's DashboardURL is meant for
// receiver-side parsing — they can do whatever they want with it.
// The Discord adapter, being the rendering layer, knows the route.
func dashboardExecutionURL(dashboardURL, executionID string) string {
	base := strings.TrimSuffix(dashboardURL, "/ui/")
	base = strings.TrimSuffix(base, "/ui")
	base = strings.TrimRight(base, "/")
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
	return nil, false, nil
}
