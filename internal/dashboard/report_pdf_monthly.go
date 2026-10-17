package dashboard

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/google/uuid"

	"mdm/internal/db"
)

// ── The monthly device-security report (compliance P4, R1) ────────────────────
//
// One A4 page per venue per month: the three statements as days held, a bar per week,
// and what happened. Rendered on request like the weekly PDF, but not cached — it is
// one query and a page of drawing. The login-free link answers only while customer
// compliance reports are switched on, so nothing reaches a customer before the wording
// is signed off; staff read it from the venue page either way.

// complianceMonthsBack is how far back a monthly report can be opened. Compliance events
// are kept 400 days (RunCompliance), so a year of months is always fully covered.
const complianceMonthsBack = 12

// complianceMonth resolves a "YYYY-MM" month in the venue's time zone: its start and the
// start of the next month. A future month, or one older than complianceMonthsBack, is not
// one anyone could have been sent.
func complianceMonth(month string, loc *time.Location, now time.Time) (from, to time.Time, ok bool) {
	t, err := time.ParseInLocation("2006-01", month, loc)
	if err != nil {
		return time.Time{}, time.Time{}, false
	}
	n := now.In(loc)
	thisMonth := time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc)
	if t.After(thisMonth) || t.Before(thisMonth.AddDate(0, -complianceMonthsBack, 0)) {
		return time.Time{}, time.Time{}, false
	}
	return t, t.AddDate(0, 1, 0), true
}

// lastMonth is the month before now's, in loc, as "YYYY-MM".
func lastMonth(now time.Time, loc *time.Location) string {
	n := now.In(loc)
	return time.Date(n.Year(), n.Month(), 1, 0, 0, 0, 0, loc).AddDate(0, -1, 0).Format("2006-01")
}

// complianceMonthToken derives the monthly report's link token: its own prefix, so a
// weekly report's token can never open a monthly one or the other way round.
func (h *Handler) complianceMonthToken(restaurantID uuid.UUID, month string) string {
	mac := hmac.New(sha256.New, []byte(h.reportSecret))
	fmt.Fprintf(mac, "compliance-month|%s|%s", restaurantID, month)
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))[:32]
}

// complianceMonthURL is the login-free link to a venue's monthly report.
func (h *Handler) complianceMonthURL(base string, restaurantID uuid.UUID, month string) string {
	return fmt.Sprintf("%s/reports/%s/month/%s/%s.pdf", strings.TrimRight(base, "/"), restaurantID, month,
		h.complianceMonthToken(restaurantID, month))
}

// ComplianceMonthPDFToken serves the monthly report a report email links to.
//
//	GET /reports/{id}/month/{month}/{token}.pdf
func (h *Handler) ComplianceMonthPDFToken(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	month := r.PathValue("month")
	tok := strings.TrimSuffix(r.PathValue("token"), ".pdf")
	// One answer for every failure, including the switch being off: a different one
	// would tell a guesser which part of the URL was wrong.
	if err != nil || !h.cfg.ComplianceReportsCustomers() || !reportTokenRe.MatchString(tok) ||
		!hmac.Equal([]byte(tok), []byte(h.complianceMonthToken(id, month))) {
		http.NotFound(w, r)
		return
	}
	h.serveComplianceMonthPDF(w, r, id, month, nil)
}

// RestaurantCompliancePDF is the venue page's monthly report, filtered to the devices
// this viewer may see. Defaults to last month.
//
//	GET /restaurants/{id}/compliance.pdf?month=YYYY-MM
func (h *Handler) RestaurantCompliancePDF(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	h.serveComplianceMonthPDF(w, r, id, r.URL.Query().Get("month"), h.reportViewerFilter(r))
}

func (h *Handler) serveComplianceMonthPDF(w http.ResponseWriter, r *http.Request, id uuid.UUID, month string, visible func(uuid.UUID) bool) {
	rest, err := h.db.GetRestaurant(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	loc := venueLoc(rest)
	if month == "" {
		month = lastMonth(time.Now(), loc)
	}
	body, err := h.complianceMonthPDF(r.Context(), rest, month, visible)
	if err == errNoSuchMonth {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		log.Printf("[compliance-pdf] %s %s: %v", id, month, err)
		http.Error(w, "Could not build the report", http.StatusInternalServerError)
		return
	}
	name := strings.Map(func(c rune) rune {
		if strings.ContainsRune(`\/:*?"<>|`, c) || c < 0x20 || c > 0x7e {
			return '-'
		}
		return c
	}, fmt.Sprintf("%s device security %s.pdf", rest.Name, month))
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="`+name+`"`)
	// Private, because the URL is the credential (see report_pdf.go).
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(body)
}

var errNoSuchMonth = fmt.Errorf("no such month")

// complianceMonthPDF builds and draws a venue's month.
func (h *Handler) complianceMonthPDF(ctx context.Context, rest *db.Restaurant, month string, visible func(uuid.UUID) bool) ([]byte, error) {
	loc := venueLoc(rest)
	now := time.Now()
	from, to, ok := complianceMonth(month, loc, now)
	if !ok {
		return nil, errNoSuchMonth
	}
	v, err := h.loadVenueSecurity(ctx, rest, from, to, visible)
	if err != nil {
		return nil, err
	}
	return renderComplianceMonthPDF(rest.Name, v, to.After(now), now)
}

// renderComplianceMonthPDF draws the monthly report: A4 portrait, the weekly report's
// palette and type.
func renderComplianceMonthPDF(venue string, v venueSecurity, inProgress bool, generated time.Time) ([]byte, error) {
	const margin = 16.0
	pdf := fpdf.New("P", "mm", "A4", "")
	pdf.SetMargins(margin, margin, margin)
	pdf.SetAutoPageBreak(false, margin)
	pdf.SetCreator("AIO MDM", false)
	pdf.SetTitle(venue+" device security report", true)
	pdf.SetCreationDate(generated)
	pdf.AliasNbPages("{nb}")
	tr := pdf.UnicodeTranslatorFromDescriptor("")
	pageW, pageH := pdf.GetPageSize()
	contentW := pageW - 2*margin
	bottom := pageH - margin - 6
	loc := v.Loc
	if loc == nil {
		loc = time.UTC
	}

	pdf.SetFooterFunc(func() {
		pdf.SetY(pageH - margin - 2)
		pdf.SetFont("Helvetica", "", 7.5)
		secText(pdf, secFaint)
		pdf.CellFormat(contentW/2, 4, tr("AIO MDM · generated "+generated.UTC().Format("Mon 2 Jan 2006 15:04")+" UTC"), "", 0, "L", false, 0, "")
		pdf.CellFormat(contentW/2, 4, fmt.Sprintf("Page %d of {nb}", pdf.PageNo()), "", 0, "R", false, 0, "")
	})
	pdf.AddPage()

	// Header.
	pdf.SetFont("Helvetica", "B", 8)
	secText(pdf, secCoral)
	pdf.CellFormat(contentW, 4, "MONTHLY DEVICE SECURITY REPORT", "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "B", 20)
	secText(pdf, secInk)
	pdf.CellFormat(contentW, 10, tr(venue), "", 1, "L", false, 0, "")
	pdf.SetFont("Helvetica", "", 10)
	secText(pdf, secMuted)
	sub := fmt.Sprintf("%s · %d device%s", v.From.In(loc).Format("January 2006"), v.Devices, plural(v.Devices))
	if inProgress {
		sub += " · month in progress, figures still change"
	}
	pdf.CellFormat(contentW, 6, tr(sub), "", 1, "L", false, 0, "")
	pdf.Ln(7)

	heading := func(s string) {
		pdf.SetX(margin)
		pdf.SetFont("Helvetica", "B", 7.5)
		secText(pdf, secMuted)
		pdf.CellFormat(contentW, 5, upper(s), "", 1, "L", false, 0, "")
		pdf.Ln(1)
	}

	heading("This month")
	secLines(pdf, tr, margin, contentW, v.Lines, true)
	pdf.Ln(3)
	pdf.SetX(margin)
	pdf.SetFont("Helvetica", "", 8)
	secText(pdf, secFaint)
	pdf.MultiCell(contentW, 4, tr(secFinePrint(v)), "", "L", false)
	pdf.Ln(7)

	// Week by week: the share of each week's measured days on which every line held.
	heading("Week by week")
	if len(v.Weeks) > 0 {
		const gap, barH = 5.0, 24.0
		n := float64(len(v.Weeks))
		bw := (contentW - gap*(n-1)) / n
		y := pdf.GetY()
		for i, wk := range v.Weeks {
			x := margin + float64(i)*(bw+gap)
			secFill(pdf, secTrack)
			pdf.RoundedRect(x, y, bw, barH, 1.5, "1234", "F")
			if wk.Days > 0 {
				h := barH * float64(max(wk.Pct, 4)) / 100
				if wk.AllDaysClean {
					secFill(pdf, secOK)
				} else {
					secFill(pdf, secWarn)
				}
				pdf.RoundedRect(x, y+barH-h, bw, h, 1.5, "1234", "F")
			}
			pdf.SetXY(x, y+barH+1)
			pdf.SetFont("Helvetica", "", 7.5)
			secText(pdf, secFaint)
			pdf.CellFormat(bw/2, 5, tr(wk.From.In(loc).Format("2 Jan")), "", 0, "L", false, 0, "")
			pdf.SetFont("Helvetica", "B", 8)
			secText(pdf, secMuted)
			val := "-"
			if wk.Days > 0 {
				val = fmt.Sprintf("%d/%d days", wk.Clean, wk.Days)
			}
			pdf.CellFormat(bw/2, 5, tr(val), "", 0, "R", false, 0, "")
		}
		pdf.SetY(y + barH + 12)
	}

	heading("What happened, and what we did")
	pdf.SetFont("Helvetica", "", 9.5)
	secText(pdf, secInk)
	if len(v.Exceptions) == 0 {
		pdf.SetX(margin)
		pdf.MultiCell(contentW, 5, tr("Nothing to report: every line held all month."), "", "L", false)
	}
	const maxListed = 30
	for i, e := range v.Exceptions {
		if i == maxListed {
			pdf.SetX(margin)
			secText(pdf, secMuted)
			pdf.MultiCell(contentW, 5, tr(fmt.Sprintf("… and %d more.", len(v.Exceptions)-maxListed)), "", "L", false)
			break
		}
		if pdf.GetY()+6 > bottom {
			pdf.AddPage()
		}
		y := pdf.GetY()
		secFill(pdf, secFaint)
		pdf.Circle(margin+1.2, y+2.6, 0.6, "F")
		pdf.SetXY(margin+4, y)
		pdf.MultiCell(contentW-4, 5, tr(e.Sentence(loc)), "", "L", false)
		pdf.Ln(0.8)
	}

	var buf bytes.Buffer
	if err := pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}
