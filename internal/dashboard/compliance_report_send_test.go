package dashboard

import (
	"strings"
	"testing"
	"time"
)

func TestComplianceReportDue(t *testing.T) {
	tokyo, _ := time.LoadLocation("Asia/Tokyo")
	cases := []struct {
		now   time.Time
		loc   *time.Location
		month string
		due   bool
	}{
		{time.Date(2026, 11, 1, 8, 59, 0, 0, time.UTC), time.UTC, "", false},
		{time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC), time.UTC, "2026-10", true},
		{time.Date(2026, 11, 3, 23, 0, 0, 0, time.UTC), time.UTC, "2026-10", true},
		{time.Date(2026, 11, 4, 0, 0, 0, 0, time.UTC), time.UTC, "", false},
		// 1 Nov 00:30 UTC is 09:30 in Tokyo: due there, not yet in UTC.
		{time.Date(2026, 11, 1, 0, 30, 0, 0, time.UTC), tokyo, "2026-10", true},
		{time.Date(2026, 1, 1, 10, 0, 0, 0, time.UTC), time.UTC, "2025-12", true},
	}
	for _, c := range cases {
		m, due := complianceReportDue(c.now, c.loc)
		if m != c.month || due != c.due {
			t.Errorf("%v in %v: got %q %v, want %q %v", c.now, c.loc, m, due, c.month, c.due)
		}
	}
}

func TestComplianceRecipients(t *testing.T) {
	got := complianceRecipients(" owner@venue.com ", []string{"Owner@Venue.com", "", "boss@venue.com", "boss@venue.com"})
	if strings.Join(got, ",") != "owner@venue.com,boss@venue.com" {
		t.Fatalf("%v", got)
	}
	if got := complianceRecipients("", nil); len(got) != 0 {
		t.Fatalf("%v", got)
	}
}

func TestComplianceReportEmailHTML(t *testing.T) {
	sec := testSecurity()
	html := complianceReportEmailHTML("Bob's <Diner>", time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC), *sec, "https://mdm.example/reports/x.pdf")
	for _, want := range []string{"Bob&#39;s &lt;Diner&gt;", "October 2026", "31 of 31 days", "not measured", "29 of 31 days", "https://mdm.example/reports/x.pdf"} {
		if !strings.Contains(html, want) {
			t.Errorf("email lacks %q", want)
		}
	}
}
