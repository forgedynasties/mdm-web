package dashboard

import (
	"bytes"
	"fmt"
	"testing"
	"time"

	"mdm/internal/db"

	"github.com/google/uuid"
)

// TestRenderReportPDF draws a venue big enough to spill onto a second page, with a
// name outside plain ASCII and a device with nothing measured, and checks a PDF comes
// out. The layout itself is checked by eye; this guards the render not failing.
func TestRenderReportPDF(t *testing.T) {
	m := db.SiteMetrics{Days: 7, DeviceCount: 40, PoweredMinutes: 400000, FullWindowMinutes: 40 * 7 * 1440,
		PadMinutes: 900, PadDrainPct: 30, PadDrainMinutes: 300}
	v := venueReport{
		Win:               reportWindow{From: time.Date(2026, 9, 21, 0, 0, 0, 0, time.UTC), To: time.Date(2026, 9, 27, 0, 0, 0, 0, time.UTC), Days: 7},
		Metrics:           &m,
		Bars:              []reportBar{{Label: "Mon", Hours: 6.9, Pct: 28}, {Label: "Wed", Hours: 24, Pct: 100}},
		AvgPoweredMinutes: 7000, AvgPluggedMinutes: 5000, AvgPluggedPct: 50, AvgPadMinutes: 12,
	}
	for i := 0; i < 40; i++ {
		v.DeviceWeeks = append(v.DeviceWeeks, db.DeviceWeek{
			DeviceID: uuid.New(), Serial: fmt.Sprintf("AT070AABU%05d", i), Nickname: "Café table — patio, far corner by the window",
			Days: 7, DeviceDays: 7, PoweredMinutes: float64(1000 * i), FullWindowMinutes: 7 * 1440,
			PluggedMinutes: 3000, PadMinutes: float64(i), PadDrainPct: 5, PadDrainMinutes: float64(i * 2),
		})
	}
	b, err := renderReportPDF("Flights — Café 東京", v, time.Now())
	if err != nil {
		t.Fatalf("render: %v", err)
	}
	if !bytes.HasPrefix(b, []byte("%PDF-")) {
		t.Fatal("output is not a PDF")
	}
	if n := bytes.Count(b, []byte("/Type /Page\n")); n < 2 {
		t.Errorf("40 devices rendered on %d page(s), want at least 2", n)
	}

	// An empty venue and the week in progress must render too.
	empty := venueReport{Win: reportWindow{From: v.Win.From, To: v.Win.From, Days: 1, Current: true}}
	if _, err := renderReportPDF("New Site", empty, time.Now()); err != nil {
		t.Fatalf("empty render: %v", err)
	}
}
