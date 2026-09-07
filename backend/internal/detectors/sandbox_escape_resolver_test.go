package detectors

import (
	"encoding/json"
	"strings"
	"testing"
)

// The resolver_override pattern, added after the DSEwiki episode.
//
// WHY THIS EXISTS AS A SEPARATE FILE AND A SEPARATE PATTERN
//
// The 2026-09-07 incident radar stated that the /etc/hosts trick "is a
// sandbox_escape signature Mesedi can express". It was not. The
// host_secret_read pattern covered /etc/passwd and /etc/shadow only, so
// the exact technique in the largest agent incident of the year did not
// fire. That is the same defect shape as the rest of this codebase's
// bad week: a capability asserted in prose and absent from the code.
//
// It is deliberately NOT folded into host_secret_read. Reading a secret
// file and repointing a hostname are different acts with different
// remediations. The remediation here is specific and is the one the
// playbook already gives: allowlist by RESOLVED ADDRESS and revalidate
// at connect time, never by hostname pattern. Keeping the signature
// separate is what lets the report name that fix.

func Test_ResolverOverride_CatchesTheDSEwikiTechnique(t *testing.T) {
	// The actual shape of the DSEwiki bypass: append a line to
	// /etc/hosts pointing an invented hostname at a real IP, then call
	// it. The proxy allowlisted *.blob.core.windows.net by pattern and
	// never checked the name resolved to anything real.
	payloads := []json.RawMessage{
		toolPayload(`echo '10.1.2.3 bypass.blob.core.windows.net' >> /etc/hosts`, ""),
	}
	sig, detected := DetectSandboxEscape(payloads)
	if !detected {
		t.Fatal("writing to /etc/hosts to point an invented hostname at a real IP was " +
			"not detected. This is the technique from the DSEwiki episode, which a " +
			"second agent reproduced fourteen minutes after the first posted it")
	}
	if !strings.Contains(sig, "resolver_override") {
		t.Errorf("signature = %q, want it to name resolver_override so the report can "+
			"give the specific fix, which is to allowlist by resolved address rather "+
			"than by hostname pattern", sig)
	}
}

func Test_ResolverOverride_AlsoCatchesNameserverRepointing(t *testing.T) {
	// One layer down, same bypass: change the nameserver instead of the
	// hosts file and every lookup follows.
	// No double quotes in the argument text: toolPayload splices the
	// string straight into a JSON string literal, so an embedded quote
	// produces malformed JSON, the detector skips the payload, and the
	// test fails for a reason that has nothing to do with the pattern.
	payloads := []json.RawMessage{
		toolPayload(`printf 'nameserver 10.9.9.9' > /etc/resolv.conf`, ""),
	}
	if _, detected := DetectSandboxEscape(payloads); !detected {
		t.Error("rewriting /etc/resolv.conf was not detected; repointing the resolver " +
			"achieves the same allowlist bypass as editing the hosts file")
	}
}

// It must fire on the return value too, not only the arguments. A
// sandbox that echoes the command back is a real shape.
func Test_ResolverOverride_FiresOnReturnValueNotJustArguments(t *testing.T) {
	payloads := []json.RawMessage{toolPayload("", `wrote /etc/hosts successfully`)}
	if _, detected := DetectSandboxEscape(payloads); !detected {
		t.Error("a /etc/hosts reference in the tool return value was missed; the " +
			"detector documents itself as scanning args AND returns")
	}
}

// The half that stops the tests above passing for a trivial reason. If
// the pattern were simply "hosts" or "/etc/" it would fire on ordinary
// text, and a detector that fires on everything is muted within a week.
func Test_ResolverOverride_DoesNotFireOnOrdinaryText(t *testing.T) {
	for _, benign := range []string{
		"SELECT host FROM hosts WHERE active = 1",
		"the list of hosts is in the config",
		"https://example.com/etc/hostsomething",
		"resolv.conf.example is a template file",
		"updating /etc/nginx/nginx.conf",
	} {
		payloads := []json.RawMessage{toolPayload(benign, "")}
		if sig, detected := DetectSandboxEscape(payloads); detected &&
			strings.Contains(sig, "resolver_override") {
			t.Errorf("resolver_override fired on benign text %q (signature %q). A "+
				"pattern this loose gets the whole detector muted", benign, sig)
		}
	}
}

// Guards the decision, not just the behaviour. If someone later folds
// this into host_secret_read to tidy up, the specific remediation the
// report can offer is lost, and this test says so.
func Test_ResolverOverrideIsItsOwnSignatureNotFoldedIntoSecretRead(t *testing.T) {
	payloads := []json.RawMessage{toolPayload("cat /etc/hosts", "")}
	sig, detected := DetectSandboxEscape(payloads)
	if !detected {
		t.Fatal("/etc/hosts not detected at all")
	}
	if strings.Contains(sig, "host_secret_read") {
		t.Error("a resolver override was reported as host_secret_read. Those are " +
			"different acts with different fixes: one is reading a secret, the other " +
			"is changing what a hostname resolves to. The report cannot give the " +
			"right remediation if they share a signature")
	}
}
