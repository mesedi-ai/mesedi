// Tests for the DLP scanner. The most important guarantees we want
// to enforce: every built-in rule actually catches a representative
// positive example, and a curated set of negative examples (things
// that LOOK like secrets but aren't) don't fire any rule.
//
// We deliberately do NOT use real production keys, even fake-shaped
// ones. The positive examples below are synthetic strings that match
// each pattern's shape but contain obviously-fake content so a
// security scanner reviewing this test file isn't fooled into thinking
// the repo leaked credentials.
package dlp

import (
	"strings"
	"testing"
)

func Test_BuiltinsValidate(t *testing.T) {
	if err := ValidateBuiltins(); err != nil {
		t.Fatalf("ValidateBuiltins: %v", err)
	}
}

func Test_Scanner_PositiveExamples(t *testing.T) {
	scanner, err := NewScanner(nil)
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}

	// Prefixes are kept as separate string literals so source-level
	// secret scanners (GitHub push protection, gitleaks, etc.) don't
	// flag this test file as containing real keys. At runtime the
	// `+` concatenations produce a single byte-identical string that
	// our own scanner WILL match. If you're editing these and any
	// test starts failing, double-check that the runtime joined
	// value still matches the rule pattern.
	const (
		awsAKIA       = "AKIA"
		awsASIA       = "ASIA"
		skPrefix      = "sk-"
		skProjPrefix  = "sk-proj-"
		ghpPrefix     = "ghp_"
		ghoPrefix     = "gho_"
		xoxbPrefix    = "xoxb-"
		stripeSkLive  = "sk_live_"
		stripePkLive  = "pk_live_"
		stripeRkLive  = "rk_live_"
	)

	cases := []struct {
		ruleID string
		input  string
	}{
		{"aws_access_key", awsAKIA + "IOSFODNN7EXAMPLE"},
		{"aws_temporary_token", awsASIA + "IOSFODNN7EXAMPLE"},
		{"openai_api_key", skPrefix + "fake-openai-key-not-real-do-not-use"},
		{"openai_api_key", skProjPrefix + "fake-project-key-not-real"},
		{"github_personal_token", ghpPrefix + "fakefakefakefakefakefakefakefakefake"},
		{"github_oauth_token", ghoPrefix + "fakefakefakefakefakefakefakefakefake"},
		{"slack_token", xoxbPrefix + "1234567890-1234567890-fakeslacktoken1234"},
		{"stripe_live_secret_key", stripeSkLive + "fakefakefakefakefakefake"},
		{"stripe_live_publishable_key", stripePkLive + "fakefakefakefakefakefake"},
		{"stripe_live_restricted_key", stripeRkLive + "fakefakefakefakefakefake"},
		{"jwt", "eyJ" + "hbGciOiJIUzI1NiIsInR5cCI6IkpXVCJ9.eyJzdWIiOiJ0ZXN0In0.fakefakefakefakesignature"},
		{"private_key_pem", "-----BEGIN RSA PRIVATE KEY-----\nfakeprivatekeycontents\n-----END RSA PRIVATE KEY-----"},
		{"private_key_pem", "-----BEGIN OPENSSH PRIVATE KEY-----\nfakekey\n-----END OPENSSH PRIVATE KEY-----"},
		{"gcp_service_account_json", `{"type": "service_account", "project_id": "fake"}`},
		{"ssn_us", "user SSN 123-45-6789 on file"},
		{"credit_card_pan", "card 4111-1111-1111-1111 expires"},
	}

	for _, tc := range cases {
		t.Run(tc.ruleID, func(t *testing.T) {
			hits := scanner.Scan(tc.input)
			if len(hits) == 0 {
				t.Fatalf("expected at least one hit for rule %q, got none. Input: %q", tc.ruleID, tc.input)
			}
			foundExpected := false
			for _, h := range hits {
				if h.RuleID == tc.ruleID {
					foundExpected = true
					break
				}
			}
			if !foundExpected {
				gotIDs := make([]string, 0, len(hits))
				for _, h := range hits {
					gotIDs = append(gotIDs, h.RuleID)
				}
				t.Errorf("expected rule %q to match, got rules %v instead", tc.ruleID, gotIDs)
			}
		})
	}
}

func Test_Scanner_NegativeExamples(t *testing.T) {
	scanner, err := NewScanner(nil)
	if err != nil {
		t.Fatalf("NewScanner: %v", err)
	}

	// Strings that LOOK like they could trigger but shouldn't. False
	// positives here = customer complaints, so we tighten patterns
	// rather than relax the test.
	cases := []struct {
		name  string
		input string
	}{
		{"short AWS-prefix-looking string", "AKIA1234"}, // < 20 chars total
		{"random base64 segment", "eyJabc"},              // missing the 3-segment JWT shape
		{"phone-shaped digits", "Call me at 1-800-555-1234 today"},
		{"random hex string", "abc123def456abc123def456abc123def456abc1"},
		{"sk- prefix in URL path", "https://example.com/sk-products/widget"}, // sk- but trailing pattern doesn't match
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			hits := scanner.Scan(tc.input)
			if len(hits) > 0 {
				ids := make([]string, 0, len(hits))
				for _, h := range hits {
					ids = append(ids, h.RuleID)
				}
				t.Errorf("expected zero hits on %q, got %v", tc.input, ids)
			}
		})
	}
}

func Test_Scanner_RedactPreservesContext(t *testing.T) {
	scanner, _ := NewScanner(nil)
	// Source-level scanner evasion: see the constants block in
	// Test_Scanner_PositiveExamples for the rationale.
	awsKey := "AKIA" + "IOSFODNN7EXAMPLE"
	input := "Sending key " + awsKey + " to the build agent."
	redacted, hits := scanner.ScanAndRedact(input)
	if len(hits) == 0 {
		t.Fatalf("expected at least one hit, got none")
	}
	if strings.Contains(redacted, awsKey) {
		t.Errorf("redacted still contains the matched secret: %q", redacted)
	}
	if !strings.Contains(redacted, "[REDACTED:aws_access_key]") {
		t.Errorf("redacted missing canonical token: %q", redacted)
	}
	if !strings.Contains(redacted, "Sending key ") || !strings.Contains(redacted, " to the build agent.") {
		t.Errorf("redacted lost surrounding context: %q", redacted)
	}
}

func Test_Scanner_MultipleDistinctHits(t *testing.T) {
	scanner, _ := NewScanner(nil)
	input := strings.Join([]string{
		"AKIA" + "IOSFODNN7EXAMPLE",
		"sk-" + "fake-openai-key-not-real-do-not-use",
		"ghp_" + "fakefakefakefakefakefakefakefakefake",
	}, " | ")
	hits := scanner.Scan(input)
	if len(hits) < 3 {
		ids := make([]string, 0, len(hits))
		for _, h := range hits {
			ids = append(ids, h.RuleID)
		}
		t.Fatalf("expected 3+ hits across 3 distinct rules, got %d (%v)", len(hits), ids)
	}
}

func Test_Scanner_Summarize_DeterministicOrder(t *testing.T) {
	scanner, _ := NewScanner(nil)
	input := "AKIA" + "IOSFODNN7EXAMPLE and AKIA" + "IOSFODNN8EXAMPLE and sk-" + "fake-openai-key-not-real"
	hits := scanner.Scan(input)
	summary := Summarize(hits)
	// Two distinct rule_ids expected: aws_access_key + openai_api_key.
	if len(summary) != 2 {
		t.Fatalf("expected 2 rule rollups, got %d (%v)", len(summary), summary)
	}
	// Alphabetical: aws_access_key, openai_api_key
	if summary[0].RuleID != "aws_access_key" || summary[1].RuleID != "openai_api_key" {
		t.Errorf("summary not in alphabetical order: %v", summary)
	}
	if summary[0].Count != 2 {
		t.Errorf("expected aws_access_key Count=2, got %d", summary[0].Count)
	}
}

func Test_HighestSeverity(t *testing.T) {
	cases := []struct {
		name string
		hits []Hit
		want Severity
	}{
		{"empty", nil, ""},
		{"high only", []Hit{{Severity: SeverityHigh}}, SeverityHigh},
		{"critical wins", []Hit{{Severity: SeverityHigh}, {Severity: SeverityCritical}}, SeverityCritical},
		{"medium fallback", []Hit{{Severity: SeverityMedium}}, SeverityMedium},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := HighestSeverity(tc.hits); got != tc.want {
				t.Errorf("HighestSeverity = %q, want %q", got, tc.want)
			}
		})
	}
}

func Test_Scanner_Idempotent(t *testing.T) {
	scanner, _ := NewScanner(nil)
	input := "key AKIA" + "IOSFODNN7EXAMPLE end"
	first := scanner.Scan(input)
	second := scanner.Scan(input)
	if len(first) != len(second) {
		t.Fatalf("non-deterministic hit count: %d vs %d", len(first), len(second))
	}
	for i := range first {
		if first[i].Start != second[i].Start || first[i].End != second[i].End {
			t.Errorf("hit %d offsets diverged: %+v vs %+v", i, first[i], second[i])
		}
	}
}
