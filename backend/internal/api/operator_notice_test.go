package api

import (
	"encoding/json"
	"strings"
	"testing"
)

// Signup notices go to a Discord server reached by a bearer URL. A
// full customer email address sitting in that channel is a standing
// exposure with no operational benefit, the domain is what tells you
// whether a signup is a real company, and the full address is one
// click away in the admin dashboard. Pin the masking so a future
// refactor cannot quietly widen it.
func TestMaskEmailForNotice(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"robert@example.com":       "r••@example.com",
		"a@example.com":            "a••@example.com",
		"rob.j.canario+seed@x.dev": "r••@x.dev",
		"UPPER@Example.COM":        "U••@Example.COM",
	}
	for in, want := range cases {
		if got := maskEmailForNotice(in); got != want {
			t.Errorf("maskEmailForNotice(%q) = %q, want %q", in, got, want)
		}
	}
}

// Anything that isn't an address must not fall through to something
// that leaks the raw value.
func TestMaskEmailForNotice_Malformed(t *testing.T) {
	t.Parallel()
	for _, in := range []string{"", "no-at-sign", "@leading"} {
		got := maskEmailForNotice(in)
		if got != "(hidden)" {
			t.Errorf("maskEmailForNotice(%q) = %q, want (hidden): a value we "+
				"cannot parse must not be echoed into the operator channel", in, got)
		}
	}
}

// A signup must not render in the same alarm red as a 5xx. An
// operator who learns to dismiss red boxes will eventually dismiss a
// real incident.
func TestOperatorNoticePayload_DiscordIsNotAlarmRed(t *testing.T) {
	t.Parallel()
	body, ct := operatorNoticePayload(
		"https://discord.com/api/webhooks/000/xxx",
		"New signup",
		"`r••@example.com` created a project.",
		map[string]string{"Project": "Acme", "Project ID": "proj_1"},
	)
	if ct != "application/json" {
		t.Errorf("content type = %q", ct)
	}
	var parsed struct {
		Embeds []struct {
			Title  string `json:"title"`
			Color  int    `json:"color"`
			Fields []struct {
				Name  string `json:"name"`
				Value string `json:"value"`
			} `json:"fields"`
		} `json:"embeds"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if len(parsed.Embeds) != 1 {
		t.Fatalf("want 1 embed, got %d", len(parsed.Embeds))
	}
	e := parsed.Embeds[0]
	if e.Color == 0xdc2626 {
		t.Error("operator notice uses the 5xx alarm red; a signup must be " +
			"visually distinguishable from an outage")
	}
	if e.Color != operatorNoticeColor {
		t.Errorf("color = %#x, want %#x", e.Color, operatorNoticeColor)
	}
	if e.Title != "New signup" {
		t.Errorf("title = %q", e.Title)
	}
	if len(e.Fields) != 2 {
		t.Errorf("want 2 fields, got %d", len(e.Fields))
	}
}

func TestOperatorNoticePayload_SlackShape(t *testing.T) {
	t.Parallel()
	body, _ := operatorNoticePayload(
		"https://hooks.slack.com/services/T0/B0/xxx",
		"New signup", "detail",
		map[string]string{"Project": "Acme"},
	)
	if !strings.Contains(string(body), `"attachments"`) {
		t.Errorf("slack payload must use attachments: %s", body)
	}
	if strings.Contains(string(body), `"embeds"`) {
		t.Errorf("slack payload must not carry Discord embeds: %s", body)
	}
}

// An unrecognised webhook host still has to receive something
// parseable, silently sending Discord embeds to a generic endpoint
// would look like a delivery success and produce nothing readable.
func TestOperatorNoticePayload_GenericFallback(t *testing.T) {
	t.Parallel()
	body, _ := operatorNoticePayload(
		"https://example.com/hooks/mesedi",
		"New signup", "detail",
		map[string]string{"Project": "Acme"},
	)
	var parsed map[string]any
	if err := json.Unmarshal(body, &parsed); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if parsed["kind"] != "operator_notice" {
		t.Errorf("kind = %v, want operator_notice", parsed["kind"])
	}
	if parsed["title"] != "New signup" {
		t.Errorf("title = %v", parsed["title"])
	}
}

// No webhook configured must be a silent no-op, not a panic. Local
// dev and self-hosters run without MESEDI_ALERT_WEBHOOK_URL.
func TestFireOperatorNotice_NoWebhookConfigured(t *testing.T) {
	SetAlertWebhookURL("")
	defer SetAlertWebhookURL("")
	fireOperatorNotice("t", "d", nil, discardLogger())
}
