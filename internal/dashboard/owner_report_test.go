package dashboard

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"strings"
	"testing"
	"time"

	"mdm/internal/db"
)

// TestOwnerHomeReportCardRenders executes the owner home's report card against the same
// data shape OwnerHome builds. Parsing alone will not catch a field that does not exist
// on db.SiteMetrics or a method called with the wrong arity — those surface only at
// execution, which on the real page means a logged 500 and a blank home for the one
// role that cannot debug it.
//
// The card is extracted from the page rather than rendering the whole thing, because
// the surrounding page needs the site chrome and a dozen unrelated helpers; the data
// contract being checked here is the report card's.
func TestOwnerHomeReportCardRenders(t *testing.T) {
	funcs := template.FuncMap{
		"hrs1": func(minutes float64) string { return fmt.Sprintf("%.1f", minutes/60) },
		"perDev": func(total float64, n int) float64 {
			if n <= 0 {
				return 0
			}
			return total / float64(n)
		},
	}
	// The card's own markup, lifted verbatim from the page so the two cannot drift
	// apart silently: if the page stops using these fields this test still passes, but
	// if it uses one that does not exist the page fails the same way this does.
	src, err := readTemplateSection("../../templates/owner_home.html",
		"{{if .ReportMetrics}}", "{{end}}")
	if err != nil {
		t.Fatalf("locate card: %v", err)
	}
	card, err := template.New("card").Funcs(funcs).Parse(src)
	if err != nil {
		t.Fatalf("parse card: %v", err)
	}

	from := time.Date(2026, 9, 7, 0, 0, 0, 0, time.UTC)
	to := time.Date(2026, 9, 13, 0, 0, 0, 0, time.UTC)
	cases := []struct {
		name string
		m    db.SiteMetrics
	}{
		{"full data", db.SiteMetrics{
			Days: 7, DeviceCount: 4, DeviceDays: 28,
			PoweredMinutes: 35000, PoweredOpenMinutes: 12000, HasOpenHours: true,
			PadMinutes: 900, ScreenOnMinutes: 8000, ScreenPoweredMinutes: 35000, ScreenDeviceDays: 28,
		}},
		// A venue with no service window and firmware that does not report screen
		// state: both optional rows must simply not render.
		{"no open hours, no standby", db.SiteMetrics{
			Days: 7, DeviceCount: 1, DeviceDays: 7, PoweredMinutes: 5000, PadMinutes: 0,
		}},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var buf bytes.Buffer
			err := card.Execute(&buf, map[string]any{
				"ReportMetrics": &c.m,
				"ReportFrom":    from,
				"ReportTo":      to,
				"ReportURL":     "https://mdm.dev.aioapp.com/reports/abc.pdf",
			})
			if err != nil {
				t.Fatalf("execute: %v", err)
			}
			out := buf.String()
			if strings.Contains(out, "Preview") {
				t.Error("the real card still carries the Preview pill")
			}
			for _, want := range []string{"Mon 7 Sep", "Sun 13 Sep", "Open the full report"} {
				if !strings.Contains(out, want) {
					t.Errorf("missing %q", want)
				}
			}
			if c.m.HasOpenHours && !strings.Contains(out, "During opening hours") {
				t.Error("a venue with service hours should show the open-hours figure")
			}
			if !c.m.HasOpenHours && strings.Contains(out, "During opening hours") {
				t.Error("a venue with no service hours must not claim an open-hours figure")
			}
			if !c.m.HasStandby() && strings.Contains(out, "standby") {
				t.Error("standby shown for firmware that does not report screen state")
			}
		})
	}
}

// readTemplateSection returns the substring of a file from the first line containing
// start through the matching trailing marker, so a test can exercise one card of a
// large page without copying its markup into the test.
func readTemplateSection(path, start, end string) (string, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	s := string(b)
	i := strings.Index(s, start)
	if i < 0 {
		return "", fmt.Errorf("start marker %q not found", start)
	}
	// Walk forward balancing {{if}}/{{end}} so we take the whole card.
	depth, j := 0, i
	for j < len(s) {
		switch {
		case strings.HasPrefix(s[j:], "{{if"), strings.HasPrefix(s[j:], "{{range"), strings.HasPrefix(s[j:], "{{with"):
			depth++
			j += 4
		case strings.HasPrefix(s[j:], end):
			depth--
			j += len(end)
			if depth == 0 {
				return s[i:j], nil
			}
		default:
			j++
		}
	}
	return "", fmt.Errorf("unbalanced template section")
}
