package dashboard

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Weekly report PDFs.
//
// The PDF is rendered off-box — a Playwright job on a workstation opens the report page
// and prints it — and POSTed here. This server never runs a browser: the container is a
// bare alpine with ca-certificates, the app has a 512 MB cap, and the one time this
// database ran out of memory it took Postgres down with it. A render that spikes several
// hundred megabytes belongs somewhere with headroom, and a weekly job has no reason to
// live in the request path.
//
// The stored file is served from an unguessable URL with no login, so a venue owner can
// open it straight from their email. That is a deliberate trade: the link IS the
// credential, and anyone it is forwarded to can read that venue's report. It carries
// one venue's operational summary for one finished week — no device controls, no
// personal data, nothing that can be acted on — and requiring a dashboard account would
// mean the people the report is written for could not read it.

// ReportStoreDir is where rendered report PDFs live. Under /app/data in production,
// which is a persistent volume; the files are regenerated weekly, so losing them costs
// a re-render and nothing else.
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
// storing means the uploader and the emailer arrive at the same URL without a table to
// keep in step, and a rotated secret invalidates every old link at once.
func (h *Handler) reportToken(restaurantID uuid.UUID, weekEnd time.Time) string {
	mac := hmac.New(sha256.New, []byte(h.reportSecret))
	fmt.Fprintf(mac, "report|%s|%s", restaurantID, weekEnd.UTC().Format("2006-01-02"))
	return base64.RawURLEncoding.EncodeToString(mac.Sum(nil))[:32]
}

// ReportPDFURL is the absolute link to a venue's report for a given week.
func (h *Handler) ReportPDFURL(r *http.Request, restaurantID uuid.UUID, weekEnd time.Time) string {
	return fmt.Sprintf("%s/reports/%s.pdf", h.baseURL(r), h.reportToken(restaurantID, weekEnd))
}

// reportPDFMaxBytes caps an upload. A week's report is a handful of pages; anything far
// past this is a mistake or an attempt to fill the volume.
const reportPDFMaxBytes = 32 << 20 // 32 MB

// ReportPDFUpload stores a rendered PDF for one venue and week. Admin-API-key only:
// the renderer is a scheduled job, not a person, and this writes to disk.
//
//	POST /api/v1/restaurants/{id}/report.pdf?week=YYYY-MM-DD
func (h *Handler) ReportPDFUpload(w http.ResponseWriter, r *http.Request) {
	id, err := uuid.Parse(r.PathValue("id"))
	if err != nil {
		http.Error(w, "invalid restaurant id", http.StatusBadRequest)
		return
	}
	weekEnd, err := time.Parse("2006-01-02", strings.TrimSpace(r.URL.Query().Get("week")))
	if err != nil {
		http.Error(w, "week must be YYYY-MM-DD (the Sunday the report ends on)", http.StatusBadRequest)
		return
	}
	body, err := io.ReadAll(io.LimitReader(r.Body, reportPDFMaxBytes+1))
	if err != nil {
		http.Error(w, "read failed", http.StatusBadRequest)
		return
	}
	if len(body) > reportPDFMaxBytes {
		http.Error(w, "too large", http.StatusRequestEntityTooLarge)
		return
	}
	// Check it is actually a PDF rather than trusting the caller: this file is served
	// back to browsers, and a mislabelled upload would be served as one.
	if len(body) < 5 || string(body[:5]) != "%PDF-" {
		http.Error(w, "body is not a PDF", http.StatusUnsupportedMediaType)
		return
	}

	dir := ReportStoreDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		log.Printf("[report-pdf] mkdir %s: %v", dir, err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	name := h.reportToken(id, weekEnd) + ".pdf"
	// Write to a temporary file and rename, so a reader never sees a half-written PDF.
	tmp, err := os.CreateTemp(dir, ".upload-*")
	if err != nil {
		log.Printf("[report-pdf] temp: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tmpName := tmp.Name()
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		os.Remove(tmpName)
		log.Printf("[report-pdf] write: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	tmp.Close()
	if err := os.Rename(tmpName, filepath.Join(dir, name)); err != nil {
		os.Remove(tmpName)
		log.Printf("[report-pdf] rename: %v", err)
		http.Error(w, "internal error", http.StatusInternalServerError)
		return
	}
	log.Printf("[report-pdf] stored %s for week ending %s (%d bytes)", id, weekEnd.Format("2006-01-02"), len(body))
	w.WriteHeader(http.StatusNoContent)
}

// ReportPDFServe returns a stored report. No session: the token is the credential (see
// the note at the top of this file).
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
