package main

import (
	"strings"
	"testing"
)

// The instructions for reproducing this verdict without Mesedi.
//
// This is the part of the document that decides whether "check us, don't
// trust us" is a real offer or a slogan. It used to end at "then run
// mesedi-verify yourself" — which assumes the reader already has
// mesedi-verify. A recipient holds the export and the report and nothing
// else, so the practical effect of that sentence was that they trusted
// us, which is the single outcome this report exists to prevent.

func joined(v string) string { return strings.Join(howToCheck(v), "\n") }

// The load-bearing instruction, and the one most likely to be softened
// by someone who thinks they are being helpful.
func TestInstructionsRefuseAVendorSuppliedBinary(t *testing.T) {
	got := joined("a7edebd6a451")
	if !strings.Contains(got, "NOT as a binary from Mesedi") {
		t.Error("the instructions do not refuse a binary supplied by Mesedi. A verdict " +
			"produced by a program the audited party handed the auditor is that party " +
			"asserting again, and the independence becomes decorative")
	}
	if !strings.Contains(got, "git clone") || !strings.Contains(got, "go build") {
		t.Errorf("the reader is told to build from source but not how:\n%s", got)
	}
}

// Without the commit, "build it from source" is an instruction to build
// some version, which cannot be compared against this document.
func TestInstructionsPinTheExactCommit(t *testing.T) {
	got := joined("a7edebd6a451")
	if !strings.Contains(got, "git checkout a7edebd6a451") {
		t.Errorf("the instructions do not pin the commit this report came from:\n%s", got)
	}
}

// A build that cannot be tied to a commit must not send the reader
// chasing one. resolveVersion produces strings like "unknown (built
// outside a source checkout)" and "abc123 + UNCOMMITTED CHANGES";
// telling someone to `git checkout` either is telling them to check out
// something that does not exist.
func TestInstructionsDoNotPinAVersionThatCannotBeCheckedOut(t *testing.T) {
	for _, v := range []string{
		"unknown (built outside a source checkout)",
		"a7edebd6a451 + UNCOMMITTED CHANGES",
		"unknown",
	} {
		got := joined(v)
		if strings.Contains(got, "git checkout "+v) {
			t.Errorf("instructions tell the reader to check out %q, which is not a "+
				"commit:\n%s", v, got)
		}
		if !strings.Contains(got, "the commit shown in the Verifier field") {
			t.Errorf("for an unreproducible build (%q) the instructions should point at "+
				"the Verifier field rather than invent a commit:\n%s", v, got)
		}
	}
}

// All four steps must survive. Dropping the hash step in particular
// would let this report be presented next to a different export.
func TestInstructionsCoverEveryStep(t *testing.T) {
	got := joined("a7edebd6a451")
	for _, want := range []string{
		"shasum -a 256",   // 1: bind the report to the file
		"git clone",       // 2: obtain the verifier independently
		"./mesedi-verify", // 3: run it
		"is a finding",    // 4: what to do on disagreement
	} {
		if !strings.Contains(got, want) {
			t.Errorf("the instructions no longer contain %q:\n%s", want, got)
		}
	}
}

// Disagreement must be named as a finding, not as something to reconcile
// with the vendor. The whole point is that the reader's run outranks
// this document.
func TestDisagreementIsNotResolvedInMesedisFavour(t *testing.T) {
	got := joined("a7edebd6a451")
	if !strings.Contains(got, "rather than resolved in Mesedi's favour") {
		t.Errorf("the instructions do not say whose result wins on disagreement:\n%s", got)
	}
}

// The instructions reach the document, not just the helper. A function
// nobody renders is a promise nobody keeps.
func TestInstructionsAreCarriedIntoTheDocument(t *testing.T) {
	d := buildPDFDoc(sampleReport(true, true), "deadbeef")
	if len(d.HowToCheck) == 0 {
		t.Fatal("the PDF carries no instructions for checking it independently")
	}
	if !strings.Contains(strings.Join(d.HowToCheck, "\n"), "NOT as a binary from Mesedi") {
		t.Error("the document dropped the refusal of a vendor-supplied binary")
	}
}
