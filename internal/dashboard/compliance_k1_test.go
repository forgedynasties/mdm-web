package dashboard

import (
	"bytes"
	"html/template"
	"net/url"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// TestCompTrendReplaysHistory: a device that failed a required check until three days
// ago and passes since; one that only appears two days ago; one whose device checks are
// all unknown (a v2.0 image).
func TestCompTrendReplaysHistory(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	a, b, c := uuid.New(), uuid.New(), uuid.New()
	ev := func(id uuid.UUID, check, to, lvl string, daysAgo int) db.ComplianceEvent {
		return db.ComplianceEvent{DeviceID: id, CheckID: check, To: to, Level: lvl, At: now.AddDate(0, 0, -daysAgo)}
	}
	events := []db.ComplianceEvent{
		ev(a, "checked_in", db.ComplianceFail, db.ComplianceRequired, 20), // before the window: its state at the start
		ev(a, "checked_in", db.CompliancePass, db.ComplianceRequired, 3),
		ev(a, "wireless_adb_off", db.CompliancePass, db.ComplianceRequired, 20),
		ev(b, "checked_in", db.CompliancePass, db.ComplianceRequired, 2),
		ev(c, "checked_in", db.CompliancePass, db.ComplianceRequired, 30),
		ev(c, "wireless_adb_off", db.ComplianceUnknown, db.ComplianceRequired, 30),
		ev(c, "encrypted", db.ComplianceUnknown, db.ComplianceRequired, 30),
	}
	dev := map[string]bool{"wireless_adb_off": true, "encrypted": true}
	tr := compTrend([]uuid.UUID{a, b, c}, events, dev, now, 14)
	if len(tr) != 14 {
		t.Fatalf("days = %d", len(tr))
	}
	early, today := tr[0], tr[13]
	if early.InScope != 2 || early.Compliant != 1 || early.Partly != 1 {
		t.Errorf("14 days ago = %+v, want a failing, c compliant and partly, b not yet scored", early)
	}
	if today.InScope != 3 || today.Compliant != 3 || today.Pct() != 100 {
		t.Errorf("today = %+v", today)
	}
	if tr[12].Day.Format("2006-01-02") != "2026-10-20" {
		t.Errorf("days run oldest first to today: %v", tr[12].Day)
	}
}

func k1SampleChecks() ([]compCheckView, compRules) {
	get := func(id string) compCheck { c, _ := complianceCheck(id); return c }
	checks := []compCheckView{
		{compCheck: get("firmware_approved"), Level: db.ComplianceRequired, Counts: compCounts{Pass: 130, Fail: 22, Total: 152},
			Failing: append(
				[]compDeviceFail{{Serial: "AT070AABU00455", Venue: "Mom's Biryani", Detail: "v2.0.96l; prod is v2.1.034"}, {Serial: "AT070AABU00767", Venue: "Mom's Biryani", Detail: "v2.0.96l; prod is v2.1.034"}},
				compDeviceFail{Serial: "AT070AABU00044", Venue: "Harbour Grill", Detail: "v2.1.030; prod is v2.1.034"})},
		{compCheck: get("checked_in"), Level: db.ComplianceRequired, Counts: compCounts{Pass: 143, Fail: 9, Total: 152},
			Failing: []compDeviceFail{{Serial: "A1", Venue: "Mom's Biryani"}, {Serial: "A2", Venue: "Mom's Biryani"}, {Serial: "A3", Venue: "Harbour Grill"}}},
		{compCheck: get("wireless_adb_off"), Level: db.ComplianceRequired, Counts: compCounts{Pass: 62, Fail: 2, Unknown: 84, Total: 148},
			Failing: []compDeviceFail{{Serial: "AT070AABU00172", Detail: "adb is listening on the network"}, {Serial: "AT070AABU00790"}}},
		{compCheck: get("security_patch_recent"), Level: db.ComplianceRecommended, Counts: compCounts{Pass: 37, Fail: 31, Unknown: 84, Total: 152}},
		{compCheck: get("encrypted"), Level: db.ComplianceRequired, Counts: compCounts{Pass: 68, Unknown: 84, Total: 152}},
		{compCheck: get("locked"), Level: db.ComplianceRequired, Counts: compCounts{Pass: 149, Fail: 3, Total: 152}, Failing: []compDeviceFail{{Serial: "K1", Venue: "Kumo Ramen"}}},
	}
	return checks, compRules{}
}

func TestK1BannerOrderAndWho(t *testing.T) {
	checks, rules := k1SampleChecks()
	sc := parseK1Scope(url.Values{}, "checks", nil, nil)
	b := buildK1Banner(checks, rules, map[string]int{"wireless_adb_off": 4}, sc, 78, 27, 152, 84)
	if b.Tone != "warn" || b.Headline != "4 to fix first" || len(b.Todos) != 3 || b.More != 1 {
		t.Fatalf("banner = %+v", b)
	}
	fw := b.Todos[0]
	if fw.Title != "22 devices not on the approved firmware" || fw.Who != "a rollout" || fw.Href != "/releases" ||
		!strings.Contains(fw.Sub, "2 on another firmware line need a rollout you watch") || !strings.Contains(fw.Sub, "1 behind on their line") {
		t.Errorf("firmware row = %+v", fw)
	}
	if ci := b.Todos[1]; ci.Title != "9 devices at a venue silent for over 24 h" || ci.Sub != "2 at Mom's Biryani · 1 at Harbour Grill" {
		t.Errorf("checked-in row = %+v", ci)
	}
	if lk := b.Todos[2]; lk.Href != "/lock" {
		t.Errorf("third row is the lock rule (3 failing > adb's 2): %+v", lk)
	}
	// Wireless adb: auto-fix is on by default, so it says so.
	all := buildK1Banner(checks[2:3], rules, map[string]int{"wireless_adb_off": 4}, sc, 78, 2, 152, 84)
	if a := all.Todos[0]; a.Who != "auto-fix" || !strings.Contains(a.Sub, "fixed 4 this week") || a.Action != "Open the 2" ||
		a.Href != "/compliance?check=wireless_adb_off&show=all&tab=devices" {
		t.Errorf("adb row = %+v", a)
	}
	calm := buildK1Banner(checks[4:5], rules, nil, sc, 100, 0, 152, 84)
	if calm.Tone != "calm" || calm.Headline != "Every device meets every required check" ||
		calm.Calm != "152 devices held to required checks · 84 measured on server checks only (v2.0 images)." {
		t.Errorf("calm = %+v", calm)
	}
}

func TestK1CardsAndGroups(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	var trend []k1Day
	for i := 0; i < 14; i++ {
		trend = append(trend, k1Day{Day: now.AddDate(0, 0, i-13), InScope: 150, Compliant: 110 + i, Partly: 90 - i})
	}
	fixes := make([]int, 14)
	fixes[12], fixes[13], fixes[2] = 3, 2, 1
	cards := buildK1Cards(trend, fixes, 1, 82, 27, 150, 77, parseK1Scope(url.Values{}, "checks", nil, nil))
	if c := cards[0]; c.A.Value != "82%" || c.A.Delta != "▲ 5 pts vs last week" || c.A.Tone != "good" || c.B.Delta != "▼ 7" || c.LineA == "" {
		t.Errorf("compliant card = %+v", c)
	}
	if c := cards[1]; c.A.Value != "5" || c.A.Delta != "▲ 4" || c.A.Tone != "flat" || c.B.Label != "check with auto-fix on" {
		t.Errorf("auto-fix card = %+v", c)
	}
	if c := cards[2]; c.A.Value != "73" || c.A.Delta != "▲ 7" || c.B.Value != "77" {
		t.Errorf("measurable card = %+v", c)
	}
	// No week-ago data: no deltas, no invented numbers.
	if c := buildK1Cards(trend[7:], nil, 0, 82, 27, 150, 77, k1Scope{})[0]; c.A.Delta != "" {
		t.Errorf("a delta with no week-ago day: %+v", c.A)
	}

	checks, rules := k1SampleChecks()
	groups := buildK1Groups(checks, rules, map[string]int{"wireless_adb_off": 4}, parseK1Scope(url.Values{}, "checks", nil, nil))
	if len(groups) != 3 || groups[0].Name != "Setup" || groups[1].Name != "Debugging" || groups[2].Name != "Intact" {
		t.Fatalf("groups = %+v", groups)
	}
	if r := groups[0].Rows[0]; r.ID != "firmware_approved" || r.Action != "Releases" {
		t.Errorf("first setup row = %+v", r)
	}
	if r := groups[1].Rows[0]; !r.AutoFix || r.FixedWeek != 4 || r.Action != "Devices" {
		t.Errorf("adb row = %+v", r)
	}
	req := buildK1Groups(checks, rules, nil, parseK1Scope(url.Values{"level": {"recommended"}}, "checks", nil, nil))
	if len(req) != 1 || req[0].Rows[0].ID != "security_patch_recent" {
		t.Errorf("level pill keeps recommended only: %+v", req)
	}
}

func TestK1ScopeAndRail(t *testing.T) {
	vid := uuid.New()
	sc := parseK1Scope(url.Values{"venue": {vid.String()}, "type": {"t7"}, "level": {"required"}}, "venues",
		map[string]string{vid.String(): "Mom's Biryani"}, []string{"t7", "kds"})
	if !sc.On || sc.VenueName != "Mom's Biryani" || sc.ClassName != "Tableside AI" || sc.Level != "required" {
		t.Fatalf("scope = %+v", sc)
	}
	if h := sc.TabHref("checks"); h != "/compliance?level=required&type=t7&venue="+vid.String() {
		t.Errorf("checks tab keeps the level: %q", h)
	}
	if h := sc.TabHref("devices"); strings.Contains(h, "level=") || !strings.Contains(h, "tab=devices") {
		t.Errorf("the level is a Checks-only filter: %q", h)
	}
	if h := sc.DevHref("failing", "wireless_adb_off"); h != "/compliance?check=wireless_adb_off&show=failing&tab=devices&type=t7&venue="+vid.String() {
		t.Errorf("devices link = %q", h)
	}
	if bad := parseK1Scope(url.Values{"venue": {uuid.NewString()}, "type": {"nope"}}, "checks", nil, nil); bad.On {
		t.Errorf("unknown venue/type fall back to everything: %+v", bad)
	}

	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	v1, v2 := uuid.New(), uuid.New()
	venues := []compVenueView{{Name: "Mom's Biryani", ID: &v1, Devices: 2, Pct: 0, Worst: "Checked in recently"}, {Name: "Olive & Ash", ID: &v2, Devices: 28, Pct: 100}, {Name: "Not at a venue", Pct: -1}}
	r := buildK1Rail(78, 118, 152, 84, []k1Day{{Day: now, InScope: 152, Compliant: 118}}, venues, true, now)
	if r.Tone != "warn" || !r.HasTrend || len(r.Bars) != 1 || len(r.Behind) != 1 || r.Report.Next != "1 Nov" || r.Report.VenueID != v1.String() || r.Report.Venues != 2 {
		t.Errorf("rail = %+v", r)
	}
}

func TestK1FixesDaily(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	fixes := []db.SystemFix{
		{Type: "adb_tcp", By: "compliance", At: now.Add(-2 * time.Hour), Status: "completed"},
		{Type: "adb_tcp", By: "compliance", At: now.AddDate(0, 0, -10), Status: "completed"},
		{Type: "adb_tcp", By: "compliance", At: now.Add(-time.Hour), Status: "failed"},
		{Type: "app_update", By: "auto-update", At: now, Status: "completed"},
	}
	daily, week := k1FixesDaily(fixes, now, 14)
	if daily[13] != 1 || daily[3] != 1 || week["wireless_adb_off"] != 1 {
		t.Errorf("daily = %v, week = %v", daily, week)
	}
}

// TestCompliancePageRendersK1 executes compliance.html with the sample on each tab.
func TestCompliancePageRendersK1(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/compliance.html"))

	now := time.Now()
	checks, rules := k1SampleChecks()
	v1 := uuid.New()
	venues := []compVenueView{{Name: "Mom's Biryani", ID: &v1, Devices: 2, Required: 2, Fails: 2, Pct: 0, Worst: "Checked in recently"}}
	sc := parseK1Scope(url.Values{}, "checks", map[string]string{v1.String(): "Mom's Biryani"}, []string{product.ClassT7})
	var trend []k1Day
	for i := 0; i < 14; i++ {
		trend = append(trend, k1Day{Day: now.AddDate(0, 0, i-13), InScope: 152, Compliant: 105 + i})
	}
	data := map[string]any{
		"Tab": "checks", "Pct": 78, "InScope": 152, "Compliant": 118, "Failing": 34, "Partly": 84, "RecOnly": 6,
		"Checks": checks, "Venues": venues, "Counts": map[string]int{"failing": 34}, "Show": "failing", "Scored": 158, "CheckF": "", "CheckName": "",
		"Scope":    sc,
		"K1Banner": buildK1Banner(checks, rules, map[string]int{"wireless_adb_off": 4}, sc, 78, 34, 152, 84),
		"K1Cards":  buildK1Cards(trend, make([]int, 14), 1, 78, 34, 152, 84, sc),
		"K1Groups": buildK1Groups(checks, rules, map[string]int{"wireless_adb_off": 4}, sc),
		"K1Rail":   buildK1Rail(78, 118, 152, 84, trend, venues, false, now),
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "compliance.html", data); err != nil {
		t.Fatalf("checks tab: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`id="page-island"`, `class="cc-btn sm isl-dup" href="/compliance/rules"`,
		`aria-current="page">Checks</a>`, `Level: <b>All</b>`, `Venue: <b>All</b>`, `Type: <b>All</b>`,
		`4 to fix first`, `<span class="k1-who">a rollout</span>`, `href="/releases">Releases</a>`, `1 more in the table`,
		`>Compliant</a>`, `>Auto-fix</a>`, `>Measurable</a>`,
		`<div class="k1-grp">Setup</div>`, `<div class="k1-grp">Debugging</div>`, `<div class="k1-grp">Intact</div>`,
		`auto-fix on · 4 this week`, `AT070AABU00455`, `href="/compliance?check=firmware_approved&amp;show=all&amp;tab=devices"`,
		`78%</b><small>compliant</small>`, `% compliant each day, last 14 days`, `Venues behind`, `Mom&#39;s Biryani`,
		`Settings › customer compliance reports is off`, `/compliance.pdf`, `/static/js/oc-menus.js`,
		`Unknown (not reported by its client`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("checks tab is missing %q", want)
		}
	}
	for _, tab := range []string{"venues", "devices"} {
		data["Tab"] = tab
		data["Scope"] = parseK1Scope(url.Values{"type": {product.ClassT7}}, tab, nil, []string{product.ClassT7})
		buf.Reset()
		if err := tmpl.ExecuteTemplate(&buf, "compliance.html", data); err != nil {
			t.Fatalf("%s tab: %v", tab, err)
		}
		out = buf.String()
		if strings.Contains(out, "Level: <b>") {
			t.Errorf("%s tab shows the Checks-only Level pill", tab)
		}
		if tab == "devices" && !strings.Contains(out, `href="/compliance?show=failing&amp;tab=devices&amp;type=t7"`) {
			t.Errorf("devices filters keep the type scope")
		}
		if tab == "venues" && !strings.Contains(out, `/restaurants/`+v1.String()+`/compliance.pdf`) {
			t.Errorf("venues rows get the PDF")
		}
	}
}
