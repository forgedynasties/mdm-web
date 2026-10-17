package dashboard

import (
	"bytes"
	"testing"
	"time"
)

func testSecurity() *venueSecurity {
	lines := []compLine{
		{Statement: compStatements[0], Measured: 14, Clean: 14, DaysMeasured: 31, DaysClean: 31},
		{Statement: compStatements[1], Measured: 0},
		{Statement: compStatements[2], Measured: 14, Clean: 13, DaysMeasured: 31, DaysClean: 29},
	}
	return &venueSecurity{Devices: 14, Unmeasured: 4, Lines: lines}
}

// The weekly PDF gains a "Device security" page only when the report carries one.
func TestRenderReportPDFSecurityPage(t *testing.T) {
	win := reportWindow{From: time.Date(2026, 10, 5, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 10, 11, 0, 0, 0, 0, time.UTC), Days: 7}
	without, err := renderReportPDF("Nugget Downtown", venueReport{Win: win}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	with, err := renderReportPDF("Nugget Downtown", venueReport{Win: win, Security: testSecurity()}, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	pages := func(b []byte) int { return bytes.Count(b, []byte("/Type /Page\n")) }
	if pages(with) != pages(without)+1 {
		t.Fatalf("security page: %d pages with, %d without", pages(with), pages(without))
	}
	if secHash(nil) == secHash(testSecurity()) {
		t.Fatal("the cache key must change with the page")
	}
}
