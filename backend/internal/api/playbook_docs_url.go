// Canonical public docs URL for a failure class's playbook.
//
// The AI root-cause analysis is handed the playbook content and asked
// to reason from it, and it naturally writes things like "this matches
// the playbook's cause #2, user-pasted secret". Without a URL the
// reader has to go find that doc themselves. Giving the model the link
// lets it close the loop inline.

package api

import "strings"

// playbookDocSlugs holds the failure classes whose public docs route
// does NOT match the class name.
//
// The routes are not uniformly derivable: most classes keep their
// underscores (`/docs/observability/coordination_deadlock`), but two
// use hyphens (`cost-velocity`, `prompt-injection`). That inconsistency
// is baked into shipped URLs, so it is mapped explicitly rather than
// guessed at — a wrong guess produces a confident 404 in a customer's
// analysis, which is worse than no link at all.
var playbookDocSlugs = map[string]string{
	// Hyphenated routes.
	"cost_velocity":    "observability/cost-velocity",
	"prompt_injection": "security/prompt-injection",
	// Security section (underscored).
	"data_leakage":   "security/data_leakage",
	"sandbox_escape": "security/sandbox_escape",
}

// playbookDocURL returns the public URL for a failure class's playbook,
// or "" when no URL can be built. Callers must treat "" as "omit the
// link" rather than rendering a broken one.
//
// docsBase is the docs-site origin (Handlers.DocsURL), e.g.
// "https://mesedi.ai/docs". A trailing slash is tolerated.
func playbookDocURL(docsBase, failureClass string) string {
	base := strings.TrimRight(strings.TrimSpace(docsBase), "/")
	class := strings.ToLower(strings.TrimSpace(failureClass))
	if base == "" || class == "" {
		return ""
	}
	slug, ok := playbookDocSlugs[class]
	if !ok {
		// Default: observability section, class name verbatim. Covers
		// crashes, loops, drift, semantic_loop, token_waste,
		// context_overflow, tool_failures, tool_schema_drift,
		// validator_failures, provider_incident,
		// infrastructure_throttled, cascading_failure,
		// coordination_deadlock, grounding_failure, hitl_timeout,
		// hitl_rejection_spike.
		slug = "observability/" + class
	}
	return base + "/" + slug
}
