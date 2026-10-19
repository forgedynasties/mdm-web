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

// TestOverviewNewRenders executes the new Overview against a small fleet: one
// restaurant with an offline and an alerting device, and one device with no
// restaurant. The page is super admin only on live, so a template error would
// otherwise surface as a 500 on the first admin login.
func TestOverviewNewRenders(t *testing.T) {
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
	funcs["canAdminOrOperator"] = func(string) bool { return true }
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}`))
	// overview.html too: the new page borrows its "ov3-w-hero" block.
	tmpl = template.Must(tmpl.ParseFiles("../../templates/overview_new.html", "../../templates/overview.html"))

	now := time.Now()
	rid := uuid.New()
	hot := uuid.New()
	alerts := []db.Alert{{Type: "device_overheating", DeviceID: &hot, Serial: "K22-0003", Severity: "warning", FiredAt: now}}
	att, attN, attCrit := buildAttention(alerts, 1)

	data := map[string]any{
		"Summary": db.Summary{Total: 4, RecentlyActive: 3, KioskCount: 2, UniqueBuilds: 2}, "Offline": 1, "NowUTC": now.UTC().Format(time.RFC3339),
		"Score": 81, "ScoreClass": "warn", "RingOffset": "52.5", "StatusWord": "Healthy", "ScoreDelta": -5,
		"Hot": 1, "KioskPct": 50, "DeployedCount": 3, "DeployedPct": 75, "RolloutsCount": 0,
		"Products": []compRole{{"t7", "Tableside AI", 2}, {"", "Unassigned", 2}},
		"SitesOK":  0, "SitesWarn": 0, "SitesBad": 1, "Crashes24h": 0, "CrashDevices24h": 0,
		"AttentionRows": att, "AttentionTotal": attN, "AttentionCritical": attCrit,
		"Venues": overviewVenues{Rows: []venueRow{{ID: rid.String(), Name: "Pho 88", Href: "/restaurants/" + rid.String(), Devices: 3, Online: 2, Offline: 1,
			Pips: []string{"off", "on", "on"}, Class: "danger", NeedsLook: true, HasH: true, H: db.GroupHealth{Score: 42, OpenWarning: 1}}},
			Total: 1, NeedLook: 1, Unplaced: 1},
		"Compliance": &compWidget{InScope: 4, Compliant: 3, Failing: 1, Pct: 75, OkDash: "1 1", FailDash: "1 1",
			HasDelta: true, Delta: -5, MoreChecks: 2, Partly: 1,
			Top: []compWidgetCheck{{ID: "wireless_adb_off", Name: "Wireless adb off", Line: "all at Pho 88 · auto-fix on", Count: 1, Href: "/compliance?tab=devices&show=failing&check=wireless_adb_off"}}},
		"OverviewSwitch": true, "Role": "admin", "AlertsOpenCount": 1,
		"Hygiene": hygieneRows(db.FleetHygiene{Fleet: 181, Silent: 86, PlacedSilent: 10, PlacedSilentAt: []string{"Huanchaco Peruvian 6", "Flights Vegas 4"},
			DeadPad: 9, Builds: 19, OddBuilds: []string{"A15-v1.62-user", "old-key", "clean-build", "4.0.87"}}),
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "overview_new.html", data); err != nil {
		t.Fatalf("overview_new.html failed to render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`<h2>Venues</h2>`, // the Venues widget (V1)
		`<b>1 needs a look</b>`,
		`2 of 3 online · 1 offline · 1 warning`,
		`<i class="off"></i><i class="on"></i><i class="on"></i>`,
		`<span class="ovv-sc s-danger">42</span>`,
		`1 device waiting for a site`, // devices at no venue
		`<h2>Compliance</h2>`,         // the compliance widget (C1)
		`>75%</text>`,
		`<b>1</b> failing a required check`,
		`▼ 5 pts vs last week`,
		`all at Pho 88 · auto-fix on`,
		`href="/compliance?tab=devices&amp;show=failing&amp;check=wireless_adb_off">Show</a>`,
		`+ 2 more checks failing · 1 device can't be measured`,
		`id="ovd-intro"`,           // the first-visit intro
		`class="ov3-hero is-warn"`, // the classic hero: score, right now, composition
		`Tableside AI <b>2</b>`,
		`<b>86</b> devices silent 14&#43; days<small>48% of the fleet, none in a restaurant.`, // clean-up checklist
		`href="/devices?hygiene=placed-silent"`,
		`4 are not a release: A15-v1.62-user, old-key, clean-build, …<`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered page is missing %q", want)
		}
	}
	if strings.Contains(out, `id="ovd-map"`) {
		t.Error("the map card is back on the Overview")
	}
}
