package dashboard

import (
	"fmt"
	"strings"

	"github.com/go-pdf/fpdf"
)

// Drawing for the device-security report (compliance_report.go): the weekly PDF's
// "Device security" page and the monthly report. Same palette and type as the weekly
// report so the pages read as one document.

type pdfRGB struct{ r, g, b int }

var (
	secInk   = pdfRGB{0x2c, 0x2c, 0x2b}
	secMuted = pdfRGB{0x77, 0x73, 0x6f}
	secFaint = pdfRGB{0xa0, 0x9c, 0x98}
	secLine  = pdfRGB{0xe6, 0xe5, 0xe3}
	secTrack = pdfRGB{0xef, 0xee, 0xec}
	secCoral = pdfRGB{0xff, 0x65, 0x4f}
	secOK    = pdfRGB{0x2f, 0x9e, 0x6a}
	secWarn  = pdfRGB{0xd9, 0x8e, 0x1a}
)

func secText(pdf *fpdf.Fpdf, c pdfRGB) { pdf.SetTextColor(c.r, c.g, c.b) }
func secFill(pdf *fpdf.Fpdf, c pdfRGB) { pdf.SetFillColor(c.r, c.g, c.b) }
func secDraw(pdf *fpdf.Fpdf, c pdfRGB) { pdf.SetDrawColor(c.r, c.g, c.b) }

// secMark draws the line's mark centred at (cx, cy): a green tick when the statement
// held, an amber "!" when it did not, a grey dash when nothing was measured. Drawn rather
// than typed, because core Helvetica has no tick.
func secMark(pdf *fpdf.Fpdf, cx, cy float64, l compLine) {
	const r = 2.4
	switch {
	case l.Measured == 0:
		secFill(pdf, secTrack)
		pdf.Circle(cx, cy, r, "F")
		pdf.SetDrawColor(0xa0, 0x9c, 0x98)
		pdf.SetLineWidth(0.5)
		pdf.Line(cx-1.1, cy, cx+1.1, cy)
	case l.AllClean():
		secFill(pdf, secOK)
		pdf.Circle(cx, cy, r, "F")
		pdf.SetDrawColor(0xff, 0xff, 0xff)
		pdf.SetLineWidth(0.5)
		pdf.SetLineCapStyle("round")
		pdf.Line(cx-1.1, cy+0.05, cx-0.3, cy+0.85)
		pdf.Line(cx-0.3, cy+0.85, cx+1.2, cy-0.8)
	default:
		secFill(pdf, secWarn)
		pdf.Circle(cx, cy, r, "F")
		pdf.SetFont("Helvetica", "B", 8)
		pdf.SetTextColor(0xff, 0xff, 0xff)
		pdf.SetXY(cx-r, cy-2)
		pdf.CellFormat(2*r, 4, "!", "", 0, "C", false, 0, "")
	}
}

// secLines draws the three statements from the current Y, each with its mark and a
// right-hand count ("13 of 14 devices", "31 of 31 days").
func secLines(pdf *fpdf.Fpdf, tr func(string) string, x, w float64, lines []compLine, days bool) {
	const rowH = 10.0
	for i, l := range lines {
		y := pdf.GetY()
		secMark(pdf, x+2.4, y+rowH/2, l)
		pdf.SetXY(x+8, y)
		pdf.SetFont("Helvetica", "", 11)
		secText(pdf, secInk)
		pdf.CellFormat(w-8-45, rowH, tr(l.Text()), "", 0, "L", false, 0, "")
		pdf.SetFont("Helvetica", "", 10)
		secText(pdf, secMuted)
		count := "not measured"
		switch {
		case days && l.DaysMeasured > 0:
			count = fmt.Sprintf("%d of %d day%s", l.DaysClean, l.DaysMeasured, plural(l.DaysMeasured))
		case !days && l.Measured > 0:
			count = fmt.Sprintf("%d of %d device%s", l.Clean, l.Measured, plural(l.Measured))
		}
		pdf.CellFormat(45, rowH, tr(count), "", 0, "R", false, 0, "")
		if i < len(lines)-1 {
			secDraw(pdf, secLine)
			pdf.SetLineWidth(0.25)
			pdf.Line(x, y+rowH, x+w, y+rowH)
		}
		pdf.SetXY(x, y+rowH)
	}
}

// secFinePrint is the note under the statements: how it is measured and what was left out.
func secFinePrint(v venueSecurity) string {
	s := "Measured by AIO MDM every minute. A line counts a device only while it could be measured; " +
		"nothing unmeasured is counted as passed."
	if v.Unmeasured > 0 {
		s += fmt.Sprintf(" %d of %d device%s could not be measured on every line (older software, or no rule that applies), "+
			"so %s left out of those lines.", v.Unmeasured, v.Devices, plural(v.Devices), map[bool]string{true: "they are", false: "it is"}[v.Unmeasured != 1])
	}
	return s
}

// drawWeeklySecurityPage adds the weekly PDF's "Device security" page.
func drawWeeklySecurityPage(pdf *fpdf.Fpdf, tr func(string) string, venue string, v venueReport, margin, contentW float64) {
	pdf.AddPage()
	pdf.SetFont("Helvetica", "B", 8)
	secText(pdf, secCoral)
	pdf.CellFormat(contentW, 4, "DEVICE SECURITY", "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "B", 20)
	secText(pdf, secInk)
	pdf.CellFormat(contentW, 10, tr(venue), "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 10)
	secText(pdf, secMuted)
	pdf.CellFormat(contentW, 6, tr(reportRange(v.Win)), "", 1, "L", false, 0, "")
	pdf.Ln(6)

	w := contentW * 0.62
	secLines(pdf, tr, margin, w, v.Security.Lines, false)
	pdf.Ln(5)
	pdf.SetX(margin)
	pdf.SetFont("Helvetica", "", 8)
	secText(pdf, secFaint)
	pdf.MultiCell(w, 4, tr(secFinePrint(*v.Security)), "", "L", false)
}

// secHash is what the security page shows, for the PDF cache key.
func secHash(v *venueSecurity) string {
	if v == nil {
		return "off"
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d/%d", v.Devices, v.Unmeasured)
	for _, l := range v.Lines {
		fmt.Fprintf(&b, "|%s:%d/%d:%d/%d", l.Statement.Key, l.Clean, l.Measured, l.DaysClean, l.DaysMeasured)
	}
	return b.String()
}
