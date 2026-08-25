package api

import (
	"strings"
	"testing"
	"time"

	"mesedi/backend/internal/store"
)

func linkTestGroup() *store.FailureGroup {
	now := time.Now().UTC()
	return &store.FailureGroup{
		FailureClass:       "data_leakage",
		Signature:          "aws_access_key",
		FirstSeen:          now.Add(-2 * time.Hour),
		LastSeen:           now,
		AffectedExecutions: 3,
		EventCount:         3,
		SampleExecutionID:  "exec-sample-1",
	}
}

// The URL has to reach the model, otherwise it cannot link the playbook
// it is already referencing in prose ("this matches the playbook's
// cause #2").
func TestAnalysisPrompt_IncludesPlaybookURLWhenProvided(t *testing.T) {
	t.Parallel()
	url := "https://mesedi.ai/docs/security/data_leakage"
	prompt := buildFailureGroupAnalysisPrompt(linkTestGroup(), nil, nil, url)

	if !strings.Contains(prompt, url) {
		t.Fatal("playbook URL missing from the prompt; the model cannot " +
			"link a URL it was never given")
	}
	if !strings.Contains(prompt, "Published at:") {
		t.Error("expected a labelled 'Published at:' line — the system " +
			"prompt keys the linking instruction off that exact label")
	}
}

// No URL must mean NO URL. The system prompt tells the model never to
// invent one, and the prompt itself must not leave a dangling label
// that looks like a link is coming.
func TestAnalysisPrompt_OmitsPlaybookURLWhenEmpty(t *testing.T) {
	t.Parallel()
	prompt := buildFailureGroupAnalysisPrompt(linkTestGroup(), nil, nil, "")

	if strings.Contains(prompt, "Published at:") {
		t.Fatal("empty URL still emitted a 'Published at:' label; a " +
			"confidently-broken link inside a paid analysis is worse " +
			"than no link")
	}
	if strings.Contains(prompt, "https://") {
		t.Error("prompt contains a URL despite none being supplied")
	}
}

// The playbook body itself must still be present either way — the link
// is an addition, not a replacement for the content the model reasons
// from.
func TestAnalysisPrompt_PlaybookContentPresentWithAndWithoutURL(t *testing.T) {
	t.Parallel()
	for _, url := range []string{"", "https://mesedi.ai/docs/security/data_leakage"} {
		prompt := buildFailureGroupAnalysisPrompt(linkTestGroup(), nil, nil, url)
		if !strings.Contains(prompt, "Mesedi playbook for this failure class") {
			t.Errorf("url=%q: playbook block missing entirely", url)
		}
	}
}
