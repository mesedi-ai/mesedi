// Package dlp implements the Data Loss Prevention scanner that gates
// outbound LLM prompts + tool-call arguments against a registry of
// regex rules matching secrets and PII. Hits are clustered into the
// data_leakage failure_class so SecOps gets a single page per (rule,
// architectural code path) pair rather than one alert per request.
//
// First-pass design notes:
//
//  1. Rules live in code, not in a database. They're security
//     primitives that change at human cadence and need to ship the
//     instant a customer accepts a deploy. A future revision can
//     overlay per-project rule overrides on top of this built-in
//     baseline.
//
//  2. Every rule has a severity. "critical" rules block + cluster
//     immediately (real API keys, signed tokens). "high" rules
//     redact + cluster but don't escalate to security-incident tier
//     (PII patterns like SSN where the false-positive risk is
//     non-trivial). The dashboard renders both, but webhook tiers
//     can be wired up to fire only on critical.
//
//  3. Patterns are tuned to minimize false positives at the expense
//     of some recall. Real production deployments will add their own
//     rules per-tenant once the rule-table primitive (
//     follow-up) ships. Until then we ship a conservative baseline
//     that catches the cases enterprise procurement reviewers ask
//     about: AWS keys, GCP keys, Stripe live keys, GitHub tokens,
//     Slack tokens, JWTs, private-key PEM headers, SSN, credit
//     cards.
//
//  4. The package exposes a single Scanner constructor that compiles
//     all enabled rules once at startup and reuses the *regexp.Regexp
//     instances for every scan. Go's regexp engine is RE2-based, so
//     all patterns here are guaranteed linear-time and free of
//     catastrophic-backtracking risk.
package dlp

import (
	"fmt"
	"regexp"
	"sync"
)

// Severity codes for a Rule. Affects clustering and downstream
// webhook routing but not the scan path itself.
type Severity string

const (
	SeverityCritical Severity = "critical" // real credentials / signed tokens
	SeverityHigh     Severity = "high"     // PII with non-trivial false-positive risk
	SeverityMedium   Severity = "medium"   // reserved for future rule expansion
)

// Rule is one named pattern the scanner matches against payload
// fields. ID is the stable signature used in failure_group
// clustering, callers MUST treat it as opaque (no parsing).
type Rule struct {
	// ID is the canonical, stable identifier used as the clustering
	// signature on data_leakage failure_groups (e.g. "aws_access_key").
	// Adding a new rule with the same ID as an existing one
	// re-points future hits at the same group; that's the intended
	// behavior when a rule definition is tightened to reduce false
	// positives.
	ID string
	// Label is the short human-friendly description rendered in the
	// dashboard alongside the chip ("AWS access key ID").
	Label string
	// Pattern is the regular expression evaluated against payload
	// fields. Linear-time guaranteed (RE2). Use look-anchored
	// boundaries (\b, ^, $) where false positives are likely.
	Pattern string
	// Severity controls webhook routing tier and the dashboard chip
	// color. Critical rules represent real credentials; high rules
	// represent PII with non-trivial false-positive risk; medium
	// is reserved.
	Severity Severity
}

// builtinRules is the package's baseline rule registry. Adding a
// rule here ships with the next deploy. Keep entries in alphabetical
// order by ID so diffs stay clean.
//
// Patterns sourced from each vendor's official documentation at the
// time the rule was added; revisit annually as new key formats ship.
var builtinRules = []Rule{
	{
		ID:    "anthropic_api_key",
		Label: "Anthropic API key",
		// Covers both the modern project-scoped sk-ant-api03- form
		// and the legacy sk-ant- form. Mesedi customers are the
		// most-exposed segment for these, they instrument LLM
		// calls, so an Anthropic key leaking through a prompt,
		// tool argument, or validator message reaches the same
		// observability pipeline as the call itself. Alphabetized
		// BEFORE openai_api_key intentionally, the OpenAI rule's
		// pattern `\bsk-...` would otherwise also match Anthropic
		// keys; ordering this rule first lets mergeOverlapping
		// keep the more-specific match.
		Pattern:  `\bsk-ant-(?:api03-)?[A-Za-z0-9_-]{20,}\b`,
		Severity: SeverityCritical,
	},
	{
		ID:       "aws_access_key",
		Label:    "AWS access key ID",
		Pattern:  `\bAKIA[0-9A-Z]{16}\b`,
		Severity: SeverityCritical,
	},
	{
		ID:       "aws_temporary_token",
		Label:    "AWS temporary credentials token",
		Pattern:  `\bASIA[0-9A-Z]{16}\b`,
		Severity: SeverityCritical,
	},
	{
		ID:    "credit_card_pan",
		Label: "Credit card primary account number",
		// 13-19 digits with optional separators. Tuned for VISA / MC
		// / AMEX / Discover prefixes; the upstream consumer should
		// Luhn-validate to suppress false positives on coincidental
		// digit runs (e.g. SKU numbers). For now this is opt-in
		// only, see Severity=High.
		Pattern:  `\b(?:4\d{3}|5[1-5]\d{2}|3[47]\d{2}|6011)[ -]?\d{4}[ -]?\d{4}[ -]?\d{4}\b`,
		Severity: SeverityHigh,
	},
	{
		ID:    "gcp_service_account_json",
		Label: "GCP service-account JSON key fragment",
		// Look for the unmistakeable header that prefixes every
		// downloaded GCP service-account JSON key body. Other forms
		// of GCP credentials (OAuth refresh tokens, etc.) need
		// future rules.
		Pattern:  `"type"\s*:\s*"service_account"`,
		Severity: SeverityCritical,
	},
	{
		ID:    "gemini_api_key",
		Label: "Google AI / Gemini API key",
		// Google AI Studio / Gemini API keys use the unambiguous
		// AIza prefix followed by exactly 35 alphanumeric +
		// underscore + hyphen characters (39 chars total). The
		// prefix is unique enough that this rule has near-zero
		// false-positive rate. Same Critical severity as other
		// provider-key patterns.
		Pattern:  `\bAIza[A-Za-z0-9_-]{35}\b`,
		Severity: SeverityCritical,
	},
	{
		ID:    "github_personal_token",
		Label: "GitHub personal access token (classic / fine-grained)",
		// Covers ghp_ (classic) and github_pat_ (fine-grained). PEM
		// quotes the prefix list because both variants are present
		// in production today.
		Pattern:  `\b(?:ghp_[A-Za-z0-9]{36}|github_pat_[A-Za-z0-9_]{82})\b`,
		Severity: SeverityCritical,
	},
	{
		ID:       "github_oauth_token",
		Label:    "GitHub OAuth user token",
		Pattern:  `\b(?:gho|ghu|ghs|ghr)_[A-Za-z0-9]{36}\b`,
		Severity: SeverityCritical,
	},
	{
		ID:    "jwt",
		Label: "JSON Web Token (JWT)",
		// Three base64url segments separated by dots. The first
		// segment must start with "eyJ" (base64 of {"). This
		// constraint cuts the false-positive rate dramatically vs
		// matching three generic base64 segments.
		Pattern:  `\beyJ[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\.[A-Za-z0-9_-]{8,}\b`,
		Severity: SeverityCritical,
	},
	{
		ID:    "openai_api_key",
		Label: "OpenAI API key",
		// Covers the legacy sk- form and the newer project-scoped
		// sk-proj- form.
		Pattern:  `\bsk-(?:proj-)?[A-Za-z0-9_-]{20,}\b`,
		Severity: SeverityCritical,
	},
	{
		ID:    "private_key_pem",
		Label: "Private key (PEM header)",
		// Catches any flavor of -----BEGIN ... PRIVATE KEY-----.
		Pattern:  `-----BEGIN (?:RSA |DSA |EC |OPENSSH |PGP |ENCRYPTED )?PRIVATE KEY-----`,
		Severity: SeverityCritical,
	},
	{
		ID:       "slack_token",
		Label:    "Slack token",
		Pattern:  `\bxox[baprs]-[A-Za-z0-9-]{10,}\b`,
		Severity: SeverityCritical,
	},
	{
		ID:    "ssn_us",
		Label: "US Social Security Number",
		// Conservative: requires the canonical XXX-XX-XXXX format
		// with hyphens. False-positive risk on long phone numbers
		// is non-zero, so we ship as High not Critical.
		Pattern:  `\b\d{3}-\d{2}-\d{4}\b`,
		Severity: SeverityHigh,
	},
	{
		ID:       "stripe_live_publishable_key",
		Label:    "Stripe live publishable key",
		Pattern:  `\bpk_live_[0-9a-zA-Z]{24,}\b`,
		Severity: SeverityCritical,
	},
	{
		ID:       "stripe_live_restricted_key",
		Label:    "Stripe live restricted key",
		Pattern:  `\brk_live_[0-9a-zA-Z]{24,}\b`,
		Severity: SeverityCritical,
	},
	{
		ID:       "stripe_live_secret_key",
		Label:    "Stripe live secret key",
		Pattern:  `\bsk_live_[0-9a-zA-Z]{24,}\b`,
		Severity: SeverityCritical,
	},
}

// BuiltinRules returns a copy of the package's compiled-in rule
// registry. Callers that want to override / extend / disable specific
// rules can use this as the starting point for their own ruleset.
func BuiltinRules() []Rule {
	out := make([]Rule, len(builtinRules))
	copy(out, builtinRules)
	return out
}

// RuleByID looks up a single rule by its canonical ID. Returns nil
// when the ID isn't in the registry. Used by the data_leakage detector
// to translate clustering signatures back into human labels.
func RuleByID(id string) *Rule {
	for i := range builtinRules {
		if builtinRules[i].ID == id {
			r := builtinRules[i]
			return &r
		}
	}
	return nil
}

// validateBuiltins is called once via sync.OnceValue to compile every
// built-in rule's pattern at startup and surface any syntax error
// before the scanner is constructed. We never want a customer's first
// scan to fail because of a malformed rule shipped in code.
var validateBuiltins = sync.OnceValue(func() error {
	seen := map[string]bool{}
	for i, r := range builtinRules {
		if r.ID == "" {
			return fmt.Errorf("dlp: rule index %d has empty ID", i)
		}
		if seen[r.ID] {
			return fmt.Errorf("dlp: duplicate rule ID %q at index %d", r.ID, i)
		}
		seen[r.ID] = true
		if r.Pattern == "" {
			return fmt.Errorf("dlp: rule %q has empty pattern", r.ID)
		}
		if _, err := regexp.Compile(r.Pattern); err != nil {
			return fmt.Errorf("dlp: rule %q failed to compile: %w", r.ID, err)
		}
	}
	return nil
})

// ValidateBuiltins returns an error if any built-in rule's pattern
// fails to compile. Called once during server startup so we crash
// loudly during deploy rather than silently failing scans at runtime.
// Calling it more than once is cheap; the underlying check is memoized.
func ValidateBuiltins() error {
	return validateBuiltins()
}
