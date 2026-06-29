// Package playbooks serves canonical fix descriptions for Mesedi
// failure-class signatures. This is Mesedi's Tier 1 repair surface
// (per docs/REPAIR_TIER_ROADMAP.md): for each failure-class signature
// the dashboard surfaces a markdown-formatted explanation of what the
// pattern usually means and what the standard remediation looks like.
// Zero Mesedi liability, text only, no actions taken. The customer's
// engineer reads the playbook and decides.
//
// Content storage uses go:embed against the content/ directory so
// playbooks ship in the binary alongside the detector code. No
// database, no migrations, no external content service, adding a
// new playbook is a markdown file plus a one-line entry in the
// patterns table below.
//
// Matching strategy:
//
//	Failure-class signatures fall into two categories, stable
//	(one playbook covers every variant) and per-instance (each
//	value needs its own playbook).
//
//	- loops/identical_call_<hash>, loops/similar_call_<hash>,
//	  loops/time_budget_<bucket>, loops/step_count_<bucket>,
//	  cost_velocity/cost_<bucket>, drift/new_model:<name>,
//	  drift/lexical_drift_<bucket>, STABLE. One playbook per
//	  sub-detector regardless of hash/bucket/model.
//
//	- tool_failures/<tool_name>, validator_failures/<validator_name>
//
// : STABLE today, default per-class playbook explains the
//
//	  general pattern. Future commits can author per-tool /
//	  per-validator overrides where the customer's tools or
//	  validators have well-known remediation patterns.
//
//	- prompt_injection/<pattern_name>, PER-INSTANCE. One
//	  playbook per detection pattern emitted by
//	  detectors/injection.go: instruction_tag,
//	  system_prompt_inject, jailbreak_dan, developer_mode,
//	  role_override, ignore_instructions.
//
//	- crashes/<empty-sig>, CATCH-ALL. Crash signatures are SHA-256
//	  hashes of exception class + stack and cannot be enumerated
//	  ahead of time, so there is no per-signature playbook. The
//	  class-level catch-all (crashes/_default.md, #259) walks the
//	  reader through the debugging framework (find the signature,
//	  inspect the last event before the crash, compare inputs
//	  across executions) and through the common crash families
//	  (parse failure, credentials, OOM, recursion, dependency).
//	  Per-execution LLM root-cause analysis handles the precise
//	  case; the static playbook is the always-available baseline.
//
// Lookup is O(N) over the patterns table for N ≈ 20 entries. Not
// worth indexing. Re-evaluate if the table grows past 200.
package playbooks

import (
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"errors"
	"io/fs"
	"strings"
)

// ErrNotFound is returned by Load when no playbook matches the
// supplied (failure_class, signature) pair.
var ErrNotFound = errors.New("playbook not found")

// disclaimer is prepended to every playbook returned by Load. It
// is intentionally short, blunt, and unambiguous so the customer
// reading the playbook in the dashboard understands the document
// is guidance based on Mesedi's pattern recognition, not a
// guarantee of fitness for the customer's specific situation.
//
// The disclaimer is centralized here rather than authored into
// each markdown file so:
//
//	(a) every existing and future playbook is covered without
//	    edit drift, and
//	(b) the wording can be updated in one place if counsel or
//	    customer-feedback indicates a tweak.
//
// Rendered as a Markdown blockquote so the dashboard styles it
// visually distinct from the body. The horizontal rule separates
// it from the playbook content that follows.
const disclaimer = "> **Guidance only.** This playbook describes a pattern Mesedi has observed across many agent deployments and a remediation that has often worked. It is not a guarantee that the recommended steps will resolve your specific situation, nor is it legal, compliance, financial, medical, or professional advice. You are responsible for evaluating, testing, and deploying any change to your production systems. Mesedi and its contributors disclaim liability for any outcome arising from following or not following this guidance. If you are unsure, consult a qualified engineer for your stack and a qualified professional for any regulated domain.\n\n---\n\n"

// content holds the markdown content for every playbook, embedded at
// compile time. The directory layout is content/<failure_class>/<name>.md
// where <name> matches the contentPath suffix in the patterns table
// below.
//
//go:embed all:content
var content embed.FS

// pattern is one row in the (failure_class, signature_prefix) →
// content_path lookup table. Order matters within a failure_class:
// the first matching pattern wins, so more-specific prefixes must
// come before less-specific ones.
type pattern struct {
	failureClass string
	// sigPrefix is matched via strings.HasPrefix. Empty string is a
	// catch-all that matches any signature within the failure_class.
	sigPrefix string
	// contentPath is the file path under content/ for this pattern's
	// markdown. May refer to a file that doesn't exist yet, Load
	// returns ErrNotFound in that case, so resolve-then-fail is fine.
	contentPath string
}

// patterns is the registry of every signature-to-content mapping.
// Add new entries here as playbook content is authored.
var patterns = []pattern{
	// ── loops ───────────────────────────────────────────────────
	{"loops", "identical_call_", "loops/identical_call.md"},
	{"loops", "similar_call_", "loops/similar_call.md"},
	{"loops", "time_budget_", "loops/time_budget.md"},
	{"loops", "step_count_", "loops/step_count.md"},

	// ── tool / validator failures ───────────────────────────────
	// Empty prefix = catch-all for the class. The content explains
	// the general remediation pattern; per-tool overrides can be
	// added with a specific prefix above this row.
	{"tool_failures", "", "tool_failures/_default.md"},
	{"validator_failures", "", "validator_failures/_default.md"},

	// ── prompt_injection, one playbook per detection pattern ───
	// Signatures emitted by detectors/injection.go. Order doesn't
	// matter here (signatures are exact-match within a class) but
	// reflects the tier ordering from the detector for readability:
	// literal sentinels first, then named jailbreaks, then semantic
	// overrides, then broad catch-alls.
	{"prompt_injection", "instruction_tag", "prompt_injection/instruction_tag.md"},
	{"prompt_injection", "system_prompt_inject", "prompt_injection/system_prompt_inject.md"},
	{"prompt_injection", "jailbreak_dan", "prompt_injection/jailbreak_dan.md"},
	{"prompt_injection", "developer_mode", "prompt_injection/developer_mode.md"},
	{"prompt_injection", "role_override", "prompt_injection/role_override.md"},
	{"prompt_injection", "ignore_instructions", "prompt_injection/ignore_instructions.md"},

	// ── cost_velocity, single class-wide playbook ──────────────
	{"cost_velocity", "cost_", "cost_velocity/_default.md"},

	// ── drift, one playbook per signal type ────────────────────
	{"drift", "new_model:", "drift/new_model.md"},
	{"drift", "lexical_drift_", "drift/lexical_drift.md"},

	// ── Tier 1 commercial-value detectors (Mesedi #1-#5, #24, #25) ──
	{"data_leakage", "", "data_leakage/_default.md"},
	{"infrastructure_throttled", "", "infrastructure_throttled/_default.md"},
	{"context_overflow", "", "context_overflow/_default.md"},
	{"token_waste", "", "token_waste/_default.md"},

	// ── Tier 2 failure-detection depth (Mesedi #6-#9) ──────────
	{"semantic_loop", "", "semantic_loop/_default.md"},
	{"tool_schema_drift", "", "tool_schema_drift/_default.md"},
	{"grounding_failure", "", "grounding_failure/_default.md"},

	// ── Tier 3 multi-agent + security (Mesedi #10-#17) ─────────
	{"cascading_failure", "", "cascading_failure/_default.md"},
	{"coordination_deadlock", "", "coordination_deadlock/_default.md"},
	{"provider_incident", "", "provider_incident/_default.md"},
	{"sandbox_escape", "", "sandbox_escape/_default.md"},

	// ── Tier 4 HITL (Mesedi #18-#21) ───────────────────────────
	{"hitl_timeout", "", "hitl_timeout/_default.md"},
	{"hitl_rejection_spike", "", "hitl_rejection_spike/_default.md"},

	// ── crashes ────────────────────────────────────────────────
	// Catch-all generic crashes playbook (#259). Earlier versions
	// of this package intentionally left crashes without a playbook
	// because crash signatures are SHA-256 hashes of exception
	// class + stack and can't be enumerated as static signature
	// prefixes. That reasoning still stands for per-signature
	// playbooks; we now ship a class-level catch-all that walks
	// customers through the debugging framework (find the signature,
	// inspect the last event before the crash, compare inputs
	// across executions) rather than pretending to know the
	// specifics. The per-execution LLM root-cause analysis handles
	// the precise case.
	{"crashes", "", "crashes/_default.md"},
}

// Resolve maps a (failure_class, signature) pair to a content path
// within the embedded filesystem. Returns (contentPath, true) on
// match or ("", false) if no pattern matches. Does NOT check whether
// the content file actually exists, Load() does that.
func Resolve(failureClass, signature string) (string, bool) {
	for _, p := range patterns {
		if p.failureClass != failureClass {
			continue
		}
		if p.sigPrefix == "" || strings.HasPrefix(signature, p.sigPrefix) {
			return p.contentPath, true
		}
	}
	return "", false
}

// Load returns the markdown content for the given (failure_class,
// signature) pair. Returns ErrNotFound if no pattern matches OR if
// the matched pattern's content file is not present in the embedded
// filesystem (allows pattern entries to be registered before their
// content is authored, the registration acts as a stub).
func Load(failureClass, signature string) (string, error) {
	path, ok := Resolve(failureClass, signature)
	if !ok {
		return "", ErrNotFound
	}
	bytes, err := content.ReadFile("content/" + path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return "", ErrNotFound
		}
		return "", err
	}
	// Prepend the guidance-only disclaimer so every rendered
	// playbook opens with it. Centralized here so no individual
	// playbook author can forget to include it.
	return disclaimer + string(bytes), nil
}

// ─────────────────────────────────────────────────────────────────────
// Playbook-signature staleness tracking (Wave ai-analysis-staleness-
// tracking).
//
// AI analyses are produced by injecting the canonical playbook
// content into Claude's prompt (Wave K). When the binary ships with
// updated playbook content, every cached AI analysis that was
// anchored on the previous content is "stale": customers reading
// the cached analysis on the failure-group detail page are reading
// text shaped by an interpretation framework that no longer matches
// what Mesedi ships.
//
// The mechanism:
//
//   1. At process boot, hash every playbook's content (SHA-256).
//      The map is keyed by content path so multiple signatures
//      mapping to the same path (e.g. loops/identical_call.md
//      covering all "identical_call_*" signatures) share one hash.
//
//   2. When AI analysis is generated, the handler reads the
//      signature for the (failure_class, signature) pair and stores
//      it on the cached row (failure_groups.analysis_playbook_signature
//      + ai_analyses.playbook_signature).
//
//   3. The dashboard renders a "Re-analyze to refresh" badge when
//      the stored signature differs from the current in-binary one.
//
// Why SHA-256 of raw bytes (NOT the disclaimer-prepended Load
// output): the disclaimer is global and changes rarely (typically
// legal copy edits). Triggering staleness across every cached
// analysis on a disclaimer tweak would be aggressive in the wrong
// direction. The disclaimer's purpose is customer-facing guidance,
// not AI-analysis grounding — changing it doesn't materially affect
// what the AI would produce.

// signatureByPath caches the SHA-256 of each playbook file's content
// (excluding the global disclaimer). Computed once at package init,
// read O(1) thereafter. Files registered in patterns but absent from
// the embedded filesystem (stub registrations) don't appear here;
// Signature() returns ("", false) for those.
var signatureByPath = func() map[string]string {
	out := map[string]string{}
	seen := map[string]bool{}
	for _, p := range patterns {
		if seen[p.contentPath] {
			continue
		}
		seen[p.contentPath] = true
		bytes, err := content.ReadFile("content/" + p.contentPath)
		if err != nil {
			// Stub registration without backing file — skip; the
			// Load() caller surfaces ErrNotFound elsewhere.
			continue
		}
		sum := sha256.Sum256(bytes)
		out[p.contentPath] = hex.EncodeToString(sum[:])
	}
	return out
}()

// Signature returns the SHA-256 hex digest of the playbook content
// that Load() would serve for the given (failure_class, signature)
// pair, EXCLUDING the disclaimer prefix. The returned string is
// 64 hex characters when found, "" when no pattern matches OR when
// the matched pattern has no backing content file (stub registration).
//
// Used by the AI-analysis write path to stamp the cached analysis
// row, and by the dashboard read path (via the API endpoint) to
// compare against the current in-binary signature for staleness
// detection.
func Signature(failureClass, signature string) (string, bool) {
	path, ok := Resolve(failureClass, signature)
	if !ok {
		return "", false
	}
	sig, ok := signatureByPath[path]
	if !ok {
		return "", false
	}
	return sig, true
}

// AllSignatures returns a copy of the in-memory signature map keyed
// by failure_class. For failure classes with multiple signature
// variants (loops, drift, prompt_injection), the returned map
// contains one entry per variant prefix — keyed as
// "<failure_class>:<sigPrefix>" or "<failure_class>" when the
// pattern's sigPrefix is empty.
//
// The dashboard fetches this map once per page render to determine
// which cached analyses are stale. Returning a flat per-(class,
// signature-prefix) map lets the dashboard answer "is this exact
// failure_group's cached analysis stale?" with one lookup.
func AllSignatures() map[string]string {
	out := make(map[string]string, len(patterns))
	for _, p := range patterns {
		sig, ok := signatureByPath[p.contentPath]
		if !ok {
			continue
		}
		key := p.failureClass
		if p.sigPrefix != "" {
			key = p.failureClass + ":" + p.sigPrefix
		}
		out[key] = sig
	}
	return out
}
