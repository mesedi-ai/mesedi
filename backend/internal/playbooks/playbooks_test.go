// Unit tests for the playbook signature surface that powers AI-
// analysis staleness detection.
//
// The Signature / AllSignatures functions return SHA-256 digests of
// the embedded playbook content (excluding the disclaimer). The
// AI-analysis write path stamps the digest on each cached row; the
// dashboard compares the stored digest against the current
// in-binary digest to detect when a playbook has been updated since
// the cached analysis was generated.
package playbooks

import (
	"encoding/hex"
	"strings"
	"testing"
)

func Test_Signature_KnownFailureClasses_ReturnHex(t *testing.T) {
	// For each canonical detector, Signature should return a
	// 64-character hex string (SHA-256). Empty signature input
	// matches the catch-all sigPrefix="" patterns.
	cases := []struct {
		failureClass string
		signature    string
	}{
		{"crashes", ""},
		{"tool_failures", ""},
		{"validator_failures", "validator_failures:foo"},
		{"data_leakage", "aws_access_key"},
		{"infrastructure_throttled", "rate_limit:anthropic"},
		{"context_overflow", "context_overflow:fail:claude-haiku-4-5"},
		{"token_waste", "token_waste:abc123"},
		{"semantic_loop", "semantic_loop:state:hash8"},
		{"tool_schema_drift", "fetch_item:hex"},
		{"grounding_failure", "grounding_failure:ragas:faithfulness"},
		{"cascading_failure", "cascading_failure:child_crash"},
		{"coordination_deadlock", "coordination_deadlock:2-cycle"},
		{"provider_incident", "provider_incident:anthropic:service_unavailable"},
		{"sandbox_escape", "shell_invocation"},
		{"hitl_timeout", "hitl_timeout:explicit"},
		{"hitl_rejection_spike", "hitl_rejection_spike:rejected"},
	}
	for _, tc := range cases {
		t.Run(tc.failureClass, func(t *testing.T) {
			sig, ok := Signature(tc.failureClass, tc.signature)
			if !ok {
				t.Fatalf("Signature(%q, %q): ok=false, want true", tc.failureClass, tc.signature)
			}
			if len(sig) != 64 {
				t.Errorf("Signature(%q): len=%d, want 64", tc.failureClass, len(sig))
			}
			if _, err := hex.DecodeString(sig); err != nil {
				t.Errorf("Signature(%q) is not valid hex: %v", tc.failureClass, err)
			}
		})
	}
}

func Test_Signature_VariantPrefixes_AllReturnHex(t *testing.T) {
	// Detectors with multiple signature variants (loops, drift,
	// prompt_injection) each have a distinct content path per
	// prefix. Every prefix MUST resolve to a non-empty signature.
	cases := []struct {
		name         string
		failureClass string
		signature    string
	}{
		{"loops_identical", "loops", "identical_call_abc"},
		{"loops_similar", "loops", "similar_call_xyz"},
		{"loops_time_budget", "loops", "time_budget_1s+"},
		{"loops_step_count", "loops", "step_count_50+"},
		{"drift_new_model", "drift", "new_model:claude-opus-4-6"},
		{"drift_lexical", "drift", "lexical_drift_0.70+"},
		{"injection_instruction_tag", "prompt_injection", "instruction_tag"},
		{"injection_jailbreak_dan", "prompt_injection", "jailbreak_dan"},
		{"injection_role_override", "prompt_injection", "role_override"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			sig, ok := Signature(tc.failureClass, tc.signature)
			if !ok {
				t.Fatalf("Signature(%q, %q): ok=false", tc.failureClass, tc.signature)
			}
			if len(sig) != 64 {
				t.Errorf("Signature(%q, %q): len=%d, want 64", tc.failureClass, tc.signature, len(sig))
			}
		})
	}
}

func Test_Signature_UnknownFailureClass(t *testing.T) {
	sig, ok := Signature("nonexistent_class", "any_signature")
	if ok {
		t.Errorf("Signature(unknown): ok=true (want false), got sig=%q", sig)
	}
	if sig != "" {
		t.Errorf("Signature(unknown): sig=%q (want empty)", sig)
	}
}

func Test_Signature_Deterministic(t *testing.T) {
	// Two calls with the same inputs must return the same digest.
	// Catches accidental non-determinism (e.g. someone adding a
	// timestamp to the hashed content).
	sig1, ok1 := Signature("data_leakage", "aws_access_key")
	sig2, ok2 := Signature("data_leakage", "aws_access_key")
	if !ok1 || !ok2 {
		t.Fatal("expected both lookups to succeed")
	}
	if sig1 != sig2 {
		t.Errorf("non-deterministic Signature output: %q vs %q", sig1, sig2)
	}
}

func Test_Signature_DifferentPlaybooks_DifferentDigests(t *testing.T) {
	// Two different playbook files MUST produce different SHA-256
	// digests. If hashing logic ever degrades (e.g. someone returns
	// a constant string), this catches it.
	sigCrashes, _ := Signature("crashes", "")
	sigDataLeakage, _ := Signature("data_leakage", "any")
	if sigCrashes == "" || sigDataLeakage == "" {
		t.Skip("missing playbook content; can't compare digests")
	}
	if sigCrashes == sigDataLeakage {
		t.Errorf("crashes and data_leakage produced identical digest %q, hashing logic broken", sigCrashes)
	}
}

func Test_Signature_VariantsWithinClass_DifferentDigests(t *testing.T) {
	// Within the loops class, the 4 variants (identical_call,
	// similar_call, time_budget, step_count) point at 4 different
	// content files. Each MUST hash distinctly.
	sigs := map[string]string{}
	prefixes := []string{
		"identical_call_x",
		"similar_call_y",
		"time_budget_1s+",
		"step_count_10+",
	}
	for _, p := range prefixes {
		sig, ok := Signature("loops", p)
		if !ok {
			t.Skipf("loops variant %q missing content; skip", p)
		}
		sigs[p] = sig
	}
	// All 4 distinct values when content present.
	seen := map[string]string{}
	for prefix, sig := range sigs {
		if existing, ok := seen[sig]; ok {
			t.Errorf("loops variants %q and %q share digest %q, content files duplicated or hash broken", prefix, existing, sig)
		}
		seen[sig] = prefix
	}
}

func Test_AllSignatures_CoversEveryPattern(t *testing.T) {
	all := AllSignatures()
	if len(all) == 0 {
		t.Fatal("AllSignatures returned empty map; no playbooks resolved")
	}
	// At minimum every pattern with backing content should be
	// present. Count expected entries: every pattern whose
	// contentPath exists in the embed. We can't enumerate patterns
	// from outside the package, but we can sanity-check known
	// representative keys are present.
	required := []string{
		"crashes",
		"tool_failures",
		"validator_failures",
		"data_leakage",
		"infrastructure_throttled",
		"context_overflow",
		"token_waste",
		"semantic_loop",
		"tool_schema_drift",
		"grounding_failure",
		"cascading_failure",
		"coordination_deadlock",
		"provider_incident",
		"sandbox_escape",
		"hitl_timeout",
		"hitl_rejection_spike",
		"cost_velocity:cost_",
		"loops:identical_call_",
		"loops:similar_call_",
		"loops:time_budget_",
		"loops:step_count_",
		"drift:new_model:",
		"drift:lexical_drift_",
		"prompt_injection:instruction_tag",
		"prompt_injection:system_prompt_inject",
		"prompt_injection:jailbreak_dan",
		"prompt_injection:developer_mode",
		"prompt_injection:role_override",
		"prompt_injection:ignore_instructions",
	}
	for _, key := range required {
		if sig, ok := all[key]; !ok || sig == "" {
			t.Errorf("AllSignatures missing key %q (or empty sig); got entry=%q ok=%v", key, sig, ok)
		}
	}
}

func Test_AllSignatures_AllValuesAreHex(t *testing.T) {
	all := AllSignatures()
	for key, sig := range all {
		t.Run(key, func(t *testing.T) {
			if len(sig) != 64 {
				t.Errorf("entry %q: len=%d, want 64", key, len(sig))
			}
			if _, err := hex.DecodeString(sig); err != nil {
				t.Errorf("entry %q: not valid hex: %v", key, err)
			}
			if strings.ContainsAny(sig, "ABCDEF") {
				t.Errorf("entry %q: uppercase hex %q (encoding/hex lowercase contract)", key, sig)
			}
		})
	}
}

func Test_AllSignatures_ConsistentWithSingleSignature(t *testing.T) {
	// AllSignatures()["data_leakage"] must equal Signature("data_leakage", "x").
	// Catches the bug where the two APIs would silently diverge.
	all := AllSignatures()
	single, ok := Signature("data_leakage", "any_signature_string")
	if !ok {
		t.Fatal("Signature(data_leakage) returned ok=false")
	}
	got := all["data_leakage"]
	if got != single {
		t.Errorf("AllSignatures[data_leakage] (%q) != Signature(data_leakage,...) (%q)", got, single)
	}
}
