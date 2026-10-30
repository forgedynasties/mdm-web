package dashboard

import (
	"bytes"
	"html/template"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// vcSample: Harbour Grill at risk and open, Mom's Biryani on watch with the default staff
// PIN, Olive & Ash healthy, and an empty venue. All on 11:00–22:00 New York hours.
func vcSample() ([]venueRow, map[string]vcExtra) {
	id := func() string { return uuid.NewString() }
	hg, mb, oa, em := id(), id(), id(), id()
	rows := []venueRow{
		{ID: oa, Name: "Olive & Ash", Href: "/restaurants/" + oa, Text: "Olive & Ash", Devices: 4, Online: 4, Class: "ok", HasH: true, H: db.GroupHealth{Score: 96}, Pips: []string{"on", "on", "on", "on"}},
		{ID: em, Name: "Pier 9", Href: "/restaurants/" + em, Text: "Pier 9", Sub: "no address", Class: "empty"},
		{ID: mb, Name: "Mom's Biryani", Href: "/restaurants/" + mb, Text: "Mom's Biryani Sunnyvale", Devices: 2, Offline: 2, Class: "warn", NeedsLook: true, HasH: true, H: db.GroupHealth{Score: 57}, Pips: []string{"off", "off"}},
		{ID: hg, Name: "Harbour Grill", Href: "/restaurants/" + hg, Text: "Harbour Grill", Devices: 26, Online: 18, Offline: 8, Class: "danger", NeedsLook: true, HasH: true, H: db.GroupHealth{Score: 42, OpenCritical: 1}},
	}
	w := db.ServiceWindow{OpenMin: 11 * 60, CloseMin: 22 * 60}
	ex := map[string]vcExtra{
		hg: {Window: &w, TZ: "America/New_York", HotNow: 1, Classes: map[string]int{"t7": 22, "kds": 4}},
		mb: {Window: &w, TZ: "America/New_York", DefaultPIN: true, Classes: map[string]int{"t7": 2}},
		oa: {Window: &w, TZ: "America/New_York", Classes: map[string]int{"t7": 3, "dongle": 1}},
		em: {Window: &w, TZ: "America/New_York", DefaultPIN: true, Classes: map[string]int{}},
	}
	return rows, ex
}

func TestBuildVenuesV1(t *testing.T) {
	rows, ex := vcSample()
	now := time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC) // 14:00 in New York: open
	p := buildVenuesV1(rows, ex, now)

	var order []string
	for _, c := range p.Cards {
		order = append(order, c.Name)
	}
	if strings.Join(order, "|") != "Harbour Grill|Mom's Biryani|Olive & Ash|Pier 9" {
		t.Errorf("worst first: %v", order)
	}
	if p.Look != 2 || p.Healthy != 1 || p.Empty != 1 || p.Open != 4 {
		t.Errorf("counts: look %d healthy %d empty %d open %d", p.Look, p.Healthy, p.Empty, p.Open)
	}
	if !p.Cards[1].DefaultPIN || p.Cards[3].DefaultPIN {
		t.Error("default staff PIN counts only for a venue with devices")
	}
	if p.Cards[0].Classes != "kds t7" {
		t.Errorf("classes = %q", p.Cards[0].Classes)
	}
	if len(p.Types) != 3 || p.Types[0].Key != "t7" || p.Types[0].Venues != 3 {
		t.Errorf("types = %+v", p.Types)
	}

	r := p.Rail
	if !r.HasAvg || r.Avg != 65 || r.Tone != "warn" || r.OpenNow != 4 || r.WithHours != 4 {
		t.Errorf("rail = %+v", r)
	}
	if len(r.Hours) != 12 || r.Hours[5].Label != "now" || !r.Hours[5].Cur || r.Hours[5].N != 4 {
		t.Errorf("hours = %+v", r.Hours)
	}
	// 14:00 + 6 h = 20:00, still open; 14:00 − 5 h = 09:00, closed.
	if r.Hours[0].N != 0 || r.Hours[11].N != 4 {
		t.Errorf("hour ends = %d, %d", r.Hours[0].N, r.Hours[11].N)
	}
	want := []string{"Harbour Grill", "Mom's Biryani", "Mom's Biryani", "Pier 9"}
	if len(r.Looks) != len(want) {
		t.Fatalf("looks = %+v", r.Looks)
	}
	for i, w := range want {
		if r.Looks[i].Name != w {
			t.Errorf("look %d = %q, want %q", i, r.Looks[i].Name, w)
		}
	}
	if r.Looks[0].Tone != "danger" || r.Looks[0].Sub != "8 offline · 1 critical · a unit running hot · open now" {
		t.Errorf("first look = %+v", r.Looks[0])
	}
	if !strings.HasSuffix(r.Looks[2].Href, "?open=lock") || !strings.Contains(r.Looks[2].Sub, db.DefaultStaffPIN) {
		t.Errorf("PIN look = %+v", r.Looks[2])
	}

	// Closed at 04:00 New York.
	night := buildVenuesV1(rows, ex, time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC))
	if night.Open != 0 || night.Cards[0].Open {
		t.Errorf("night: open %d", night.Open)
	}
}

func TestVcHotNow(t *testing.T) {
	if vcHotNow(false, 57.8, true) {
		t.Error("an offline device's last temperature is stale, not hot now")
	}
	if !vcHotNow(true, 47, true) || vcHotNow(true, 40, true) || vcHotNow(true, 50, false) {
		t.Error("hot now = online, a reading, at or over 45")
	}
}

// TestVenuesListRenders executes restaurants.html in both views.
func TestVenuesListRenders(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	funcs["dict"] = func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	funcs["add"] = func(a, b int) int { return a + b }
	funcs["canAdminOrOperator"] = func(string) bool { return true }
	funcs["venueIcon"] = func(id, name, size string) template.HTML { return `<span class="vicon"></span>` }
	parse := func() *template.Template {
		tm := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}`))
		return template.Must(tm.ParseFiles("../../templates/restaurants.html"))
	}
	rows, ex := vcSample()
	ex[rows[3].ID] = func(e vcExtra) vcExtra { e.Place, e.State, e.TZ2 = "Sunnyvale, CA", "CA", "New York"; return e }(ex[rows[3].ID])
	sum := venueSummary{Devices: 32, Offline: 10, Critical: 1, NeedLook: 2, WithOffline: 2, Empty: 1}
	data := map[string]any{"Role": "admin", "Venues": rows, "Summary": sum, "View": "cards",
		"V1": buildVenuesV1(rows, ex, time.Date(2026, 10, 21, 18, 0, 0, 0, time.UTC))}
	var buf bytes.Buffer
	if err := parse().ExecuteTemplate(&buf, "restaurants.html", data); err != nil {
		t.Fatalf("cards view: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`id="page-island"`, `id="vc1-grid"`, `data-vc1-pill="health"`, `data-val="look"`, `Needs a look<span class="rt">2</span>`,
		`data-val="t7" data-label="Tableside AI"`, `class="ov2-v s-danger"`, `At risk · 42`, `data-health="look"`, `data-open="1"`,
		`data-types="kds t7"`, `1 running hot`, `default staff PIN`, `<span class="vc1-open on">Open now</span>`,
		`average health`, `4 of 4 open now`, `2 need a look`, `Needs a look <span>4</span>`, `?open=lock`,
		`href="/restaurants?view=table"`, `/static/js/oc-menus.js`, `/static/js/venues_v1.js`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("cards view is missing %q", want)
		}
	}
	if strings.Contains(out, `data-cl `) {
		t.Error("the table should only render in the table view")
	}

	data["View"] = "table"
	buf.Reset()
	if err := parse().ExecuteTemplate(&buf, "restaurants.html", data); err != nil {
		t.Fatalf("table view: %v", err)
	}
	out = buf.String()
	for _, want := range []string{
		`data-cl data-one="venue"`, `data-clf="look"`, `data-clf="st-CA"`, `>California<`, `data-clp="t7"`, `data-clc="pin"`,
		`data-cls="health" data-dir="asc"`, `class="ft cl-ft"`, `data-flags="look offline open hot st-CA"`, `Sunnyvale, CA · New York`,
		`<span class="cl-now on">Open</span>`, `class="cc-tag bad">1 running hot`, `href="/restaurants?view=cards"`, `/static/js/coll_list.js`,
		`/static/css/coll-list.css`, `<use href="#dk-kds">`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("table view is missing %q", want)
		}
	}
	if strings.Contains(out, `id="vc1-grid"`) {
		t.Error("the card grid should only render in the cards view")
	}
}

func TestVenuePlace(t *testing.T) {
	for _, c := range [][3]string{
		{"1321 Blossom Hill Rd, San Jose, CA 95118", "San Jose, CA", "CA"},
		{"609 1st St Brentwood, CA 94513", "Brentwood, CA", "CA"},
		{"2680 W Business 77, San Benito, TX 78586, USA", "San Benito, TX", "TX"},
		{"3726 Planet Hollywood Way, The Las Vegas Strip, NV", "The Las Vegas Strip, NV", "NV"},
		{"Main Street", "", ""}, {"", "", ""},
	} {
		if p, st := venuePlace(c[0]); p != c[1] || st != c[2] {
			t.Errorf("venuePlace(%q) = %q, %q", c[0], p, st)
		}
	}
	if tzCity("America/Los_Angeles") != "Los Angeles" || tzCity("") != "" {
		t.Error("tzCity")
	}
}
