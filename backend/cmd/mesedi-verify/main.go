// Command mesedi-verify is the standalone, independent verifier for a
// Mesedi chain export.
//
// It exists so an auditor never has to take Mesedi's word for anything.
// Given an export file, it recomputes every hash from the export's own
// contents, then resolves each checkpoint against a public transparency
// log that neither Mesedi nor Verdifax controls. Mesedi is not contacted
// and no Mesedi credential is used.
//
// USAGE
//
//	mesedi-verify export.json              # verify, print a human report
//	cat export.json | mesedi-verify        # verify from stdin
//	mesedi-verify --json export.json       # machine-readable
//	mesedi-verify --offline export.json    # skip the log; proves less
//	mesedi-verify --rekor <url> export.json
//
// EXIT CODES
//
//	0  Every check that was run passed.
//	1  One or more checks failed.
//	2  The export could not be read or parsed.
//
// # WHY --offline PROVES LESS, AND WHY THE REPORT SAYS SO
//
// Offline, this checks that the export is internally consistent: the
// chain links, the hashes recompute, the executions fold to the anchored
// roots. All of that would also pass for an export a lying Mesedi
// constructed carefully, because Mesedi produced every byte of it. Only
// resolving the log entries breaks that circle. An offline run is useful
// for a quick structural check; it is not evidence, and the report says
// exactly that rather than leaving the reader to infer it.
//
// # LICENSE
//
// MIT (see LICENSE in this directory). The verifier is open source on
// purpose: an auditor must be able to read the code that adjudicates the
// evidence. A closed verifier is the vendor asserting again, one level up.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"mesedi/backend/internal/attest"
)

// buildVersion is set at link time. "unknown" is honest for a local build
// and is never fabricated into something that looks like a release.
var buildVersion = "unknown"

type report struct {
	Format      string                    `json:"format"`
	ProjectID   string                    `json:"project_id"`
	GeneratedAt time.Time                 `json:"export_generated_at"`
	VerifiedAt  time.Time                 `json:"verified_at"`
	Verifier    string                    `json:"verifier_version"`
	Online      bool                      `json:"log_was_consulted"`
	Structural  attest.ExportVerification `json:"structural"`
	LogEntries  []LogEntryCheck           `json:"log_entries,omitempty"`
	OK          bool                      `json:"ok"`
}

func main() {
	var (
		asJSON  = flag.Bool("json", false, "machine-readable output")
		offline = flag.Bool("offline", false,
			"do not contact the transparency log (proves substantially less)")
		rekorURL = flag.String("rekor", DefaultRekorURL, "transparency log base URL")
		version  = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *version {
		fmt.Fprintln(os.Stdout, "mesedi-verify", buildVersion)
		return
	}

	raw, err := readExport(flag.Arg(0))
	if err != nil {
		fmt.Fprintln(os.Stderr, "mesedi-verify:", err)
		os.Exit(2)
	}

	var export attest.ChainExport
	if err := json.Unmarshal(raw, &export); err != nil {
		fmt.Fprintln(os.Stderr, "mesedi-verify: this file is not a chain export:", err)
		os.Exit(2)
	}

	rep := report{
		Format:      export.Format,
		ProjectID:   export.ProjectID,
		GeneratedAt: export.GeneratedAt,
		VerifiedAt:  time.Now().UTC(),
		Verifier:    buildVersion,
		Online:      !*offline,
		Structural:  attest.VerifyChainExport(export),
	}
	rep.OK = rep.Structural.OK

	if *offline {
		rep.Structural.Unverified = append(rep.Structural.Unverified,
			"RUN OFFLINE. The transparency log was not contacted, so this report "+
				"shows only that the export is internally consistent. An export "+
				"constructed to be self-consistent would also pass. Re-run without "+
				"--offline for a result that does not depend on trusting Mesedi.")
	} else {
		rep.LogEntries = resolveLogEntries(export, *rekorURL)
		for _, e := range rep.LogEntries {
			if !e.OK {
				rep.OK = false
			}
		}
		// Replace the library's standing caveat: it is written for the
		// offline case and would now understate what was done.
		rep.Structural.Unverified = replacePrefix(
			rep.Structural.Unverified,
			"Transparency log entries were NOT resolved",
			"Each checkpoint hash was confirmed PRESENT in the public log at the "+
				"index it claims. What was not additionally checked is the log's own "+
				"signed tree head. That further step would guard against Sigstore "+
				"itself being dishonest; it is not what protects you from Mesedi. For "+
				"a hash to be found here at all, Mesedi must genuinely have published "+
				"it to an append-only log it does not control. If Sigstore is "+
				"dishonest, this system's guarantee is gone by design, and that "+
				"premise is stated up front rather than papered over.")
	}

	if *asJSON {
		out, _ := json.MarshalIndent(rep, "", "  ")
		fmt.Fprintln(os.Stdout, string(out))
	} else {
		printReport(os.Stdout, rep)
	}

	if !rep.OK {
		os.Exit(1)
	}
}

func readExport(path string) ([]byte, error) {
	if path == "" || path == "-" {
		raw, err := io.ReadAll(os.Stdin)
		if err != nil {
			return nil, fmt.Errorf("reading stdin: %w", err)
		}
		if len(raw) == 0 {
			return nil, fmt.Errorf("no export given. Pass a file, or pipe one in")
		}
		return raw, nil
	}
	return os.ReadFile(path)
}

// resolveLogEntries asks the log about every checkpoint.
//
// Continues past failures rather than stopping at the first. An auditor
// needs to know whether one checkpoint is missing from the log or all of
// them; those are very different findings and stopping early hides which.
func resolveLogEntries(export attest.ChainExport, rekorURL string) []LogEntryCheck {
	client := newRekorClient(rekorURL)
	ctx := context.Background()

	out := make([]LogEntryCheck, 0, len(export.Intervals))
	for _, iv := range export.Intervals {
		seq := iv.Checkpoint.Seq
		if iv.LogEntryID == "" {
			out = append(out, LogEntryCheck{
				Seq: seq, OK: false,
				Detail: "the export names no log entry for this checkpoint, so it was never published",
			})
			continue
		}

		got, integrated, err := client.lookup(ctx, iv.LogEntryID)
		if err != nil {
			out = append(out, LogEntryCheck{
				Seq: seq, LogIndex: iv.LogEntryID, OK: false, Detail: err.Error(),
			})
			continue
		}
		if !strings.EqualFold(got, iv.Checkpoint.Hash) {
			out = append(out, LogEntryCheck{
				Seq: seq, LogIndex: iv.LogEntryID, OK: false, Integrated: integrated,
				Detail: fmt.Sprintf(
					"the log entry records %s but this checkpoint hashes to %s. The "+
						"published record and the record you were given are not the same",
					shorten(got), shorten(iv.Checkpoint.Hash)),
			})
			continue
		}
		out = append(out, LogEntryCheck{
			Seq: seq, LogIndex: iv.LogEntryID, OK: true, Integrated: integrated,
			Detail: "this checkpoint's hash is present in the public log at the index it claims",
		})
	}
	return out
}

func printReport(w io.Writer, rep report) {
	fmt.Fprintf(w, "Mesedi chain verification\n")
	fmt.Fprintf(w, "  project        %s\n", rep.ProjectID)
	fmt.Fprintf(w, "  export written %s\n", rep.GeneratedAt.Format(time.RFC3339))
	fmt.Fprintf(w, "  verified       %s by mesedi-verify %s\n\n",
		rep.VerifiedAt.Format(time.RFC3339), rep.Verifier)

	fmt.Fprintln(w, "STRUCTURE")
	for _, c := range rep.Structural.Checks {
		fmt.Fprintf(w, "  %s %-34s %s\n", mark(c.OK), c.Name, c.Detail)
	}

	if rep.Online {
		fmt.Fprintln(w, "\nPUBLIC TRANSPARENCY LOG")
		for _, e := range rep.LogEntries {
			label := fmt.Sprintf("checkpoint %d", e.Seq)
			if e.LogIndex != "" {
				label += " @ " + e.LogIndex
			}
			fmt.Fprintf(w, "  %s %-34s %s\n", mark(e.OK), label, e.Detail)
		}
	}

	// Printed for passing runs too, and at the same weight as the results.
	// A report that lists only what passed invites the reader to assume
	// everything was checked.
	fmt.Fprintln(w, "\nWHAT THIS REPORT DOES NOT SHOW")
	for _, u := range rep.Structural.Unverified {
		fmt.Fprintf(w, "  - %s\n", wrap(u, 74, "    "))
	}

	fmt.Fprintln(w)
	if rep.OK {
		fmt.Fprintln(w, "RESULT: every check that ran passed.")
		fmt.Fprintln(w, "The record is intact. That is not the same as the AI having been correct.")
	} else {
		fmt.Fprintln(w, "RESULT: FAILED. See the lines marked FAIL above.")
	}
}

func mark(ok bool) string {
	if ok {
		return "[ ok ]"
	}
	return "[FAIL]"
}

func shorten(h string) string {
	if len(h) <= 16 {
		return h
	}
	return h[:8] + "..." + h[len(h)-8:]
}

func replacePrefix(lines []string, prefix, replacement string) []string {
	out := make([]string, 0, len(lines))
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			out = append(out, replacement)
			continue
		}
		out = append(out, l)
	}
	return out
}

// wrap keeps the limits readable in a terminal. They are the part of the
// report most likely to be skimmed, so they should not arrive as one
// unbroken line.
func wrap(s string, width int, indent string) string {
	var (
		b    strings.Builder
		line int
	)
	for i, word := range strings.Fields(s) {
		if i > 0 {
			if line+1+len(word) > width {
				b.WriteString("\n" + indent)
				line = 0
			} else {
				b.WriteString(" ")
				line++
			}
		}
		b.WriteString(word)
		line += len(word)
	}
	return b.String()
}
