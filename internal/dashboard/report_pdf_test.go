package dashboard

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/google/uuid"
)

func testHandler(secret string) *Handler { return &Handler{reportSecret: secret} }

// TestReportTokenIsStableAndScoped: the uploader and the emailer derive the URL
// independently, so the same venue and week must always give the same token — and a
// different venue or week must not.
func TestReportTokenIsStableAndScoped(t *testing.T) {
	h := testHandler("s3cr3t")
	a, b := uuid.New(), uuid.New()
	wk := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)

	if h.reportToken(a, wk) != h.reportToken(a, wk) {
		t.Error("token is not stable for the same venue and week")
	}
	// Time of day must not matter — only the date.
	if h.reportToken(a, wk) != h.reportToken(a, wk.Add(17*time.Hour)) {
		t.Error("token changed with the time of day")
	}
	if h.reportToken(a, wk) == h.reportToken(b, wk) {
		t.Error("two venues share a token")
	}
	if h.reportToken(a, wk) == h.reportToken(a, wk.AddDate(0, 0, -7)) {
		t.Error("two weeks share a token")
	}
	// A different secret must invalidate old links.
	if h.reportToken(a, wk) == testHandler("other").reportToken(a, wk) {
		t.Error("token does not depend on the secret")
	}
	if !reportTokenRe.MatchString(h.reportToken(a, wk)) {
		t.Errorf("token %q is not in the accepted alphabet", h.reportToken(a, wk))
	}
}

// TestReportPDFServeRejectsBadTokens is the important one: this route has no session,
// so the path segment is attacker-controlled and gets joined to a directory.
func TestReportPDFServeRejectsBadTokens(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("REPORT_DIR", dir)
	// A real file the traversal attempts would be trying to reach.
	if err := os.WriteFile(filepath.Join(dir, "secret.pdf"), []byte("%PDF-1.4 secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	h := testHandler("s3cr3t")

	for _, tok := range []string{
		"../../../etc/passwd", "..%2f..%2fetc%2fpasswd", "a/b", "", "short",
		"with space", "semi;colon", "dot.dot",
	} {
		req := httptest.NewRequest(http.MethodGet, "/reports/x", nil)
		req.SetPathValue("token", tok)
		rec := httptest.NewRecorder()
		h.ReportPDFServe(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Errorf("token %q returned %d, want 404", tok, rec.Code)
		}
	}
}

// TestReportPDFServeReturnsTheFile: a valid token gets the PDF, with a content type a
// browser will render rather than sniff.
func TestReportPDFServeReturnsTheFile(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("REPORT_DIR", dir)
	h := testHandler("s3cr3t")
	tok := h.reportToken(uuid.New(), time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC))
	if err := os.WriteFile(filepath.Join(dir, tok+".pdf"), []byte("%PDF-1.4 hello"), 0o644); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodGet, "/reports/"+tok+".pdf", nil)
	req.SetPathValue("token", tok+".pdf") // the router passes the segment with its suffix
	rec := httptest.NewRecorder()
	h.ReportPDFServe(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("got %d, want 200", rec.Code)
	}
	if ct := rec.Header().Get("Content-Type"); ct != "application/pdf" {
		t.Errorf("content-type %q", ct)
	}
	if rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Error("missing nosniff on an unauthenticated file route")
	}
	// The URL is the credential, so a shared cache must not keep it.
	if cc := rec.Header().Get("Cache-Control"); cc == "" || cc[:7] != "private" {
		t.Errorf("cache-control %q must be private", cc)
	}
}

// TestReportPDFServeMissingIsNotFound: an unrendered week and a wrong token must be
// indistinguishable, or the 404 becomes an oracle for which tokens exist.
func TestReportPDFServeMissingIsNotFound(t *testing.T) {
	t.Setenv("REPORT_DIR", t.TempDir())
	h := testHandler("s3cr3t")
	tok := h.reportToken(uuid.New(), time.Now())
	req := httptest.NewRequest(http.MethodGet, "/reports/"+tok, nil)
	req.SetPathValue("token", tok)
	rec := httptest.NewRecorder()
	h.ReportPDFServe(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Errorf("got %d, want 404", rec.Code)
	}
}
