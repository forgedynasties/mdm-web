package dashboard

import (
	"strings"
	"testing"

	"mdm/internal/db"
)

// TestReportEmailHasNoDeviceTable pins the shape of the mail: headline figures and a
// way through to the full thing, not a table of every device. An email is read on a
// phone, and the per-device breakdown both made it long and wrapped badly in narrow
// reading panes — the page renders it properly and can save it as a PDF.
func TestReportEmailHasNoDeviceTable(t *testing.T) {
	weeks := []db.DeviceWeek{
		{Serial: "AT070AABU00318", PoweredMinutes: 9000, PadMinutes: 120, PluggedMinutes: 8000, DeviceDays: 7},
		{Serial: "AT070AABU00527", PoweredMinutes: 8000, PadMinutes: 60, PluggedMinutes: 7000, DeviceDays: 7},
		{Serial: "AT070AABUF0039", PoweredMinutes: 7000, PadMinutes: 30, PluggedMinutes: 6000, DeviceDays: 7},
	}
	url := "https://mdm.dev.aioapp.com/restaurants/abc-123/report?days=7"
	out := reportEmailHTML("Flights Vegas", 7, db.SiteMetrics{}, weeks, url)

	// No device appears by serial — that is the whole change.
	for _, w := range weeks {
		if strings.Contains(out, w.Serial) {
			t.Errorf("the mail still lists device %s", w.Serial)
		}
	}
	// But the reader is told how many there are, and can get to the detail.
	if !strings.Contains(out, "3 devices reporting") {
		t.Error("the mail no longer says how many devices reported")
	}
	if !strings.Contains(out, url) {
		t.Error("the mail does not link to the full report")
	}
	if !strings.Contains(out, "View the full report") {
		t.Error("no call to action to reach the full report")
	}
	// The venue and window still lead the mail.
	if !strings.Contains(out, "Flights Vegas") {
		t.Error("venue name missing")
	}
}

// TestReportEmailEscapesUntrustedText: the venue name and the URL are interpolated into
// HTML, and a venue is named by a user.
func TestReportEmailEscapesUntrustedText(t *testing.T) {
	out := reportEmailHTML(`Bob"s <script>alert(1)</script>`, 7, db.SiteMetrics{}, nil,
		"https://x/report?a=1&b=2")
	if strings.Contains(out, "<script>") {
		t.Error("venue name was not escaped into the mail")
	}
	if !strings.Contains(out, "&amp;b=2") {
		t.Error("the report URL was not escaped; an & in a query string breaks the href")
	}
}
