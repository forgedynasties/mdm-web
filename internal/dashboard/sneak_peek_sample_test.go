package dashboard

import (
	"bytes"
	"html/template"
	"net/http/httptest"
	"os"
	"regexp"
	"sort"
	"strings"
	"testing"
	"time"
)

// TestSneakPeekSampleAddsUp checks the preview's numbers agree with each other: every
// card of the Overview is built from one sample fleet, and a count that disagrees
// with another on the same page is what makes sample data look fake.
func TestSneakPeekSampleAddsUp(t *testing.T) {
	s := newSneakPeekSample(time.Now())
	sum := s.Summary()
	groups := s.Groups()
	// overviewViewModel sorts groups by score in place before the venues are built.
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Score < groups[j].Score })
	ven := s.Venues(groups)

	placed, online := 0, 0
	for _, v := range ven.Rows {
		placed += v.Devices
		online += v.Online
		if v.H.Name != v.Name {
			t.Errorf("row %s carries %s's health", v.Name, v.H.Name)
		}
		t.Logf("%-20s %3d devices %3d online  score %3d  %s", v.Name, v.Devices, v.Online, v.H.Score, v.Class)
	}
	if ven.More != 0 || ven.Total != len(spVenues) {
		t.Errorf("venues: %d rows + %d more, want all %d", len(ven.Rows), ven.More, len(spVenues))
	}
	if placed+ven.Unplaced != sum.Total {
		t.Errorf("venue devices %d + unplaced %d != fleet %d", placed, ven.Unplaced, sum.Total)
	}
	if ven.Unplaced != s.Inbox() {
		t.Errorf("unplaced %d != inbox %d", ven.Unplaced, s.Inbox())
	}
	if online+ven.Unplaced != sum.RecentlyActive { // every unplaced sample device is online
		t.Errorf("venue online %d + unplaced %d != fleet online %d", online, ven.Unplaced, sum.RecentlyActive)
	}
	if ven.NeedLook == 0 {
		t.Error("no venue needs a look: the widget would show nothing to do")
	}

	cc, co := s.ClassCounts()
	n, on := 0, 0
	for i := range cc {
		n += cc[i].N
		on += co[i].Online
	}
	if n != sum.Total || on != sum.RecentlyActive {
		t.Errorf("devices by type %d/%d online, fleet %d/%d", on, n, sum.RecentlyActive, sum.Total)
	}

	c := s.Compliance()
	if c.InScope != sum.Total || c.Compliant+c.Failing != c.InScope || len(c.Top) != 3 {
		t.Errorf("compliance: %+v", c)
	}
	t.Logf("compliance %d%%: %d compliant, %d failing, top %v, %d more", c.Pct, c.Compliant, c.Failing, c.Top, c.MoreChecks)

	att, total, crit := buildAttention(s.alerts, s.Inbox())
	if total < len(att) || crit == 0 {
		t.Errorf("attention: %d rows of %d, %d critical", len(att), total, crit)
	}
	for _, r := range att {
		t.Logf("attention %-8s %-28s %s %d %s", r.Severity, r.Issue, r.Serial, r.Devices, r.Restaurant)
	}
	crits, warns := 0, 0
	for _, g := range groups {
		crits += g.OpenCritical
		warns += g.OpenWarning
	}
	if crits+warns != s.OpenAlerts() {
		t.Errorf("venue alerts %d+%d != open alerts %d", crits, warns, s.OpenAlerts())
	}
	for _, r := range hygieneRows(s.Hygiene(sum)) {
		t.Logf("clean up: %d %s — %s", r.Count, r.What, r.Note)
	}
	t.Logf("fleet %d, online %d, low %d, builds %d, locked %d, hot %d, alerts %d", sum.Total, sum.RecentlyActive, sum.LowBattery, sum.UniqueBuilds, sum.KioskCount, s.Hot(), s.OpenAlerts())
}

// TestSneakPeekOverviewRenders renders the preview's data through today's Overview
// template, the one "/" renders, with a Handler that has no database at all: a read
// of the real fleet would panic here instead of leaking into the preview.
func TestSneakPeekOverviewRenders(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatalf("read handlers.go: %v", err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	funcs["add"] = func(a, b int) int { return a + b }
	funcs["sub"] = func(a, b int) int { return a - b }
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/overview_new.html", "../../templates/overview.html"))

	h := &Handler{}
	r := sneakPeekAuth(httptest.NewRequest("GET", "/sneak-peek", nil))
	data := h.sneakPeekOverview(r, newSneakPeekSample(time.Now()), 180)
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "overview_new.html", data); err != nil {
		t.Fatalf("overview_new.html failed to render the sample: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		// The C layout (21 Oct): filters, to-do banner, delta cards, pill chart, rollouts;
		// venue cards, clean-up table and the rail from O2.
		`class="ov2-ban`, `<b>Bluefin Sushi Bar</b>`, `2 need a look`, `3 devices waiting for a site`,
		`<b>Compliance</b>`, `>84%</text>`,
		`href="/sneak-peek?type=kds"`, `to look at today`, `class="oc-todo"`,
		`<span class="lb">TSAI</span>`, `<span class="lb">Menu</span>`, `<span class="lb">KDS</span>`,
		`Firmware MDM 1.13.10`, `>Stability</a>`,
		`Needs attention`, `class="ov2-rail"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("sneak peek Overview is missing %q", want)
		}
	}
	for _, gone := range []string{`Kiosk 22 / T7`, `id="ovd-intro"`} {
		if strings.Contains(out, gone) {
			t.Errorf("sneak peek Overview still shows %q", gone)
		}
	}
}
