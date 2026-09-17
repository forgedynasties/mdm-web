package dashboard

import (
	"bytes"
	"fmt"
	"html/template"
	"strings"
	"testing"
	"time"

	"mdm/internal/db"

	"github.com/google/uuid"
)

// TestRestaurantReportRenders executes templates/restaurant_report.html against the
// same data shape RestaurantReport builds. Parsing alone does not catch a field that
// does not exist on the struct or a method called with the wrong arity — those only
// surface at execution, which on a real page means a logged 500 and a blank report.
//
// The func map mirrors the report's own helpers; the rest are stubbed, since this
// test is about the data contract, not about formatting.
func TestRestaurantReportRenders(t *testing.T) {
	funcs := template.FuncMap{
		"hrs1": func(minutes float64) string { return fmt.Sprintf("%.1f", minutes/60) },
		"band": func(pct int) string {
			switch {
			case pct < 70:
				return "risk"
			case pct < 85:
				return "warn"
			}
			return ""
		},
		"initials": func(serial string) string {
			if len(serial) <= 2 {
				return strings.ToUpper(serial)
			}
			return strings.ToUpper(serial[len(serial)-2:])
		},
		"padDrainBar": func(rate float64) int { return pctCapped(rate, 0.2) },
		"shortDate":   func(t time.Time) string { return t.Format("Jan 2") },
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFiles("../../templates/restaurant_report.html")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	// A venue with one healthy device and one that reports neither pad nor screen
	// state, so both the measured and the "not enough data" branches render.
	weeks := []db.DeviceWeek{
		{
			DeviceID: uuid.New(), Serial: "AT070AABU00333", Nickname: "Front counter",
			Days: 7, DeviceDays: 7, PoweredMinutes: 9000, FullWindowMinutes: 7 * 24 * 60,
			PluggedMinutes: 5000, PadMinutes: 600, PadDrainPct: 40, PadDrainMinutes: 400,
			ScreenOnMinutes: 3000, ScreenPoweredMinutes: 9000, ScreenDeviceDays: 7,
		},
		{
			DeviceID: uuid.New(), Serial: "AT070AABU00480",
			Days: 7, DeviceDays: 2, PoweredMinutes: 1200, FullWindowMinutes: 2 * 24 * 60,
			PluggedMinutes: 300,
		},
	}
	data := map[string]any{
		"Restaurant":        &db.Restaurant{ID: uuid.New(), Name: "Flights Vegas", Address: "3726 Planet Hollywood Way"},
		"Days":              7,
		"WindowHours":       168,
		"WindowFrom":        time.Now().AddDate(0, 0, -6),
		"WindowTo":          time.Now(),
		"Metrics":           db.SiteMetrics{Days: 7, DeviceCount: 2, PoweredMinutes: 10200, FullWindowMinutes: 9 * 24 * 60, PadMinutes: 600, PadDrainPct: 40, PadDrainMinutes: 400, ScreenOnMinutes: 3000, ScreenPoweredMinutes: 9000, ScreenDeviceDays: 7},
		"DeviceWeeks":       weeks,
		"Bars":              []reportBar{{Label: "Mon", Hours: 21.5, Pct: 89}, {Label: "Tue", Hours: 19.4, Pct: 80}},
		"AvgPoweredMinutes": 5100.0,
		"AvgPluggedMinutes": 2650.0,
		"AvgPluggedPct":     26,
		"AvgPadMinutes":     300.0,
		"AvgPadPct":         2,
		"AvgStandbyMinutes": 6000.0,
	}

	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "restaurant_report.html", data); err != nil {
		t.Fatalf("execute: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		"Flights Vegas",
		"AT070AABU00333",
		"Front counter",
		"Weekly fleet summary",
		"Individual device reports",
		"168 hours",
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered report is missing %q", want)
		}
	}
	// The second device has no pad time and no screen state: both cells must degrade
	// to a dash rather than claiming a measured zero.
	if strings.Count(out, `<span class="na">—</span>`) < 2 {
		t.Error("expected unmeasured pad drain and standby cells to render as —")
	}
}

// TestRestaurantReportEmpty covers a venue whose devices have no rolled-up day yet:
// the page must still render, with the explanation instead of an empty table.
func TestRestaurantReportEmpty(t *testing.T) {
	funcs := template.FuncMap{
		"hrs1":        func(float64) string { return "0.0" },
		"band":        func(int) string { return "" },
		"initials":    func(string) string { return "XX" },
		"padDrainBar": func(float64) int { return 0 },
		"shortDate":   func(t time.Time) string { return t.Format("Jan 2") },
	}
	tmpl, err := template.New("").Funcs(funcs).ParseFiles("../../templates/restaurant_report.html")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}
	data := map[string]any{
		"Restaurant":  &db.Restaurant{ID: uuid.New(), Name: "New Site"},
		"Days":        7,
		"WindowHours": 168,
		"WindowFrom":  time.Now().AddDate(0, 0, -6),
		"WindowTo":    time.Now(),
		"DeviceWeeks": []db.DeviceWeek{},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "restaurant_report.html", data); err != nil {
		t.Fatalf("execute: %v", err)
	}
	if !strings.Contains(buf.String(), "No device has a rolled-up day") {
		t.Error("empty venue should explain why the table is empty")
	}
}
