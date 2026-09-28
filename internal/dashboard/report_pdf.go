package dashboard

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"mdm/internal/db"

	"github.com/google/uuid"
)

// Weekly report PDFs.
//
// Rendered here, by the server, in Go (go-pdf/fpdf) — no browser. A headless Chromium
// is what kept this off the box before: the container is a bare alpine with a 512 MB
// cap, and the one time this database ran out of memory it took Postgres down with it.
// Drawing the PDF directly costs a few milliseconds and a few hundred kilobytes, so it
// happens on request, and the result is cached on disk until the week's rollups change.
//
// The tokenised link opens with no login, so a venue owner can read it straight from
// their email. That is a deliberate trade: the link IS the credential, and anyone it is
// forwarded to can read that venue's report. It carries one venue's operational summary
// for one week — no device controls, no personal data, nothing that can be acted on —
// and requiring a dashboard account would mean the people the report is written for
// could not read it.

// ReportStoreDir is where report PDFs live: the render cache under cache/, and the
// files the old off-box renderer uploaded. Under /app/data in production, a persistent
// volume; losing it costs a re-render and nothing else.
func ReportStoreDir() string {
	if d := strings.TrimSpace(os.Getenv("REPORT_DIR")); d != "" {
		return d
	}
	return "data/reports"
}

// reportTokenRe guards the path segment before it is ever joined to a directory. The
// token is our own base64url, so anything outside that alphabet is not a token we
// issued and must not reach the filesystem.
var reportTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{16,64}$`)

// reportToken derives the unguessable path for one venue's week. Deriving rather than
// storing means there is no table to keep in step, and a rotated secret invalidates
// every old link at once. New links key it on the week's Monday; the old stored files
// were keyed on its Sunday.
func (h *Handler) reportToken(restaurantID uuid.UUID, day time.Time) string {
	mac := hmac.New(sha256.New, []byte(h.reportSecret))
	fmt.Fprintf(mac, "report|%s|%s", restaurantID, day.UTC().Format("2006-01-02"))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))[:32]
}

// ReportPDFURL is the absolute, login-free link to a venue's report for the week
// starting on weekStart (a Monday).
func (h *Handler) ReportPDFURL(r *http.Request, restaurantID uuid.UUID, weekStart time.Time) string {
	return fmt.Sprintf("%s/reports/%s/%s/%s.pdf", h.baseURL(r), restaurantID,
		weekStart.Format("2006-01-02"), h.reportToken(restaurantID, weekStart))
}

// reportWindowFor is the window for the week starting on a Monday: the whole week if
// it has finished, Monday to today if it is in progress. A future week, or a date
// that is not a Monday, is not a week anyone was sent.
func reportWindowFor(monday string, now time.Time) (reportWindow, bool) {
	from, err := time.Parse("2006-01-02", monday)
	if err != nil || from.Weekday() != time.Monday {
		return reportWindow{}, false
	}
	today := now.UTC().Truncate(24 * time.Hour)
	if from.After(today) {
		return reportWindow{}, false
	}
	to := from.AddDate(0, 0, 6)
	if !to.Before(today) {
		return reportWindow{From: from, To: today, Days: int(today.Sub(from).Hours()/24) + 1, Current: true}, true
	}
	return reportWindow{From: from, To: to, Days: 7}, true
}

// ReportPDFToken serves the login-free PDF a report email and the owner's home page
// link to. The token is checked before anything is read.
//
//	GET /reports/{id}/{week}/{token}.pdf
func (h *Handler) ReportPDFToken(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	tok := strings.TrimSuffix(r.PathValue("token"), ".pdf")
	win, ok := reportWindowFor(r.PathValue("week"), time.Now())
	// One answer for every failure: a different one would tell a guesser which part
	// of the URL was wrong.
	if err != nil || !ok || !reportTokenRe.MatchString(tok) ||
		!hmac.Equal([]byte(tok), []byte(h.reportToken(id, win.From))) {
		http.NotFound(w, r)
		return
	}
	h.serveReportPDF(w, r, id, win, nil)
}

// RestaurantReportPDF is the dashboard's Save as PDF: the week picked on the page,
// filtered to the devices this viewer may see.
//
//	GET /restaurants/{id}/report.pdf?week=YYYY-MM-DD
func (h *Handler) RestaurantReportPDF(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "Invalid restaurant ID", http.StatusBadRequest)
		return
	}
	win, _ := pickReportWeek(r.URL.Query().Get("week"), time.Now())
	h.serveReportPDF(w, r, id, win, h.reportViewerFilter(r))
}

func (h *Handler) serveReportPDF(w http.ResponseWriter, r *http.Request, id uuid.UUID, win reportWindow, visible func(uuid.UUID) bool) {
	rest, err := h.db.GetRestaurant(r.Context(), id)
	if err != nil {
		http.NotFound(w, r)
		return
	}
	body, err := h.venueReportPDF(r.Context(), rest, win, visible)
	if err != nil {
		log.Printf("[report-pdf] %s %s: %v", id, win.Value(), err)
		http.Error(w, "Could not build the report", http.StatusInternalServerError)
		return
	}
	name := fmt.Sprintf("%s weekly report %s.pdf", rest.Name, win.From.Format("2006-01-02"))
	name = strings.Map(func(c rune) rune {
		if strings.ContainsRune(`\/:*?"<>|`, c) || c < 0x20 || c > 0x7e {
			return '-'
		}
		return c
	}, name)
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="`+name+`"`)
	// Private, because the URL is the credential and a shared cache must not hand it to
	// someone who did not have the link. Short, because the week in progress changes.
	w.Header().Set("Cache-Control", "private, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	w.Write(body)
}

// reportPDFLayout is part of every cache key, so a change to the drawing below makes
// every cached file stale at once. Bump it with any change to renderReportPDF.
const reportPDFLayout = "2"

// venueReportPDF returns the venue's PDF for a week, from the disk cache when the
// week's rollups have not changed since it was drawn.
//
// Only the unfiltered report is cached. A viewer who may see just some of a venue's
// devices gets one drawn for them, which is rare and cheap, rather than a cache keyed
// on every access policy.
func (h *Handler) venueReportPDF(ctx context.Context, rest *db.Restaurant, win reportWindow, visible func(uuid.UUID) bool) ([]byte, error) {
	if visible != nil {
		v, err := h.buildVenueReport(ctx, rest.ID, win, visible)
		if err != nil {
			return nil, err
		}
		return renderReportPDF(rest.Name, v, time.Now())
	}
	stamp, rows, err := h.db.DailyStatsStamp(ctx, rest.ID, win.Days, win.To)
	if err != nil {
		return nil, err
	}
	// The key is whatever the PDF shows: the venue's name, the window, and the newest
	// rollup inside it — the hourly rollup rewrites the week in progress, a recompute
	// rewrites a finished one, and either moves computed_at.
	sum := sha256.Sum256([]byte(fmt.Sprintf("%s|%s|%d|%d|%d", reportPDFLayout, rest.Name,
		win.Days, stamp.UnixNano(), rows)))
	prefix := fmt.Sprintf("%s-%s-", rest.ID, win.Value())
	dir := filepath.Join(ReportStoreDir(), "cache")
	path := filepath.Join(dir, prefix+hex.EncodeToString(sum[:8])+".pdf")
	if b, err := os.ReadFile(path); err == nil {
		return b, nil
	}

	v, err := h.buildVenueReport(ctx, rest.ID, win, nil)
	if err != nil {
		return nil, err
	}
	b, err := renderReportPDF(rest.Name, v, time.Now())
	if err != nil {
		return nil, err
	}
	// A cache that cannot be written is not a failed request.
	if err := writeReportCache(dir, prefix, path, b); err != nil {
		log.Printf("[report-pdf] cache %s: %v", path, err)
	}
	return b, nil
}

// writeReportCache stores a PDF and drops the older copies of the same venue-week,
// so the week in progress leaves one file behind, not one per hourly rollup.
func writeReportCache(dir, prefix, path string, b []byte) error {
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	if old, err := filepath.Glob(filepath.Join(dir, prefix+"*.pdf")); err == nil {
		for _, f := range old {
			os.Remove(f)
		}
	}
	// Write and rename, so a concurrent reader never sees a half-written file.
	tmp, err := os.CreateTemp(dir, ".render-*")
	if err != nil {
		return err
	}
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		os.Remove(tmp.Name())
		return err
	}
	tmp.Close()
	if err := os.Rename(tmp.Name(), path); err != nil {
		os.Remove(tmp.Name())
		return err
	}
	return nil
}

// ReportPDFServe returns a PDF stored by the old off-box renderer, so the links in the
// emails it went out with keep working. Nothing writes these any more. No session: the
// token is the credential (see the note at the top of this file).
//
//	GET /reports/{token}.pdf
func (h *Handler) ReportPDFServe(w http.ResponseWriter, r *http.Request) {
	tok := strings.TrimSuffix(r.PathValue("token"), ".pdf")
	if !reportTokenRe.MatchString(tok) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(ReportStoreDir(), tok+".pdf"))
	if err != nil {
		// Same answer for "never rendered" and "wrong token": a different one would
		// tell a guesser which tokens exist.
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", `inline; filename="weekly-report.pdf"`)
	// Private, because the URL is the credential and a shared cache must not hand it to
	// someone who did not have the link.
	w.Header().Set("Cache-Control", "private, max-age=3600")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "weekly-report.pdf", st.ModTime(), f)
}
