// Sandbox-escape detector (Mesedi #17).
//
// Catches code-execution sandboxes (E2B, Daytona, Cody-style execution
// environments) that return tool_call arguments or return_values
// suggesting the agent tried to break out of its containment. The
// canonical signals: importing `os`/`subprocess`, opening raw
// sockets, writing to /proc, attempting to mount filesystems,
// shelling out via `eval`/`exec`/`system`, or accessing AWS / GCP
// instance metadata services.
//
// Distinct from `tool_failures` (where the call errored) and
// `data_leakage` (where the OUTBOUND prompt leaked secrets). This
// detector inspects sandbox tool args and returns for explicit
// escape-attempt patterns that should immediately page security.
//
// Implementation:
//
//  1. Pattern registry. Each pattern has an id (used as the
//     clustering signature) and a regex. All patterns are RE2
//     (linear-time guaranteed).
//
//  2. DetectSandboxEscape scans every tool_call payload's
//     arguments + return_value (stringified JSON) against every
//     pattern. First match wins per execution; the cluster
//     signature is the matched pattern's id so SecOps sees one
//     group per escape vector (e.g. "instance_metadata_access").
package detectors

import (
	"encoding/json"
	"regexp"
	"sort"
)

type sandboxPattern struct {
	id    string
	regex *regexp.Regexp
}

// sandboxPatterns is the compiled pattern list. Keep alphabetized
// for diff sanity; first-match-wins in pattern-id order (so a stable
// ordering keeps signatures deterministic across iteration).
var sandboxPatterns = mustCompilePatterns([]struct {
	id      string
	pattern string
}{
	{
		// Imports of the os module from a Python code-exec sandbox.
		// Common pattern when an agent tries to shell out via the
		// sandbox's python runtime.
		id:      "python_os_import",
		pattern: `(?:^|\W)(?:from\s+os\s+import|import\s+os\b)`,
	},
	{
		id:      "python_subprocess_import",
		pattern: `(?:^|\W)(?:from\s+subprocess\s+import|import\s+subprocess\b)`,
	},
	{
		// Direct shell invocation patterns common in escape attempts.
		id:      "shell_invocation",
		pattern: `(?:os\.system|os\.popen|subprocess\.(?:run|call|Popen|check_output)|child_process\.exec|child_process\.spawn)`,
	},
	{
		// eval / exec called with a string argument (the agent
		// trying to extend its sandbox capabilities at runtime).
		id:      "dynamic_code_eval",
		pattern: `\b(?:eval|exec)\s*\(`,
	},
	{
		// Raw socket access, the agent trying to phone home or
		// scan the network from inside the sandbox.
		id:      "raw_socket_open",
		pattern: `(?:import\s+socket\b|socket\.(?:socket|create_connection|gethostbyname))`,
	},
	{
		// Cloud instance metadata endpoints. AWS / GCP / Azure all
		// use 169.254.169.254 as their link-local IMDS address. Word
		// boundaries anchor each alternative so the pattern does not
		// accidentally match inside larger tokens like
		// "metadata.google.internal.example.com" -- which would still
		// be suspicious but is structurally a different host (#204
		// alert #5, go/regex-injection / regex-missing-anchor).
		id:      "instance_metadata_access",
		pattern: `\b(?:169\.254\.169\.254|metadata\.google\.internal|metadata\.azure\.com)\b`,
	},
	{
		// /proc / /sys access typically tied to container-escape
		// attempts.
		id:      "proc_sys_access",
		pattern: `(?:^|[^a-zA-Z])(?:/proc/(?:self|1)|/sys/kernel|/sys/class)`,
	},
	{
		// chmod / chown / setuid moves common in privilege escalation.
		id:      "privilege_escalation",
		pattern: `(?:chmod\s+[0-9]+\s+|chown\s+root|setuid\s*\(\s*0)`,
	},
	{
		// Reading the host's environment / secret files.
		id:      "host_secret_read",
		pattern: `(?:\.aws/credentials|\.ssh/(?:id_rsa|id_ed25519|known_hosts)|/etc/(?:passwd|shadow))`,
	},
})

// mustCompilePatterns is a constructor that panics at init() if any
// pattern fails to compile. Patterns ship in code; a compile failure
// here means a malformed regex shipped to prod and should fail loud
// during deploy rather than silently degrading at runtime.
func mustCompilePatterns(raw []struct {
	id      string
	pattern string
}) []sandboxPattern {
	out := make([]sandboxPattern, len(raw))
	for i, p := range raw {
		out[i] = sandboxPattern{
			id:    p.id,
			regex: regexp.MustCompile(p.pattern),
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].id < out[j].id })
	return out
}

// DetectSandboxEscape scans the supplied tool_call payloads for
// known sandbox-escape patterns. Preserved for the unit tests that
// pin the built-in behavior; production code paths now go through
// DetectSandboxEscapeWithCustom (Wave 2.1.b).
func DetectSandboxEscape(toolPayloads []json.RawMessage) (signature string, detected bool) {
	sig, _, fired := DetectSandboxEscapeWithCustom(toolPayloads, nil)
	return sig, fired
}

// SandboxEscapeMatch is one entry in the all-matches result set.
// Used by DetectSandboxEscapeAllMatchesWithCustom; the handler
// emits one failure_group per match.
type SandboxEscapeMatch struct {
	// Signature is the fully-formed failure_group signature
	// ("sandbox_escape:python_os_import",
	// "sandbox_escape:custom:<pattern_id>").
	Signature string
	// MatchedPatternID is non-empty only for custom-pattern matches.
	// The handler uses it to call IncrementPatternMatchCount so the
	// dashboard editor's match_count column reflects every fire,
	// not just the first.
	MatchedPatternID string
}

// MaxSandboxEscapeMatchesPerExecution caps the per-execution emit
// to defensive 20. Real executions can hit at most 9 built-in
// patterns + N custom patterns; 20 leaves headroom without unbounded
// growth.
const MaxSandboxEscapeMatchesPerExecution = 20

// DetectSandboxEscapeAllMatchesWithCustom returns ALL distinct
// pattern matches found across the execution's tool_call payloads,
// up to MaxSandboxEscapeMatchesPerExecution. Closes sandbox_escape.G1:
// the legacy first-match-wins variant loses multi-vector attack
// visibility (an execution that hits both python_os_import AND
// host_secret_read produces 2 failure_groups now, was 1).
//
// Built-ins emit before custom patterns, in the order
// sandboxPatterns is defined (alphabetical by id; same as the
// legacy function). Custom patterns emit in slice order. A pattern
// that matches multiple payloads within the same execution is
// counted once (signature-dedup via map).
//
// custom may be nil.
func DetectSandboxEscapeAllMatchesWithCustom(
	toolPayloads []json.RawMessage,
	custom []*CustomPattern,
) []SandboxEscapeMatch {
	if len(toolPayloads) == 0 {
		return nil
	}
	seen := make(map[string]bool)
	var matches []SandboxEscapeMatch
	for _, raw := range toolPayloads {
		if len(matches) >= MaxSandboxEscapeMatchesPerExecution {
			break
		}
		var p struct {
			Arguments   json.RawMessage `json:"arguments,omitempty"`
			ReturnValue json.RawMessage `json:"return_value,omitempty"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		for _, sp := range sandboxPatterns {
			sig := "sandbox_escape:" + sp.id
			if seen[sig] {
				continue
			}
			matched := (len(p.Arguments) > 0 && sp.regex.Match(p.Arguments)) ||
				(len(p.ReturnValue) > 0 && sp.regex.Match(p.ReturnValue))
			if matched {
				seen[sig] = true
				matches = append(matches, SandboxEscapeMatch{Signature: sig})
				if len(matches) >= MaxSandboxEscapeMatchesPerExecution {
					break
				}
			}
		}
		if len(matches) >= MaxSandboxEscapeMatchesPerExecution {
			break
		}
		for _, c := range custom {
			if c == nil || c.Compiled == nil {
				continue
			}
			sig := "sandbox_escape:custom:" + c.PatternID
			if seen[sig] {
				continue
			}
			matched := (len(p.Arguments) > 0 && c.Compiled.Match(p.Arguments)) ||
				(len(p.ReturnValue) > 0 && c.Compiled.Match(p.ReturnValue))
			if matched {
				seen[sig] = true
				matches = append(matches, SandboxEscapeMatch{
					Signature:        sig,
					MatchedPatternID: c.PatternID,
				})
				if len(matches) >= MaxSandboxEscapeMatchesPerExecution {
					break
				}
			}
		}
	}
	return matches
}

// DetectSandboxEscapeWithCustom is the per-project-aware variant.
// Built-ins first (preserves legacy first-match-wins ordering), then
// custom patterns. Returns (signature, matchedPatternID, fired);
// matchedPatternID is the project_patterns.pattern_id only when a
// custom pattern matched, allowing the handler to call
// IncrementPatternMatchCount.
//
// custom may be nil.
//
// LEGACY first-match-wins API kept for backward-compat with existing
// tests. The handler now uses DetectSandboxEscapeAllMatchesWithCustom
// per the all-matches-recorded wave (sandbox_escape.G1).
func DetectSandboxEscapeWithCustom(
	toolPayloads []json.RawMessage,
	custom []*CustomPattern,
) (signature, matchedPatternID string, fired bool) {
	if len(toolPayloads) == 0 {
		return "", "", false
	}
	for _, raw := range toolPayloads {
		// Decode just the fields we care about. arguments and
		// return_value are arbitrary JSON; coerce to string by
		// re-marshalling.
		var p struct {
			Arguments   json.RawMessage `json:"arguments,omitempty"`
			ReturnValue json.RawMessage `json:"return_value,omitempty"`
		}
		if err := json.Unmarshal(raw, &p); err != nil {
			continue
		}
		for _, sp := range sandboxPatterns {
			if len(p.Arguments) > 0 && sp.regex.Match(p.Arguments) {
				return "sandbox_escape:" + sp.id, "", true
			}
			if len(p.ReturnValue) > 0 && sp.regex.Match(p.ReturnValue) {
				return "sandbox_escape:" + sp.id, "", true
			}
		}
		for _, c := range custom {
			if c == nil || c.Compiled == nil {
				continue
			}
			if len(p.Arguments) > 0 && c.Compiled.Match(p.Arguments) {
				return "sandbox_escape:custom:" + c.PatternID, c.PatternID, true
			}
			if len(p.ReturnValue) > 0 && c.Compiled.Match(p.ReturnValue) {
				return "sandbox_escape:custom:" + c.PatternID, c.PatternID, true
			}
		}
	}
	return "", "", false
}
