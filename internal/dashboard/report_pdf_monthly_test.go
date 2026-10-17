package dashboard

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestComplianceMonth(t *testing.T) {
	ny, _ := time.LoadLocation("America/New_York")
	now := time.Date(2026, 11, 1, 3, 0, 0, 0, time.UTC) // still 31 Oct in New York
	if _, _, ok := complianceMonth("2026-11", ny, now); ok {
		t.Fatal("November has not begun in New York")
	}
	from, to, ok := complianceMonth("2026-10", ny, now)
	if !ok || from.Format(time.RFC3339) != "2026-10-01T00:00:00-04:00" || to.Format(time.RFC3339) != "2026-11-01T00:00:00-04:00" {
		t.Fatalf("October: %v %v %v", from, to, ok)
	}
	for _, bad := range []string{"2025-09", "2026-13", "October", ""} {
		if _, _, ok := complianceMonth(bad, ny, now); ok {
			t.Errorf("%q accepted", bad)
		}
	}
	if got := lastMonth(now, ny); got != "2026-09" {
		t.Fatalf("last month in New York: %s", got)
	}
	if got := lastMonth(now, time.UTC); got != "2026-10" {
		t.Fatalf("last month in UTC: %s", got)
	}
}

func TestComplianceMonthToken(t *testing.T) {
	h := &Handler{reportSecret: "secret"}
	id := uuid.New()
	a, b := h.complianceMonthToken(id, "2026-10"), h.complianceMonthToken(id, "2026-09")
	if a == b || len(a) != 32 || !reportTokenRe.MatchString(a) {
		t.Fatalf("tokens %q %q", a, b)
	}
	if a == h.reportToken(id, time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC)) {
		t.Fatal("a weekly token must not open a monthly report")
	}
}

func TestRenderComplianceMonthPDF(t *testing.T) {
	sec := testSecurity()
	sec.From = time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	sec.Loc = time.UTC
	for i := 0; i < 5; i++ {
		sec.Weeks = append(sec.Weeks, compWeek{From: sec.From.AddDate(0, 0, 7*i), Days: 7, Clean: 7 - i%2, Pct: 100 - 14*(i%2), AllDaysClean: i%2 == 0})
	}
	end := sec.From.AddDate(0, 0, 10)
	for i := 0; i < 45; i++ { // spills onto a second page and past the listing cap
		sec.Exceptions = append(sec.Exceptions, compException{Name: fmt.Sprintf("Café table %d", i), Statement: compStatements[i%3],
			Start: sec.From.AddDate(0, 0, i%28), End: &end})
	}
	b, err := renderComplianceMonthPDF("Flights — Café 東京", *sec, false, time.Now())
	if err != nil || !bytes.HasPrefix(b, []byte("%PDF-")) {
		t.Fatalf("render: %v", err)
	}
	empty := venueSecurity{From: sec.From, Lines: []compLine{{Statement: compStatements[0]}}}
	if _, err := renderComplianceMonthPDF("New Site", empty, true, time.Now()); err != nil {
		t.Fatalf("empty render: %v", err)
	}
}
