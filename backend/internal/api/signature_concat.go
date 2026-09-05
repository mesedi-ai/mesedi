package api

// Granular-signature wave: helpers that concatenate failure_group
// signatures from a (name, qualifier) pair for the 2 detectors that
// previously had over-coarse signatures (tool_failures + validator_failures).
//
// Design:
//   - Forward-only. When the qualifier is empty (legacy event /
//     opt-out customer / SDK that pre-dates the qualifier), the
//     signature is just the name, preserves backward-compat with
//     historical failure_groups. When qualifier is non-empty, the
//     signature is "<name>:<sanitized_qualifier>", same colon-
//     separator convention used by sandbox_escape, prompt_injection,
//     and grounding_failure for sub-signature shapes.
//   - Qualifier is sanitized before concat: control characters
//     stripped + length capped. Same defensive shape as the
//     sanitizeAllowlistKey helper in allowlist_config.go.
//   - Allowlist matching uses the BARE name (not the composite
//     signature) so existing customer allowlists keep working
//     after this change.

import (
	"strings"
	"unicode"
)

// signaturePartMaxLen caps the qualifier substring at 80 characters.
// Empirically: exception_type class names are typically 10-30 chars
// (RuntimeError, ConnectionError); customer-supplied categories
// should be similarly short. 80 leaves headroom for the occasional
// long class name (urllib3.exceptions.MaxRetryError = 32 chars)
// without letting hostile-or-buggy code dump multi-kilobyte strings
// into the failure_group signature column.
const signaturePartMaxLen = 80

// sanitizeSignaturePart cleans a qualifier substring before it gets
// concatenated into a failure_group signature. Strips control
// characters, trims surrounding whitespace, caps length. Returns
// empty string on input that's empty-after-cleaning so callers can
// gate on "" to mean "no qualifier" without false positives from
// whitespace-only inputs.
func sanitizeSignaturePart(raw string) string {
	trimmed := strings.TrimSpace(raw)
	if trimmed == "" {
		return ""
	}
	// Strip control characters defensively. A category with \x00 or
	// \t in it shouldn't reach the signature column; we'd rather
	// drop the qualifier and fall back to the bare-name signature
	// than persist a corrupt composite key.
	var b strings.Builder
	b.Grow(len(trimmed))
	for _, r := range trimmed {
		if unicode.IsControl(r) {
			continue
		}
		b.WriteRune(r)
	}
	cleaned := b.String()
	if len(cleaned) > signaturePartMaxLen {
		cleaned = cleaned[:signaturePartMaxLen]
	}
	return cleaned
}

// toolFailureSignature builds the failure_group signature for the
// tool_failures detector. Returns "<tool>:<exception_type>" when
// exception_type is non-empty after sanitization; falls back to
// "<tool>" otherwise (backward compat for legacy tool_call events
// that pre-date the SDK's exception_type capture).
func toolFailureSignature(toolName, exceptionType string) string {
	if toolName == "" {
		return ""
	}
	qualifier := sanitizeSignaturePart(exceptionType)
	if qualifier == "" {
		return toolName
	}
	return toolName + ":" + qualifier
}

// validatorFailureSignature builds the failure_group signature for
// the validator_failures detector. Returns "<name>:<category>" when
// category is non-empty after sanitization; falls back to "<name>"
// otherwise (backward compat, customers who don't opt in to the
// SDK's category arg keep their existing failure_group clusters).
func validatorFailureSignature(validatorName, category string) string {
	if validatorName == "" {
		return ""
	}
	qualifier := sanitizeSignaturePart(category)
	if qualifier == "" {
		return validatorName
	}
	return validatorName + ":" + qualifier
}
