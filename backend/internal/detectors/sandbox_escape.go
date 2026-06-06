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
		// use 169.254.169.254 as their link-local IMDS address.
		id:      "instance_metadata_access",
		pattern: `(?:169\.254\.169\.254|metadata\.google\.internal|metadata\.azure\.com)`,
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
// known sandbox-escape patterns. Returns the first matching
// pattern's id as the cluster signature (one failure_group per
// escape vector per project).
//
// Returns ("", false) when no pattern matched. Iteration order is
// stable across runs because sandboxPatterns is sorted by id at
// init.
func DetectSandboxEscape(toolPayloads []json.RawMessage) (signature string, detected bool) {
	if len(toolPayloads) == 0 {
		return "", false
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
				return "sandbox_escape:" + sp.id, true
			}
			if len(p.ReturnValue) > 0 && sp.regex.Match(p.ReturnValue) {
				return "sandbox_escape:" + sp.id, true
			}
		}
	}
	return "", false
}
