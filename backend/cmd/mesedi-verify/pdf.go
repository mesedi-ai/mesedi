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
//     document. Only the verdict WORD is coloured, and only on failure —
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
	FooterID    string
}

// buildPDFDoc turns a report into the document to be drawn. Pure: no
// clock, no filesystem, no network, so its tests assert on content
// rather than on pixels.
func buildPDFDoc(rep report, exportSHA string) pdfDoc {
	d := pdfDoc{
		Failed:   !rep.OK,
		Verdict:  "VERIFIED",
		FooterID: fmt.Sprintf("mesedi-verify %s  |  export %s", rep.Verifier, shorten(exportSHA)),
	}
	if !rep.OK {
		d.Verdict = "FAILED"
	}

	// The note beside the verdict is where the honest limits of a PASS
	// get stated, not in a footnote. "Intact" and "correct" are different
	// claims and the difference is the whole product.
	switch {
	case !rep.OK:
		d.VerdictNote = "One or more checks did not pass. The findings are listed below. " +
			"Do not treat this record as reliable evidence until each failure is explained."
	case rep.Online:
		d.VerdictNote = "The record is internally consistent and every checkpoint was found " +
			"in the public transparency log at the position it claims. This says the record " +
			"is intact. It does not say the AI was correct."
	default:
		d.VerdictNote = "The record is internally consistent. The transparency log was NOT " +
			"contacted, so this run does not show the record was ever published. It is a " +
			"structural check, not evidence."
	}

	logLine := "not contacted (offline run)"
	if rep.Online {
		logLine = "consulted"
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

	if rep.Online {
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
				Mark: mark(e.OK), Label: label, Detail: e.Detail,
			})
		}
		d.Sections = append(d.Sections, logs)
	}

	// Carried verbatim from the verification result. Rewording them here
	// would create a second, softer version of the caveats that only the
	// PDF reader sees, which is the failure mode this field exists to
	// prevent.
	d.Limits = append(d.Limits, rep.Structural.Unverified...)

	return d
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
	w.closing()

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

func (w *pdfWriter) closing() {
	p := w.pdf
	if p.GetY() > 250 {
		p.AddPage()
	}
	p.SetFont("Helvetica", "I", 8)
	p.SetTextColor(90, 90, 90)
	p.MultiCell(pdfContentW, 4,
		w.tr("To check this report rather than believe it: hash the export file with "+
			"`shasum -a 256` and confirm it matches the Export SHA-256 above, then run "+
			"`mesedi-verify <export>` yourself. Disagreement between your run and this "+
			"document is a finding."),
		"", "L", false)
}
