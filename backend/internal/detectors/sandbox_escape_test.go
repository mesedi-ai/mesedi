// Unit tests for the sandbox-escape detector.
//
// Detector scans tool_call payloads for known escape patterns:
// imports of os/subprocess, eval/exec, raw sockets, instance-metadata
// access (169.254.169.254), /proc/sys access, privilege escalation
// (chmod/chown), host secret reads, JS vm module, dynamic require,
// Function constructor. First-match-wins (legacy) and all-matches
// (1) variants.
//
// Custom patterns scan AFTER built-ins so customer rules never
// preempt the canonical signal names.
package detectors

import (
	"encoding/json"
	"regexp"
	"sort"
	"strings"
	"testing"
)

func toolPayload(args, retval string) json.RawMessage {
	type p struct {
		Arguments   json.RawMessage `json:"arguments,omitempty"`
		ReturnValue json.RawMessage `json:"return_value,omitempty"`
	}
	out := p{}
	if args != "" {
		out.Arguments = json.RawMessage(`"` + args + `"`)
	}
	if retval != "" {
		out.ReturnValue = json.RawMessage(`"` + retval + `"`)
	}
	b, _ := json.Marshal(out)
	return b
}

// ─────────────────────────────────────────────────────────────────────
// DetectSandboxEscape (legacy first-match-wins)
// ─────────────────────────────────────────────────────────────────────

func Test_DetectSandboxEscape_NoPayloads(t *testing.T) {
	sig, fired := DetectSandboxEscape(nil)
	if fired {
		t.Errorf("nil payloads should not fire, got %q", sig)
	}
}

func Test_DetectSandboxEscape_BenignPayload(t *testing.T) {
	cases := []string{
		"select * from users where id = 1",
		"calculate fibonacci of 10",
		"format the date as YYYY-MM-DD",
	}
	for _, in := range cases {
		t.Run(in, func(t *testing.T) {
			payloads := []json.RawMessage{toolPayload(in, "")}
			sig, fired := DetectSandboxEscape(payloads)
			if fired {
				t.Errorf("benign input %q matched %q (false positive)", in, sig)
			}
		})
	}
}

func Test_DetectSandboxEscape_KnownEscapePatterns(t *testing.T) {
	cases := []struct {
		name    string
		args    string
		wantSig string
	}{
		{"python_os_import", "import os and shell out", "sandbox_escape:python_os_import"},
		{"python_subprocess_import", "from subprocess import call", "sandbox_escape:python_subprocess_import"},
		{"shell_invocation_os_system", "os.system('rm -rf /')", "sandbox_escape:shell_invocation"},
		{"shell_invocation_subprocess_run", "subprocess.run(['ls'])", "sandbox_escape:shell_invocation"},
		{"dynamic_code_eval", "eval('malicious')", "sandbox_escape:dynamic_code_eval"},
		{"dynamic_code_exec", "exec(payload)", "sandbox_escape:dynamic_code_eval"},
		{"raw_socket_open_import", "import socket", "sandbox_escape:raw_socket_open"},
		{"raw_socket_open_method", "socket.socket()", "sandbox_escape:raw_socket_open"},
		{"aws_imds_v4", "curl http://169.254.169.254/", "sandbox_escape:instance_metadata_access"},
		{"gcp_metadata", "fetch metadata.google.internal", "sandbox_escape:instance_metadata_access"},
		{"proc_self", "cat /proc/self/environ", "sandbox_escape:proc_sys_access"},
		{"sys_kernel", "ls /sys/kernel", "sandbox_escape:proc_sys_access"},
		{"chmod_setuid", "chmod 4755 /tmp/payload", "sandbox_escape:privilege_escalation"},
		{"chown_root", "chown root /tmp/workdir/payload", "sandbox_escape:privilege_escalation"},
		{"aws_credentials", "cat ~/.aws/credentials", "sandbox_escape:host_secret_read"},
		{"ssh_id_rsa", "read ~/.ssh/id_rsa", "sandbox_escape:host_secret_read"},
		{"etc_passwd", "cat /etc/passwd", "sandbox_escape:host_secret_read"},
		{"js_vm_module", "vm.runInNewContext('process.exit(1)')", "sandbox_escape:js_vm_module"},
		{"js_function_constructor", "new Function('return process')", "sandbox_escape:js_function_constructor"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			payloads := []json.RawMessage{toolPayload(tc.args, "")}
			sig, fired := DetectSandboxEscape(payloads)
			if !fired {
				t.Fatalf("expected fire on %q", tc.args)
			}
			if sig != tc.wantSig {
				t.Errorf("input %q: got %q, want %q", tc.args, sig, tc.wantSig)
			}
		})
	}
}

func Test_DetectSandboxEscape_DynamicRequire(t *testing.T) {
	// require(varName) should fire; require('fs') should NOT (literal arg).
	payloads := []json.RawMessage{toolPayload("require(modulePath)", "")}
	sig, fired := DetectSandboxEscape(payloads)
	if !fired {
		t.Fatal("dynamic require should fire")
	}
	if sig != "sandbox_escape:js_dynamic_require" {
		t.Errorf("expected js_dynamic_require, got %q", sig)
	}
}

func Test_DetectSandboxEscape_StaticRequireDoesNotFire(t *testing.T) {
	// require('fs') with a literal string MUST NOT fire (would
	// false-positive on every Node.js tool call). Note: also avoid
	// patterns that incidentally trigger other patterns (e.g. 'os').
	payloads := []json.RawMessage{toolPayload("require('lodash')", "")}
	if _, fired := DetectSandboxEscape(payloads); fired {
		t.Error("static require('lodash') should not fire")
	}
}

func Test_DetectSandboxEscape_MatchesOnReturnValue(t *testing.T) {
	// Match in return_value (not arguments) must still fire — agent
	// could be inspecting tool output for escape signal.
	payloads := []json.RawMessage{toolPayload("", "the system contains /etc/passwd")}
	sig, fired := DetectSandboxEscape(payloads)
	if !fired {
		t.Fatal("escape pattern in return_value should fire")
	}
	if sig != "sandbox_escape:host_secret_read" {
		t.Errorf("expected host_secret_read, got %q", sig)
	}
}

func Test_DetectSandboxEscape_MalformedPayloadSkipped(t *testing.T) {
	payloads := []json.RawMessage{
		json.RawMessage(`{not-valid-json`),
		toolPayload("eval('x')", ""),
	}
	sig, fired := DetectSandboxEscape(payloads)
	if !fired {
		t.Fatal("malformed payload should be skipped; valid eval should still fire")
	}
	if sig != "sandbox_escape:dynamic_code_eval" {
		t.Errorf("expected dynamic_code_eval, got %q", sig)
	}
}

// ─────────────────────────────────────────────────────────────────────
// DetectSandboxEscapeWithCustom — built-in priority, custom fallback
// ─────────────────────────────────────────────────────────────────────

func Test_DetectSandboxEscapeWithCustom_BuiltinWinsOverCustom(t *testing.T) {
	custom := []*CustomPattern{{
		PatternID: "pat_eval",
		Pattern:   `eval`,
		Compiled:  regexp.MustCompile(`eval`),
	}}
	payloads := []json.RawMessage{toolPayload("eval(x)", "")}
	sig, matchedID, fired := DetectSandboxEscapeWithCustom(payloads, custom)
	if !fired {
		t.Fatal("expected fire")
	}
	if !strings.HasPrefix(sig, "sandbox_escape:dynamic_code_eval") {
		t.Errorf("built-in should win; got %q", sig)
	}
	if matchedID != "" {
		t.Errorf("built-in match should return empty matchedID, got %q", matchedID)
	}
}

func Test_DetectSandboxEscapeWithCustom_CustomFiresOnUniquePattern(t *testing.T) {
	custom := []*CustomPattern{{
		PatternID: "pat_internal_api",
		Pattern:   `(?i)internal-api-token`,
		Compiled:  regexp.MustCompile(`(?i)internal-api-token`),
	}}
	payloads := []json.RawMessage{toolPayload("fetch internal-api-token", "")}
	sig, matchedID, fired := DetectSandboxEscapeWithCustom(payloads, custom)
	if !fired {
		t.Fatal("expected custom pattern to fire")
	}
	if sig != "sandbox_escape:custom:pat_internal_api" {
		t.Errorf("expected 'sandbox_escape:custom:pat_internal_api', got %q", sig)
	}
	if matchedID != "pat_internal_api" {
		t.Errorf("expected matchedID 'pat_internal_api', got %q", matchedID)
	}
}

func Test_DetectSandboxEscapeWithCustom_NilCustomSafe(t *testing.T) {
	custom := []*CustomPattern{nil, {PatternID: "broken", Compiled: nil}}
	payloads := []json.RawMessage{toolPayload("eval(x)", "")}
	sig, _, fired := DetectSandboxEscapeWithCustom(payloads, custom)
	if !fired || sig != "sandbox_escape:dynamic_code_eval" {
		t.Errorf("nils in custom should be skipped without panic; got fired=%v sig=%q", fired, sig)
	}
}

// ─────────────────────────────────────────────────────────────────────
// DetectSandboxEscapeAllMatchesWithCustom (1)
// ─────────────────────────────────────────────────────────────────────

func Test_DetectSandboxEscapeAllMatches_MultipleVectorsFireOnce(t *testing.T) {
	// Single payload triggers TWO built-in patterns — both must surface
	// (closes G1 vs legacy first-match-wins which only returned one).
	payloads := []json.RawMessage{toolPayload("import os; eval('hack')", "")}
	matches := DetectSandboxEscapeAllMatchesWithCustom(payloads, nil)
	var sigs []string
	for _, m := range matches {
		sigs = append(sigs, m.Signature)
	}
	sort.Strings(sigs)
	// Expect: dynamic_code_eval + python_os_import (at minimum)
	wantContains := []string{
		"sandbox_escape:dynamic_code_eval",
		"sandbox_escape:python_os_import",
	}
	for _, want := range wantContains {
		found := false
		for _, got := range sigs {
			if got == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("expected all-matches to include %q; got %v", want, sigs)
		}
	}
}

func Test_DetectSandboxEscapeAllMatches_DedupAcrossPayloads(t *testing.T) {
	// Same pattern matches in two payloads — dedup to ONE signature.
	payloads := []json.RawMessage{
		toolPayload("eval(x)", ""),
		toolPayload("eval(y)", ""),
	}
	matches := DetectSandboxEscapeAllMatchesWithCustom(payloads, nil)
	count := 0
	for _, m := range matches {
		if m.Signature == "sandbox_escape:dynamic_code_eval" {
			count++
		}
	}
	if count != 1 {
		t.Errorf("expected dynamic_code_eval to emit ONCE across two payloads; got %d", count)
	}
}

func Test_DetectSandboxEscapeAllMatches_RespectsMaxCap(t *testing.T) {
	// Build a single payload that matches many built-ins; the cap is 20,
	// but the built-in registry only has ~11 patterns. Confirm cap
	// constant exposed correctly.
	if MaxSandboxEscapeMatchesPerExecution != 20 {
		t.Errorf("MaxSandboxEscapeMatchesPerExecution = %d, want 20", MaxSandboxEscapeMatchesPerExecution)
	}
}
