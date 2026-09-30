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
	on, off, hot, loose := uuid.New(), uuid.New(), uuid.New(), uuid.New()
	devs := []db.WallDevice{
		{ID: on, Serial: "T7-0001", Class: "t7", RestaurantID: &rid, LastSeenAt: now},
		{ID: off, Serial: "T7-0002", Class: "t7", RestaurantID: &rid, LastSeenAt: now.Add(-2 * time.Hour)},
		{ID: hot, Serial: "K22-0003", Class: "kiosk", RestaurantID: &rid, LastSeenAt: now},
		{ID: loose, Serial: "MB-0004", Class: "dongle", LastSeenAt: now},
	}
	connected := map[uuid.UUID]struct{}{on: {}, hot: {}, loose: {}}
	alerts := []db.Alert{{Type: "device_overheating", DeviceID: &hot, Serial: "K22-0003", Severity: "warning", FiredAt: now}}
	groups := []db.GroupHealth{{GroupID: rid, Name: "Pho 88", DeviceCount: 3, OfflineCount: 1, Deployed: true, ScoreClass: "danger"}}
	wall := buildWall(devs, connected, alerts, groups, map[uuid.UUID]string{rid: "Pho 88"}, now)
	lat, lng := 33.77, -117.94
	mapJS, dots := buildMap(wall, []db.Restaurant{{ID: rid, Name: "Pho 88", Latitude: &lat, Longitude: &lng}}, nil, groups)
	att, attN, attCrit := buildAttention(alerts, 1)

	data := map[string]any{
		"Summary": db.Summary{Total: 4, RecentlyActive: 3, KioskCount: 2, UniqueBuilds: 2}, "Offline": 1, "NowUTC": now.UTC().Format(time.RFC3339),
		"Score": 81, "ScoreClass": "warn", "RingOffset": "52.5", "StatusWord": "Healthy", "ScoreDelta": -5,
		"Hot": 1, "KioskPct": 50, "DeployedCount": 3, "DeployedPct": 75, "RolloutsCount": 0,
		"Products": []compRole{{"t7", "Tableside AI", 2}, {"", "Unassigned", 2}},
		"SitesOK":  0, "SitesWarn": 0, "SitesBad": 1, "Crashes24h": 0, "CrashDevices24h": 0,
		"AttentionRows": att, "AttentionTotal": attN, "AttentionCritical": attCrit,
		"Wall": wall, "WallTotal": len(devs), "MapData": mapJS, "MapDots": dots, "MapsEmbedKey": "k",
		"OverviewSwitch": true, "Role": "admin", "AlertsOpenCount": 1,
		"Uptime": uptimeRows([]db.RestaurantUptime{{ID: rid, Name: "Pho 88", Days: []db.UptimeDay{
			{Date: now.AddDate(0, 0, -1), Expected: 100, Up: 97, DownDevice: 1}, {Date: now, Expected: 0}}}}, nil),
		"Hygiene": hygieneRows(db.FleetHygiene{Fleet: 181, Silent: 86, PlacedSilent: 10, PlacedSilentAt: []string{"Huanchaco Peruvian 6", "Flights Vegas 4"},
			DeadPad: 9, Builds: 19, OddBuilds: []string{"A15-v1.62-user", "old-key", "clean-build", "4.0.87"}}),
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "overview_new.html", data); err != nil {
		t.Fatalf("overview_new.html failed to render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`href="/devices/T7-0002" class="off"`,        // offline square
		`href="/devices/K22-0003" class="warn"`,      // alerting square
		`Menu board · Not deployed`,                  // loose device, grouped by type
		`id="ovd-map-data"`,                          // map data shipped
		`"name":"Pho 88"`,                            // restaurant on the map
		`id="ovd-intro"`,                             // the first-visit intro
		`href="/?overview=classic" hx-boost="false"`, // the switch, a full load past the boosted "/" cache
		`class="ov3-hero is-warn"`,                   // the classic hero: score, right now, composition
		`Tableside AI <b>2</b>`,
		`<span class="p warn">97.0%</span>`,          // service uptime, one restaurant
		`97.0% up · 1 device missed time`,
		`<b>86</b> devices silent 14&#43; days<small>48% of the fleet, none in a restaurant.`, // clean-up checklist
		`href="/devices?hygiene=placed-silent"`,
		`4 are not a release: A15-v1.62-user, old-key, clean-build, ….`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("rendered page is missing %q", want)
		}
	}
	if dots != 1 {
		t.Errorf("map dots = %d, want 1 (restaurants only)", dots)
	}
}
