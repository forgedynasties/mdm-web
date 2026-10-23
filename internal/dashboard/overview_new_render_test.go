package dashboard

import (
	"bytes"
	"fmt"
	"html/template"
	"net/http/httptest"
	"os"
	"regexp"
	"strconv"
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

	// Overview C (21 Oct): pills, to-do banner, delta cards, pill chart, rollouts.
	cool, warm := 31.0, 47.0
	devs := []ovDev{
		{ID: uuid.New(), Class: "t7", Venue: &rid, Online: true, SeenAt: now, Battery: 12, HasBatt: true, TempC: &cool},
		{ID: uuid.New(), Class: "t7", Venue: &rid, Online: false, SeenAt: now.Add(-2 * time.Hour), Battery: 80, HasBatt: true, TempC: &cool},
		{ID: hot, Class: "kiosk", Venue: &rid, Online: true, SeenAt: now, TempC: &warm},
		{ID: uuid.New(), Class: "", Online: true, SeenAt: now, Unplaced: true},
	}
	req := httptest.NewRequest("GET", "/", nil)
	sc := parseOvScope(req, "/", []string{"t7", "kiosk"}, []ovVenueOpt{{rid, "Pho 88"}})
	tr := db.ScopedTrend{Crashes24h: 2, CrashesPrev: 5}
	for i := 0; i < 8; i++ {
		tr.Days = append(tr.Days, db.ScopedDay{Day: now.AddDate(0, 0, i-7), Active: 2 + i%2, Crashes: i})
	}
	rollouts := []db.Rollout{
		{Kind: "app_update", Package: "com.aioapp.mdm", Name: "1.13.10", Who: "auto-update",
			Start: now.Add(-48 * time.Hour), End: now.Add(-30 * time.Hour), Total: 3, Done: 3},
		{Kind: "ota", Name: "v2.1.034", Who: "ali@aioapp.com", Status: "active", Start: now.Add(-3 * time.Hour),
			Total: 2, Done: 1, InFlight: 1, ReleaseID: 7, DeploymentID: 11},
	}

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
		"OverviewSwitch": true, "Role": "admin", "AlertsOpenCount": 1, "DateLine": "Tuesday, October 21",
		"Scope":      sc,
		"OvBanner":   buildOvBanner(att, attN, nil, nil, nil, tr, sc, 3, 4),
		"OvStats":    buildOvStats(tr, devs, sc, now),
		"OvPills":    buildOvPills(devs, alerts, sc),
		"OvRollouts": buildOvRollouts(rollouts, now, true),
		"Hygiene": hygieneRows(db.FleetHygiene{Fleet: 181, Silent: 86, PlacedSilent: 10, PlacedSilentAt: []string{"Huanchaco Peruvian 6", "Flights Vegas 4"},
			DeadPad: 9, Builds: 19, OddBuilds: []string{"A15-v1.62-user", "old-key", "clean-build", "4.0.87"}}),
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "overview_new.html", data); err != nil {
		t.Fatalf("overview_new.html failed to render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		// C (21 Oct): filter pills, a to-do banner, delta cards, pill chart, rollouts.
		`Type: <b>All</b>`, `Venue: <b>All</b>`, `Window: <b>24 h</b>`,
		`href="/?type=kiosk"`, `href="/?window=7d"`,
		`class="ov2-ban is-warn"`,  // one warning problem, none critical
		`2 things to look at today`, // the overheating device and the unassigned one
		`1 new device waiting for a venue`,
		`href="/devices?onboarding=pending">Assign</a>`,
		`Fleet health · 81 · Healthy`,
		`>Devices</a>`, `>Stability</a>`, `>Battery</a>`,
		`75%</b><small>online · vs yesterday</small>`,
		`▼ 3</span><b>2</b><small>crashes · 24 h</small>`, // crashes down is good
		`<span class="lb">TSAI</span>`, `<span class="lb">Kiosk</span>`, `Total: <b>3</b>`, // the untyped one is left out
		`Firmware MDM 1.13.10`, `Firmware v2.1.034`, `href="/releases/7/deployments/11"`, `1 running`,
		`class="oc-tb run"`, `>A</span>`,
		// O2 (20 Oct): venue cards, clean-up table, and the rail.
		`1 needs a look`,
		`<b>Pho 88</b>`,
		`2 of 3 online · 1 offline`,
		`<i class="off"></i><i class="on"></i><i class="on"></i>`,
		`At risk · 42`,
		`1 device waiting for a site`, // devices at no venue
		`<b>Compliance</b>`,
		`>75%</text>`,
		`<em>1 failing</em>`,
		`Most failed: Wireless adb off`,
		`id="ovd-intro"`, // the first-visit intro
		`class="ov2-rail"`,
		`<td class="num">86</td>`, // clean-up table
		`48% of the fleet, none in a restaurant.`,
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

// TestOverviewScoped renders the page filtered to one type at one venue: the pills say
// so, the banner names the scope, and the chart keeps only that type.
func TestOverviewScopedPills(t *testing.T) {
	rid := uuid.New()
	req := httptest.NewRequest("GET", "/?type=kds&venue="+rid.String()+"&window=7d", nil)
	sc := parseOvScope(req, "/", []string{"t7", "kds"}, []ovVenueOpt{{rid, "Pho 88"}})
	if sc.Class != "kds" || sc.Venue == nil || sc.Days != 7 || sc.Label != "Kitchen displays at Pho 88" {
		t.Fatalf("scope = %+v", sc)
	}
	if sc.ClearHref != "/?window=7d" {
		t.Errorf("clear keeps the window: got %q", sc.ClearHref)
	}
	devs := []ovDev{
		{ID: uuid.New(), Class: "kds", Venue: &rid, Online: true},
		{ID: uuid.New(), Class: "t7", Venue: &rid, Online: true},
		{ID: uuid.New(), Class: "kds", Online: false},
	}
	p := buildOvPills(devs, nil, sc)
	if len(p.Cols) != 1 || p.Cols[0].Class != "kds" || p.Total != 1 {
		t.Errorf("pills = %+v", p)
	}
	if st := buildOvStats(db.ScopedTrend{}, devs, sc, time.Now()); st[2].Empty == "" {
		t.Error("a KDS-only scope has no battery devices, the Battery card should say so")
	}
	// A venue nobody can see is ignored, not an error.
	bad := parseOvScope(httptest.NewRequest("GET", "/?venue="+uuid.NewString()+"&type=nope", nil), "/", nil, nil)
	if bad.On {
		t.Errorf("unknown venue and type should fall back to the fleet: %+v", bad)
	}
}

// TestOvBannerOrder: critical first, then the worst venue, then a crash spike, warnings
// and waiting devices; three rows, the rest counted.
func TestOvBannerOrder(t *testing.T) {
	now := time.Now()
	att := []attentionRow{
		{Severity: "critical", Issue: "Device offline", Serial: "T7-1", Restaurant: "Pho 88", Since: now.Add(-time.Hour), Href: "/alerts?device=T7-1"},
		{Severity: "warning", Issue: "Battery low", Devices: 4, Href: "/alerts"},
		{Severity: "info", Issue: "Waiting for a restaurant", Devices: 3, Href: "/devices?onboarding=pending"},
	}
	gid := uuid.New()
	stale := 57.8
	groups := []db.GroupHealth{{GroupID: gid, Name: "Mom's Biryani", DeviceCount: 5, OfflineCount: 2, ScoreClass: "danger", Score: 40, TempMax: &stale}}
	// The 57.8 °C is an offline tablet's last reading: not "running hot".
	hotNow := ovHotVenues([]ovDev{{ID: uuid.New(), Venue: &gid, Online: false, TempC: &stale}})
	b := buildOvBanner(att, 3, groups, nil, hotNow, db.ScopedTrend{Crashes24h: 12, CrashesPrev: 4}, ovScope{}, 50, 60)
	if b.Tone != "bad" || b.Headline != "5 things to look at today" || len(b.Todos) != 3 || b.More != 2 {
		t.Fatalf("banner = %+v", b)
	}
	want := []string{"Device offline", "Mom's Biryani: 2 offline", "12 crashes in the last 24 h"}
	for i, w := range want {
		if b.Todos[i].Title != w {
			t.Errorf("row %d = %q, want %q", i, b.Todos[i].Title, w)
		}
	}
	calm := buildOvBanner(nil, 0, nil, nil, nil, db.ScopedTrend{}, ovScope{}, 52, 216)
	if calm.Tone != "calm" || calm.Headline != "Everything is running" || calm.Calm != "52 of 216 devices online. Nothing needs you right now." {
		t.Errorf("calm = %+v", calm)
	}
}

// TestOvRolloutsLayout: bars sit on a seven-day track, a running one ends at now, a
// bar stays on the track, and live ones survive the six-row cut.
func TestOvRolloutsLayout(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	var rs []db.Rollout
	for i := 0; i < 8; i++ {
		rs = append(rs, db.Rollout{Kind: "install_apk", Name: fmt.Sprintf("App %d", i), Who: "ops",
			Start: now.Add(-time.Duration(140-i*10) * time.Hour), End: now.Add(-time.Duration(139-i*10) * time.Hour), Total: 4, Done: 4})
	}
	rs = append(rs, db.Rollout{Kind: "ota", Name: "v2.1.040", Status: "active", Start: now.Add(-150 * time.Hour), Total: 10, Done: 2, InFlight: 8, ReleaseID: 1, DeploymentID: 2})
	// An empty "pending" deployment from weeks ago is not a rollout.
	rs = append(rs, db.Rollout{Kind: "ota", Name: "v2.0.96l", Status: "pending", Start: now.Add(-800 * time.Hour), End: now.Add(-800 * time.Hour), ReleaseID: 3, DeploymentID: 156})
	out := buildOvRollouts(rs, now, true)
	for _, b := range out.Bars {
		if b.Name == "Firmware v2.0.96l" {
			t.Error("a deployment with no devices is on the timeline")
		}
	}
	if len(out.Bars) != ovRolloutRows || out.Running != 1 {
		t.Fatalf("bars = %d, running = %d", len(out.Bars), out.Running)
	}
	if out.Bars[0].Name != "Firmware v2.1.040" || out.Bars[0].Tone != "run" {
		t.Errorf("the running firmware push should be kept and come first (oldest): %+v", out.Bars[0])
	}
	one := uuid.New()
	for _, c := range []struct {
		r    db.Rollout
		want string
	}{
		{db.Rollout{Kind: "install_apk", CommandID: one, Commands: 1}, "/commands/" + one.String()},
		{db.Rollout{Kind: "app_update", Who: "auto-update", CommandID: one, Commands: 76}, "/clients"},
		{db.Rollout{Kind: "install_apk", CommandID: one, Commands: 2, AppPackage: "aio.app.nugget"}, "/apps/aio.app.nugget"},
		{db.Rollout{Kind: "install_apk", CommandID: one, Commands: 3}, "/commands/" + one.String()},
		{db.Rollout{Kind: "ota", ReleaseID: 7, DeploymentID: 11}, "/releases/7/deployments/11"},
	} {
		if got := ovRolloutHref(c.r, true); got != c.want {
			t.Errorf("href(%+v) = %q, want %q", c.r, got, c.want)
		}
	}
	// Below Fleet manager the app library is closed, so the newest job opens instead.
	if got := ovRolloutHref(db.Rollout{Kind: "install_apk", CommandID: one, Commands: 2, AppPackage: "aio.app.nugget"}, false); got != "/commands/"+one.String() {
		t.Errorf("without the app library: %q", got)
	}
	if out.Days[6] != "Today" || out.Days[0] != "Thu 15" {
		t.Errorf("axis = %v", out.Days)
	}
	for _, b := range out.Bars {
		if l, _ := strconv.ParseFloat(b.Left, 64); l < 0 || l > 100 {
			t.Errorf("%s starts off the track at %s%%", b.Name, b.Left)
		}
	}
}
