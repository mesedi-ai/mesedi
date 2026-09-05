package main

import (
	"bytes"
	"fmt"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
)

// The PDF report.
//
// WHY THIS LIVES IN THE VERIFIER AND NOT IN THE MESEDI SERVER
//
// A PDF that Mesedi generates, saying Mesedi's records are intact, is
// Mesedi asserting again with better typography. It is exactly the thing
// the rest of this binary exists to remove. The document is worth
// something only because the party that produced it is not the party
// under audit: the reader can rebuild this binary from source, run it
// against the same export, and get the same verdict.
//
// So the PDF is emitted here, by the open-source verifier, and it is
// deliberately UNBRANDED. No logo, no colour scheme, nothing that makes
// it look like a certificate issued by a vendor. Its credibility is
// supposed to come from being reproducible, not from looking official.
//
// TWO DESIGN RULES THAT ARE NOT COSMETIC
//
//  1. The verdict band is identical in size and position whether the
//     result is VERIFIED or FAILED, and the limits section is set in the
//     same type as the results. A report that made passing look good and
//     buried the caveats in small print at the back would be a sales
//     document. Only the verdict WORD is coloured, and only on failure ,
//     emphasis for a bad outcome is a safety convention; visual reward
//     for a good one is persuasion.
//
//  2. The SHA-256 of the export file appears in the subject block and in
//     the footer of every page. Without it the PDF is a floating verdict
//     that could be presented next to any export at all. With it, the
//     reader can hash the JSON they were handed and confirm this report
//     is about that file and no other.
//
// The rendering dependency (fpdf) cannot influence the verdict. It
// receives a finished pdfDoc and draws it. That separation is why the
// content assembly below is a pure function with its own tests: the part
// an auditor must trust stays testable without parsing PDF bytes.

// pdfRow is one line of a results table. Mark is printed verbatim, so
// the PDF and the terminal report cannot drift into saying different
// things about the same check.
type pdfRow struct {
	Mark   string
	Label  string
	Detail string
}

type pdfSection struct {
	Title string
	Rows  []pdfRow
	// Empty is what to print when a section has no rows. Silence would
	// read as "nothing to report" when it may mean "nothing was checked".
	Empty string
}

type pdfDoc struct {
	Verdict     string // "VERIFIED" or "FAILED"
	VerdictNote string
	Failed      bool
	Fields      [][2]string
	Sections    []pdfSection
	Limits      []string

	// HowToCheck is the reader's route to reproducing this verdict
	// WITHOUT us. Built here rather than written into the renderer
	// because step two has to name the exact commit this binary came
	// from, and that is data.
	//
	// It used to be one sentence ending "then run mesedi-verify yourself",
	// which assumes the reader has mesedi-verify. An auditor handed this
	// document has the export and the report and no verifier, and no
	// instruction for obtaining one, so in practice they cannot check
	// and end up trusting us anyway, which is the one outcome this whole
	// report exists to prevent.
	HowToCheck []string

	FooterID string
}

// buildPDFDoc turns a report into the document to be drawn. Pure: no
// clock, no filesystem, no network, so its tests assert on content
// rather than on pixels.
func buildPDFDoc(rep report, exportSHA string) pdfDoc {
	// Three verdicts, matching the terminal report. A checkpoint nobody
	// can check is not a checkpoint that failed, and a document that
	// printed FAILED over an unverifiable chain would accuse Mesedi of
	// something it is not known to have done, a falsehood in the
	// opposite direction from the one this tool exists to prevent.
	var failed, unchecked int
	for _, e := range rep.LogEntries {
		switch {
		case e.failed():
			failed++
		case e.unverifiable():
			unchecked++
		}
	}
	realFailure := failed > 0 || !rep.Structural.OK

	// Counted by METHOD, because the verdict below depends on what was
	// established rather than on whether the network was used. Driving it
	// from rep.Online instead is what made an offline run, including one
	// that proved every checkpoint against Sigstore's signature, print
	// "this run does not show the record was ever published".
	verifiedCount, offlineCount := countVerified(rep.LogEntries)

	d := pdfDoc{
		Failed:   realFailure,
		Verdict:  "VERIFIED",
		FooterID: fmt.Sprintf("mesedi-verify %s  |  export %s", rep.Verifier, shorten(exportSHA)),
	}
	switch {
	case realFailure:
		d.Verdict = "FAILED"
	case unchecked > 0:
		d.Verdict = "INCOMPLETE"
	}

	// The note beside the verdict is where the honest limits of a PASS
	// get stated, not in a footnote. "Intact" and "correct" are different
	// claims and the difference is the whole product.
	switch {
	case realFailure:
		d.VerdictNote = "One or more checks did not pass. The findings are listed " +
			"below. Do not rely on this record until each one is explained."
	case unchecked > 0:
		d.VerdictNote = fmt.Sprintf("The record holds together, but %d checkpoint(s) "+
			"could not be checked against the public log at all. Nothing here says "+
			"those are wrong. Nothing here says they are right. Do not present this "+
			"as a verified record.", unchecked)
	case verifiedCount > 0 && offlineCount == verifiedCount:
		// Proven without the network. The old code reached the `default`
		// branch here and told the reader "the transparency log was NOT
		// contacted, so this run does not show the record was ever
		// published. It is a structural check, not evidence." Every word
		// of that was false for a run whose checkpoints carried inclusion
		// proofs: they were proven present under Sigstore's own signature.
		//
		// Understating a real verification is not the safe direction. This
		// note is the line a procurement officer quotes, and it was
		// discarding the strongest claim the product can make because the
		// verdict was driven by whether the network was used rather than
		// by what was established.
		d.VerdictNote = "The record holds together, and every checkpoint was proven " +
			"present in Sigstore's public log under a summary Sigstore signed. That was " +
			"checked here without contacting Sigstore, so it does not depend on that " +
			"log being reachable or honest today. This says the record is intact. It " +
			"does not say the AI was right."
	case verifiedCount > 0 && offlineCount > 0:
		d.VerdictNote = "The record holds together, and every checkpoint was found in " +
			"Sigstore's public log where it claims to be. Some were proven from a proof " +
			"stored in the file, which checks Sigstore's signature directly. The rest " +
			"were confirmed by asking the log. This says the record is intact. It does " +
			"not say the AI was right."
	case verifiedCount > 0:
		d.VerdictNote = "The record holds together, and every checkpoint was found in " +
			"Sigstore's public log where it claims to be. This says the record is " +
			"intact. It does not say the AI was right."
	default:
		d.VerdictNote = "The record holds together, but nothing was checked against a " +
			"public log, so this does not show the record was ever published. It is a " +
			"consistency check, not evidence."
	}

	// Leads with what WAS established, then how.
	//
	// This read "not consulted, 4 of 4 proven offline from inclusion
	// proofs", which opens on a negative and on jargon: "not consulted"
	// is easily read as "not checked", which is now the opposite of what
	// happened. The reader wants the fact first and the method second.
	// "Consulted" is also stiffer than it needs to be; the log is queried
	// or it is not.
	total := len(rep.LogEntries)
	logLine := "not queried, and no inclusion proofs were available to check"
	switch {
	case verifiedCount > 0 && offlineCount == verifiedCount:
		logLine = fmt.Sprintf(
			"%d of %d checkpoints proven from inclusion proofs, without querying the log",
			offlineCount, total)
	case verifiedCount > 0 && offlineCount > 0:
		logLine = fmt.Sprintf(
			"%d of %d proven from inclusion proofs; the remainder confirmed by querying the log",
			offlineCount, total)
	case verifiedCount > 0:
		logLine = fmt.Sprintf("%d of %d confirmed by querying the log", verifiedCount, total)
	case rep.Online:
		logLine = "queried, but nothing could be confirmed"
	}
	d.Fields = [][2]string{
		{"Project", rep.ProjectID},
		{"Export format", rep.Format},
		{"Export written", stamp(rep.GeneratedAt)},
		{"Export SHA-256", exportSHA},
		{"Verified", stamp(rep.VerifiedAt)},
		{"Verifier", "mesedi-verify " + rep.Verifier + " (open source, MIT)"},
		{"Transparency log", logLine},
	}

	structure := pdfSection{
		Title: "STRUCTURE OF THE RECORD",
		Empty: "No structural checks ran. The export could not be interpreted.",
	}
	for _, c := range rep.Structural.Checks {
		structure.Rows = append(structure.Rows, pdfRow{
			Mark: mark(c.OK), Label: c.Name, Detail: c.Detail,
		})
	}
	d.Sections = append(d.Sections, structure)

	// Gated on there being results, NOT on the run having used the
	// network, the same stale condition that suppressed this section in
	// the text report. It mattered more here: an offline run produced a
	// PDF with no transparency-log section at all, so the copy actually
	// handed to an auditor omitted every per-checkpoint verdict while the
	// limitations page still referred to them.
	if len(rep.LogEntries) > 0 {
		logs := pdfSection{
			Title: "PUBLIC TRANSPARENCY LOG",
			Empty: "The export contained no checkpoints to resolve.",
		}
		for _, e := range rep.LogEntries {
			label := fmt.Sprintf("checkpoint %d", e.Seq)
			if e.LogIndex != "" {
				label += " @ " + e.LogIndex
			}
			logs.Rows = append(logs.Rows, pdfRow{
				Mark: statusMark(e.Status), Label: label, Detail: e.Detail,
			})
		}
		// The finding belongs with the findings. Printing it among the
		// limitations, which is where it used to go, told the reader the
		// report's best result was something the report did not show.
		if rep.LogSummary != "" {
			logs.Rows = append(logs.Rows, pdfRow{
				Mark: statusMark(StatusVerified), Label: "summary",
				Detail: rep.LogSummary,
			})
		}
		d.Sections = append(d.Sections, logs)
	}

	// Carried verbatim from the verification result. Rewording them here
	// would create a second, softer version of the caveats that only the
	// PDF reader sees, which is the failure mode this field exists to
	// prevent.
	d.Limits = append(d.Limits, rep.Structural.Unverified...)

	d.HowToCheck = howToCheck(rep.Verifier)

	return d
}

// howToCheck spells out reproducing this verdict independently.
//
// # THE STEP THAT WAS MISSING, AND WHY IT MATTERED MOST
//
// The old text said "then run mesedi-verify yourself" and stopped. A
// reader holding this PDF has the export and the report and no verifier,
// and nothing here told them where to get one, so the practical outcome
// of "check us, don't trust us" was that they trusted us.
//
// The second instruction is the load-bearing one and it is a refusal: do
// not accept a compiled binary from Mesedi. A verdict produced by a
// program the audited party handed you is that party asserting again. It
// has to be built from source the reader fetched themselves, at the
// commit named in the report, or the independence is decorative.
//
// Deliberately short on shell detail beyond that. Instructions that go
// stale are worse than a pointer, because a command that fails reads as
// the system being broken rather than the document being old.
func howToCheck(verifier string) []string {
	// Only a reproducible build can be pinned to a commit; resolveVersion
	// marks the rest. Telling a reader to check out "unknown" would send
	// them after something that does not exist.
	at := "the commit shown in the Verifier field above"
	if versionIsReproducible(verifier) {
		at = verifier
	}

	return []string{
		"1. Confirm this report is about your file:  shasum -a 256 <export.json>",
		"   It must match the Export SHA-256 above. If not, this report is about a",
		"   different file and says nothing about yours.",
		"",
		"2. Build the verifier yourself, NOT as a binary from Mesedi: a verdict",
		"   from a program the audited party gave you is that party speaking again.",
		"     git clone https://github.com/mesedi-ai/mesedi",
		"     cd mesedi/backend && git checkout " + at,
		"     go build ./cmd/mesedi-verify",
		"",
		"3. Run it on the same file:  ./mesedi-verify --offline <export.json>",
		"   --offline needs no network and is the stronger check. Drop it to also ask",
		"   the log about any checkpoint with no stored proof.",
		"",
		"4. Compare. Any disagreement between your run and this document is a finding,",
		"   and should be treated as one rather than resolved in Mesedi's favour.",
	}
}

func stamp(t time.Time) string {
	if t.IsZero() {
		return "not stated"
	}
	return t.UTC().Format("2006-01-02 15:04:05 UTC")
}

// Page geometry. A4 in millimetres.
const (
	pdfPageW    = 210.0
	pdfMargin   = 18.0
	pdfContentW = pdfPageW - 2*pdfMargin
	pdfMarkW    = 15.0
	pdfLabelW   = 45.0
	pdfLineH    = 4.4
)

type pdfWriter struct {
	pdf *fpdf.Fpdf
	tr  func(string) string
}

// renderPDF draws a finished document. It makes no decisions about
// content; everything it prints came from buildPDFDoc.
func renderPDF(d pdfDoc) ([]byte, error) {
	pdf := fpdf.New("P", "mm", "A4", "")
	// cp1252, which covers the em dashes and quotes that appear in the
	// check details. Without the translator those arrive as mojibake in
	// the one document where precision is the point.
	w := &pdfWriter{pdf: pdf, tr: pdf.UnicodeTranslatorFromDescriptor("")}

	pdf.SetMargins(pdfMargin, pdfMargin, pdfMargin)
	pdf.SetAutoPageBreak(true, 20)
	pdf.AliasNbPages("")
	pdf.SetFooterFunc(func() {
		pdf.SetY(-14)
		pdf.SetFont("Courier", "", 7.5)
		pdf.SetTextColor(110, 110, 110)
		pdf.CellFormat(pdfContentW, 4,
			w.tr(fmt.Sprintf("%s  |  page %d of {nb}", d.FooterID, pdf.PageNo())),
			"T", 0, "C", false, 0, "")
	})
	pdf.AddPage()

	w.title(d)
	w.verdict(d)
	w.fields(d.Fields)
	for _, s := range d.Sections {
		w.section(s)
	}
	w.limits(d.Limits)
	w.closing(d)

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, fmt.Errorf("rendering the PDF: %w", err)
	}
	return buf.Bytes(), nil
}

func (w *pdfWriter) title(d pdfDoc) {
	p := w.pdf
	p.SetTextColor(0, 0, 0)
	p.SetFont("Helvetica", "B", 16)
	p.CellFormat(pdfContentW, 8, w.tr("Chain verification report"), "", 1, "L", false, 0, "")

	p.SetFont("Helvetica", "", 8.5)
	p.SetTextColor(80, 80, 80)
	p.MultiCell(pdfContentW, 4,
		w.tr("Produced by mesedi-verify, an independent open-source verifier that holds "+
			"no Mesedi credentials and does not contact Mesedi. Rebuild it from source "+
			"and run it against the same export to reproduce this result."),
		"", "L", false)
	p.Ln(3)
}

func (w *pdfWriter) verdict(d pdfDoc) {
	p := w.pdf
	y := p.GetY()

	p.SetDrawColor(0, 0, 0)
	p.SetLineWidth(0.5)
	p.Line(pdfMargin, y, pdfMargin+pdfContentW, y)
	p.Ln(2)

	p.SetFont("Helvetica", "B", 22)
	if d.Failed {
		// Colour on failure only. See the file comment.
		p.SetTextColor(150, 20, 20)
	} else {
		p.SetTextColor(0, 0, 0)
	}
	p.CellFormat(pdfContentW, 11, w.tr(d.Verdict), "", 1, "L", false, 0, "")

	p.SetFont("Helvetica", "", 9)
	p.SetTextColor(30, 30, 30)
	p.MultiCell(pdfContentW, 4.4, w.tr(d.VerdictNote), "", "L", false)

	y = p.GetY() + 1
	p.Line(pdfMargin, y, pdfMargin+pdfContentW, y)
	p.Ln(5)
}

func (w *pdfWriter) fields(fields [][2]string) {
	p := w.pdf
	for _, f := range fields {
		p.SetFont("Helvetica", "B", 8.5)
		p.SetTextColor(70, 70, 70)
		p.CellFormat(pdfLabelW, 5, w.tr(f[0]), "", 0, "L", false, 0, "")

		// Courier for the hash so an auditor comparing it character by
		// character against their own `shasum` output is not fighting a
		// proportional font.
		p.SetFont("Courier", "", 8.5)
		p.SetTextColor(0, 0, 0)
		p.MultiCell(pdfContentW-pdfLabelW, 5, w.tr(f[1]), "", "L", false)
	}
	p.Ln(3)
}

func (w *pdfWriter) heading(text string) {
	p := w.pdf
	if p.GetY() > 240 {
		p.AddPage()
	}
	p.SetFont("Helvetica", "B", 10)
	p.SetTextColor(0, 0, 0)
	p.CellFormat(pdfContentW, 6, w.tr(text), "B", 1, "L", false, 0, "")
	p.Ln(1.5)
}

func (w *pdfWriter) section(s pdfSection) {
	p := w.pdf
	w.heading(s.Title)

	if len(s.Rows) == 0 {
		p.SetFont("Helvetica", "I", 8.5)
		p.SetTextColor(90, 90, 90)
		p.MultiCell(pdfContentW, pdfLineH, w.tr(s.Empty), "", "L", false)
		p.Ln(3)
		return
	}

	detailW := pdfContentW - pdfMarkW - pdfLabelW
	for _, r := range s.Rows {
		p.SetFont("Helvetica", "", 8.5)
		lines := len(p.SplitLines([]byte(w.tr(r.Detail)), detailW))
		if lines == 0 {
			lines = 1
		}
		h := float64(lines) * pdfLineH

		// Break before the row, never inside it. A check whose verdict
		// sits on one page and whose explanation sits on the next is how
		// a reader ends up quoting half a finding.
		if p.GetY()+h > 272 {
			p.AddPage()
		}
		y := p.GetY()

		p.SetFont("Courier", "B", 8)
		p.SetTextColor(0, 0, 0)
		p.SetXY(pdfMargin, y)
		p.CellFormat(pdfMarkW, pdfLineH, w.tr(r.Mark), "", 0, "L", false, 0, "")

		p.SetFont("Helvetica", "B", 8.5)
		p.SetTextColor(40, 40, 40)
		p.SetXY(pdfMargin+pdfMarkW, y)
		p.MultiCell(pdfLabelW, pdfLineH, w.tr(r.Label), "", "L", false)

		p.SetFont("Helvetica", "", 8.5)
		p.SetTextColor(20, 20, 20)
		p.SetXY(pdfMargin+pdfMarkW+pdfLabelW, y)
		p.MultiCell(detailW, pdfLineH, w.tr(r.Detail), "", "L", false)

		if end := y + h; p.GetY() < end {
			p.SetY(end)
		}
		p.Ln(1.2)
	}
	p.Ln(2.5)
}

func (w *pdfWriter) limits(limits []string) {
	p := w.pdf
	w.heading("WHAT THIS REPORT DOES NOT SHOW")

	p.SetFont("Helvetica", "", 8.5)
	p.SetTextColor(20, 20, 20)
	if len(limits) == 0 {
		// Should be unreachable: the verifier always states limits. If it
		// ever happens, say so rather than printing an empty section that
		// implies everything was checked.
		p.MultiCell(pdfContentW, pdfLineH,
			w.tr("The verifier reported no limits. That is unexpected and should itself "+
				"be treated as a defect in this report."), "", "L", false)
		return
	}
	for _, l := range limits {
		y := p.GetY()
		lines := len(p.SplitLines([]byte(w.tr(l)), pdfContentW-6))
		if lines == 0 {
			lines = 1
		}
		if y+float64(lines)*pdfLineH > 272 {
			p.AddPage()
			y = p.GetY()
		}
		p.SetXY(pdfMargin, y)
		p.CellFormat(6, pdfLineH, w.tr("-"), "", 0, "L", false, 0, "")
		p.SetXY(pdfMargin+6, y)
		p.MultiCell(pdfContentW-6, pdfLineH, w.tr(strings.TrimSpace(l)), "", "L", false)
		p.Ln(1.2)
	}
	p.Ln(2)
}

func (w *pdfWriter) closing(d pdfDoc) {
	p := w.pdf
	if p.GetY() > 250 {
		p.AddPage()
	}
	p.SetFont("Helvetica", "B", 9)
	p.SetTextColor(0, 0, 0)
	p.MultiCell(pdfContentW, 4.4,
		w.tr("TO CHECK THIS REPORT RATHER THAN BELIEVE IT"), "", "L", false)
	p.Ln(1)

	p.SetFont("Helvetica", "", 8)
	p.SetTextColor(60, 60, 60)
	for _, line := range d.HowToCheck {
		p.MultiCell(pdfContentW, 3.8, w.tr(line), "", "L", false)
	}
}
