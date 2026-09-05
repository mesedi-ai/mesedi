package main

import (
	"bytes"
	"os"
	"strings"
	"testing"
)

// No em dashes or en dashes in anything a reader sees.
//
// House style, and it kept being violated: they were added to the
// terminal report, the PDF verdict, the caveats and the instructions
// over the course of a single day, in a document written for auditors
// and procurement officers.
//
// TESTED ON THE RENDERED OUTPUT, NOT ON THE SOURCE, for two reasons.
// A source scan has to be told which files and which literals count,
// and it gets that wrong in both directions: it misses text assembled
// from other packages (internal/attest writes caveats that appear
// verbatim in this report), and it flags things that are not prose at
// all. rekorproof.go contains
//
//	const sigLinePrefix = "— "
//
// which is not a typographic choice. It is the signature-line prefix of
// the c2sp.org/tlog-checkpoint format that Sigstore signs. "Fixing" it
// breaks signature verification outright, silently, and a source-level
// sweep is exactly how someone would do that. Checking the rendered
// text cannot make that mistake, because a parsing constant never
// reaches the page.

const (
	emDash = "\u2014"
	enDash = "\u2013"
)

func reportWithEverythingPopulated() report {
	rep := sampleReport(true, true)
	rep.LogEntries = entriesFor(3, 4, 2) // mixed: exercises every wording branch
	rep.LogSummary = logConfirmation(rep.LogEntries)
	rep.Structural.Unverified = append(rep.Structural.Unverified,
		logResolutionCaveat(rep.LogEntries, false))
	return rep
}

func assertNoDashes(t *testing.T, where, text string) {
	t.Helper()
	for _, d := range []string{emDash, enDash} {
		if !strings.Contains(text, d) {
			continue
		}
		// Show the offending line rather than the whole document.
		for _, line := range strings.Split(text, "\n") {
			if strings.Contains(line, d) {
				t.Errorf("%s contains %q:\n  %s", where, d, strings.TrimSpace(line))
			}
		}
	}
}

func TestTerminalReportHasNoDashes(t *testing.T) {
	var buf bytes.Buffer
	printReport(&buf, reportWithEverythingPopulated())
	assertNoDashes(t, "the terminal report", buf.String())
}

func TestPDFTextHasNoDashes(t *testing.T) {
	d := buildPDFDoc(reportWithEverythingPopulated(), "deadbeef")

	assertNoDashes(t, "the PDF verdict note", d.VerdictNote)
	assertNoDashes(t, "the PDF footer id", d.FooterID)
	assertNoDashes(t, "the PDF instructions", strings.Join(d.HowToCheck, "\n"))
	assertNoDashes(t, "the PDF limitations", strings.Join(d.Limits, "\n"))
	for _, f := range d.Fields {
		assertNoDashes(t, "the PDF subject field "+f[0], f[1])
	}
	for _, s := range d.Sections {
		assertNoDashes(t, "the PDF section title "+s.Title, s.Title)
		assertNoDashes(t, "the PDF section "+s.Title, s.Empty)
		for _, r := range s.Rows {
			assertNoDashes(t, "the PDF row "+r.Label, r.Label+" "+r.Detail)
		}
	}
}

// Every wording branch, not just the one a sample report happens to
// take. The verdict note alone has five, and the branch that shipped an
// em dash was not the one the existing tests exercised.
func TestEveryWordingBranchIsFreeOfDashes(t *testing.T) {
	cases := []struct {
		name                      string
		verified, total, offline_ int
		offline                   bool
	}{
		{"nothing verified, offline", 0, 4, 0, true},
		{"nothing verified, online", 0, 4, 0, false},
		{"all verified by lookup", 4, 4, 0, false},
		{"all verified offline", 4, 4, 4, true},
		{"mixed methods", 4, 4, 2, false},
		{"partial, offline proofs", 2, 5, 2, true},
		{"one verified offline", 1, 3, 1, true},
		{"no checkpoints at all", 0, 0, 0, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			entries := entriesFor(tc.verified, tc.total, tc.offline_)
			assertNoDashes(t, "logConfirmation", logConfirmation(entries))
			assertNoDashes(t, "logResolutionCaveat", logResolutionCaveat(entries, tc.offline))

			rep := sampleReport(true, !tc.offline)
			rep.LogEntries = entries
			d := buildPDFDoc(rep, "deadbeef")
			assertNoDashes(t, "the PDF verdict note", d.VerdictNote)
			for _, f := range d.Fields {
				assertNoDashes(t, "the PDF field "+f[0], f[1])
			}
		})
	}
}

// The parsing constants must NOT be swept up by a future tidy. Asserted
// here so the reason sits next to the rule it is an exception to.
//
// Checked against the source text because both are declared inside
// functions and are not reachable from a test. That is weaker than a
// value comparison and still worth having: rekorproof.go's own comment
// records that an em-dash sanitizer pass already broke every Rekor
// checkpoint verification once. The failure is silent, the code still
// compiles, and only signature parsing stops working.
func TestTheCheckpointSignaturePrefixIsLeftAlone(t *testing.T) {
	src, err := os.ReadFile("rekorproof.go")
	if err != nil {
		t.Fatalf("read rekorproof.go: %v", err)
	}
	const decl = `const sigLinePrefix = "` + emDash + ` "`
	if !strings.Contains(string(src), decl) {
		t.Fatalf("rekorproof.go no longer declares %s.\n\n"+
			"This is NOT house style. It is the signature-line prefix of the "+
			"c2sp.org/signed-note format that Sigstore signs. Replacing the em dash "+
			"makes every checkpoint signature fail to parse: the code still compiles, "+
			"the tests that do not use real Sigstore data still pass, and offline "+
			"verification silently stops working. This has already happened once.",
			decl)
	}
}
