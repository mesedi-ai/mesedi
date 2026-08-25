package api

import "testing"

// The two irregular slugs are the whole reason this is a map and not a
// string concat. Both are hyphenated while every other class keeps its
// underscores, and both are baked into shipped public URLs.
func TestPlaybookDocURL_IrregularSlugs(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"cost_velocity":    "https://mesedi.ai/docs/observability/cost-velocity",
		"prompt_injection": "https://mesedi.ai/docs/security/prompt-injection",
	}
	for class, want := range cases {
		if got := playbookDocURL("https://mesedi.ai/docs", class); got != want {
			t.Errorf("%s:\n got %s\nwant %s", class, got, want)
		}
	}
}

// The security section is not derivable from the class name either —
// these three live under /security/, everything else under
// /observability/.
func TestPlaybookDocURL_SecuritySection(t *testing.T) {
	t.Parallel()
	cases := map[string]string{
		"data_leakage":   "https://mesedi.ai/docs/security/data_leakage",
		"sandbox_escape": "https://mesedi.ai/docs/security/sandbox_escape",
	}
	for class, want := range cases {
		if got := playbookDocURL("https://mesedi.ai/docs", class); got != want {
			t.Errorf("%s:\n got %s\nwant %s", class, got, want)
		}
	}
}

func TestPlaybookDocURL_DefaultsToObservability(t *testing.T) {
	t.Parallel()
	for _, class := range []string{
		"crashes", "loops", "drift", "semantic_loop", "token_waste",
		"context_overflow", "tool_failures", "tool_schema_drift",
		"validator_failures", "provider_incident",
		"infrastructure_throttled", "cascading_failure",
		"coordination_deadlock", "grounding_failure", "hitl_timeout",
		"hitl_rejection_spike",
	} {
		want := "https://mesedi.ai/docs/observability/" + class
		if got := playbookDocURL("https://mesedi.ai/docs", class); got != want {
			t.Errorf("%s:\n got %s\nwant %s", class, got, want)
		}
	}
}

// Empty means "omit the link". A confidently-wrong URL inside a paid
// analysis is worse than no link, so the caller must be able to tell
// the difference.
func TestPlaybookDocURL_EmptyWhenUnbuildable(t *testing.T) {
	t.Parallel()
	if got := playbookDocURL("", "crashes"); got != "" {
		t.Errorf("no docs base should yield empty, got %q", got)
	}
	if got := playbookDocURL("https://mesedi.ai/docs", ""); got != "" {
		t.Errorf("no failure class should yield empty, got %q", got)
	}
}

// Handlers.DocsURL is never assigned at startup — no flag, no env var —
// so it is always "" in production. Reading it directly made every
// playbook link silently vanish. The real source is
// MESEDI_DASHBOARD_URL (set in fly.toml) with the app. subdomain
// stripped.
func TestDocsBaseURL_FallsBackToMarketingOrigin(t *testing.T) {
	t.Parallel()
	h := &Handlers{DashboardURL: "https://app.mesedi.ai"}
	if got, want := h.docsBaseURL(), "https://mesedi.ai/docs"; got != want {
		t.Fatalf("got %s, want %s", got, want)
	}
	// End to end: this is the URL a real analysis would carry.
	got := playbookDocURL(h.docsBaseURL(), "coordination_deadlock")
	want := "https://mesedi.ai/docs/observability/coordination_deadlock"
	if got != want {
		t.Errorf("resolved playbook URL:\n got %s\nwant %s", got, want)
	}
}

func TestDocsBaseURL_PrefersExplicitDocsURL(t *testing.T) {
	t.Parallel()
	h := &Handlers{
		DocsURL:      "https://docs.example.com/",
		DashboardURL: "https://app.mesedi.ai",
	}
	if got, want := h.docsBaseURL(), "https://docs.example.com"; got != want {
		t.Fatalf("explicit DocsURL should win: got %s, want %s", got, want)
	}
}

func TestDocsBaseURL_EmptyWhenNothingConfigured(t *testing.T) {
	t.Parallel()
	h := &Handlers{}
	if got := h.docsBaseURL(); got != "" {
		t.Fatalf("no config should yield empty, got %q", got)
	}
}

func TestPlaybookDocURL_TolerantOfInput(t *testing.T) {
	t.Parallel()
	want := "https://mesedi.ai/docs/observability/crashes"
	for _, base := range []string{
		"https://mesedi.ai/docs",
		"https://mesedi.ai/docs/",
		"  https://mesedi.ai/docs  ",
	} {
		if got := playbookDocURL(base, "CRASHES"); got != want {
			t.Errorf("base %q:\n got %s\nwant %s", base, got, want)
		}
	}
}
