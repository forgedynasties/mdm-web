package dashboard

import (
	"bytes"
	"encoding/json"
	"html/template"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// vpSample is Mom's Biryani on 21 Oct: both tablets dead for days at 2% and 1%, the staff
// PIN never set, open now; plus a warning alert on one of them.
func vpSample(now time.Time) vpInputs {
	id := uuid.New()
	warm := 57.8
	mk := func(serial string, ago time.Duration, batt int) vpDev {
		d := vpDev{Serial: serial, Firmware: true}
		d.ID, d.Class, d.Venue, d.SeenAt, d.Battery, d.HasBatt, d.TempC = uuid.New(), product.ClassT7, &id, now.Add(-ago), batt, true, &warm
		return d
	}
	a, b := mk("AT070AABU00455", 98*time.Hour, 2), mk("AT070AABU00767", 75*time.Hour, 1)
	return vpInputs{
		ID: id, Name: "Mom's Biryani", Devices: []vpDev{a, b},
		Alerts: []db.Alert{
			{Type: "device_offline", DeviceID: &a.ID, Serial: a.Serial, Severity: "critical", Summary: "Offline", FiredAt: now.Add(-90 * time.Hour), RestaurantID: id.String()},
			{Type: "battery_low", DeviceID: &b.ID, Serial: b.Serial, Severity: "warning", Summary: "Battery low", FiredAt: now.Add(-80 * time.Hour), RestaurantID: id.String()},
		},
		Health: &db.GroupHealth{GroupID: id, Name: "Mom's Biryani", DeviceCount: 2, OfflineCount: 2, Score: 57, ScoreClass: "warn"},
		Open:   true, OpenAt: "12:00", CloseAt: "23:30",
		Window: "7d", Now: now,
	}
}

func TestVenueP1Banner(t *testing.T) {
	now := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	in := vpSample(now)
	b := buildVpBanner(in)
	if b.Kicker != "Venue health · 57 · Watch · open now, closes 23:30" {
		t.Errorf("kicker = %q", b.Kicker)
	}
	// The offline alert names a device the "offline for over a day" row already lists, so
	// it is not a row of its own; the 57.8 °C is an offline tablet's last reading, so no
	// "running hot" either.
	if b.Tone != "warn" || len(b.Todos) != 3 {
		t.Fatalf("banner = %+v", b)
	}
	if b.Todos[0].Title != "Both devices offline for over a day: AT070AABU00455 (4 d), AT070AABU00767 (3 d)" ||
		b.Todos[0].Sub != "last seen at 2% and 1% battery: likely off their chargers" {
		t.Errorf("row 1 = %+v", b.Todos[0])
	}
	if b.Todos[1].Title != "Staff PIN is still the default 147147" || b.Todos[1].Href != "?open=lock#vn-lock" {
		t.Errorf("row 2 = %+v", b.Todos[1])
	}
	if b.Todos[2].Action != "Open" || b.Headline != "3 things to look at at Mom's Biryani" {
		t.Errorf("row 3 / headline = %+v / %q", b.Todos[2], b.Headline)
	}
	for _, td := range b.Todos {
		if strings.Contains(td.Title, "hot") {
			t.Errorf("an offline device's stale temperature made a row: %q", td.Title)
		}
	}

	// A set PIN, devices back online and no alerts: the calm banner.
	in.StaffPIN, in.Alerts = "4321", nil
	for i := range in.Devices {
		in.Devices[i].Online, in.Devices[i].SeenAt = true, now
		cool := 33.0
		in.Devices[i].TempC = &cool
	}
	calm := buildVpBanner(in)
	if calm.Tone != "calm" || calm.Headline != "Everything is running at Mom's Biryani" || calm.Calm != "2 of 2 devices online. Open until 23:30." {
		t.Errorf("calm = %+v", calm)
	}
	// One online device running hot is news, with its own link.
	hot := 49.0
	in.Devices[0].TempC = &hot
	if r := buildVpBanner(in); len(r.Todos) != 1 || r.Todos[0].Title != "AT070AABU00455 running hot (49 °C)" || r.Todos[0].Href != "/devices/AT070AABU00455" {
		t.Errorf("hot = %+v", r.Todos)
	}
	// Mains-only venues (no firmware client) have no staff PIN to nag about.
	in.Devices[0].TempC = nil
	for i := range in.Devices {
		in.Devices[i].Firmware = false
	}
	in.StaffPIN = ""
	if r := buildVpBanner(in); len(r.Todos) != 0 {
		t.Errorf("no firmware devices, no PIN row: %+v", r.Todos)
	}
	if e := buildVpBanner(vpInputs{Name: "New place", Now: now}); e.Headline != "No devices at New place yet" {
		t.Errorf("empty venue = %+v", e)
	}
}

func TestVenueP1Cards(t *testing.T) {
	now := time.Date(2026, 10, 21, 14, 0, 0, 0, time.UTC)
	in := vpSample(now)
	for i := 0; i < 14; i++ {
		in.Trend.Days = append(in.Trend.Days, db.ScopedDay{Day: now.AddDate(0, 0, i-13), Active: 2 - i/10, Crashes: i % 2})
	}
	p := buildVenueP1(in)
	if len(p.Stats) != 3 || p.Stats[0].Title != "Online" || p.Stats[0].A.Value != "0%" || p.Stats[0].B.Value != "2" {
		t.Fatalf("stats = %+v", p.Stats)
	}
	// Two dead tablets are battery devices gone quiet, not mains power.
	if e := p.Stats[2].Empty; e != "No battery reading in the last day: the 2 battery devices here are offline." {
		t.Errorf("battery card = %q", e)
	}
	if !strings.Contains(p.Stats[0].Href, "restaurant="+in.ID.String()) {
		t.Errorf("the Online card should open the venue's devices: %q", p.Stats[0].Href)
	}
	if p.Pills.Total != 2 || len(p.Pills.Cols) != 1 || p.Pills.Cols[0].Offline != 2 || p.Pills.Cols[0].Alerts != 2 {
		t.Errorf("pills = %+v", p.Pills)
	}
	if len(p.Windows) != 3 || !p.Windows[1].Active || p.Windows[1].Href != "?window=7d" {
		t.Errorf("windows = %+v", p.Windows)
	}
	if vpWindow("") != "7d" || vpWindow("30d") != "30d" || vpWindow("365d") != "7d" {
		t.Error("window parsing")
	}
}

// TestVenueP1Renders executes restaurant_detail.html with the sample: banner, cards, pill
// chart, the device table and the settings rail all present, the island unchanged.
func TestVenueP1Renders(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := template.FuncMap{}
	for _, re := range []*regexp.Regexp{
		regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`),
		regexp.MustCompile(`funcMap\["([a-zA-Z][a-zA-Z0-9]*)"\]`),
	} {
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			funcs[m[1]] = func(...any) any { return "" }
		}
	}
	funcs["dict"] = func(kv ...any) map[string]any {
		m := map[string]any{}
		for i := 0; i+1 < len(kv); i += 2 {
			m[kv[i].(string)] = kv[i+1]
		}
		return m
	}
	funcs["canAdminOrOperator"] = func(string) bool { return true }
	funcs["canAdmin"] = func(string) bool { return true }
	funcs["sub"] = func(a, b int) int { return a - b }
	funcs["add"] = func(a, b int) int { return a + b }
	funcs["ramPct"] = func(string) int { return 0 }
	funcs["batteryFillW"] = func(int) int { return 0 }
	funcs["serialHead"] = func(s string) string { return s[:len(s)-serialTailLen(s)] }
	funcs["serialTail"] = func(s string) string { return s[len(s)-serialTailLen(s):] }
	funcs["venueIcon"] = func(id any, name, size string) template.HTML { return `<span class="vicon"></span>` }
	funcs["venuePauseChoices"] = func() []struct{ Value, Label string } { return venuePauseChoices }
	funcs["withNoHead"] = func(data map[string]any) map[string]any {
		c := map[string]any{"NoHead": true}
		for k, v := range data {
			c[k] = v
		}
		return c
	}
	// Every template (the settings card pulls in rows defined elsewhere), then a bare
	// header and footer over the layout's.
	tmpl := template.Must(template.New("").Funcs(funcs).ParseGlob("../../templates/*.html"))
	// (An empty body would not replace the layout's: html/template keeps the old one.)
	template.Must(tmpl.Parse(`{{define "header"}}<i hidden></i>{{end}}{{define "footer"}}<i hidden></i>{{end}}`))

	now := time.Now()
	in := vpSample(now)
	rest := &db.Restaurant{ID: in.ID, Name: in.Name, Address: "103 E Fremont Ave, Sunnyvale", Timezone: "America/Los_Angeles", DeviceCount: 2}
	var devices []db.Device
	for _, d := range in.Devices {
		devices = append(devices, db.Device{ID: d.ID, SerialNumber: d.Serial, Product: "t7", AgentKind: "firmware",
			BatteryPct: d.Battery, LastSeenAt: d.SeenAt, RestaurantID: &in.ID, LatestExtra: json.RawMessage(`{}`)})
	}
	data := map[string]any{
		"Role": "admin", "Restaurant": rest, "Devices": devices, "Online": map[uuid.UUID]bool{}, "OnlineCount": 0,
		"ActiveThresholdSecs": 180, "ServiceWindow": serviceWindowView{GroupID: in.ID.String(), Open: "12:00", Close: "23:30"},
		"Location": venueLocation{}, "Kiosk": &venueKioskView{Total: 2}, "P1": buildVenueP1(in),
		"Updates": venueUpdatesView{Mode: "closed", Paused: true, PausedSub: "resumes Fri 24 Oct, 09:00", Summary: "Paused",
			SummarySub: "resumes Fri 24 Oct, 09:00", FleetWindow: "02:00 – 05:00", ClosedWindow: "23:30 – 12:00"},
		"Lost": venueLostView{SelfLockMinutes: 15, Paused: true, PausedSub: "resumes Sat 25 Oct, 14:00", Summary: "Paused",
			SummarySub: "resumes Sat 25 Oct, 14:00"},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "restaurant_detail.html", data); err != nil {
		t.Fatalf("restaurant_detail.html failed to render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`<template id="page-island">`, `class="isl-dup-sr"`,
		`class="ov2 vp1"`, `class="ov2-ban is-warn"`, `3 things to look at at Mom&#39;s Biryani`,
		`Both devices offline for over a day: AT070AABU00455 (4 d), AT070AABU00767 (3 d)`,
		`href="?open=lock#vn-lock">Set PIN</a>`,
		`href="?window=7d" class="on"`, `>Online</a>`, `>Stability</a>`, `>Battery</a>`,
		`class="card vp1-dev"`, `/members?head=0`, `data-class="t7"`,
		`id="restaurant-members"`, `AT070AABU00455`,
		`class="vp1-rail"`, `id="vn-settings"`, `id="vn-lock"`,
		`id="vn-enrol"`, `action="/restaurants/` + in.ID.String() + `/auto-enroll"`, `name="on" value="1"`,
		`id="vn-updates"`, `<b class="vu-paused">Paused</b>`, `Resume goes back to while closed`,
		`name="action" value="resume"`, `value="closed"`, `Outside service hours: 23:30 – 12:00 now`,
		`name="pause" value="3d" checked`, `value="paused" checked`, `action="/restaurants/` + in.ID.String() + `/updates"`,
		`id="vn-lost"`, `action="/restaurants/` + in.ID.String() + `/lost"`, `Resume goes back to watching and locking after 15 minutes`,
		`value="lock"`, `<option value="15" selected>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("venue page is missing %q", want)
		}
	}
	if strings.Contains(out, `/self-lock"`) {
		t.Error("the self-lock form is still in the Device lock row")
	}
	if strings.Contains(out, `class="vn-grid"`) {
		t.Error("the old two-column grid is back")
	}
}

func TestVenueTabs(t *testing.T) {
	now := time.Now()
	in := vpSample(now)
	if tabs := buildVenueP1(in).Tabs; len(tabs) != 2 || tabs[0].Label != "All" || tabs[1].Class != product.ClassT7 {
		t.Fatalf("one type: tabs = %+v, want All + Tableside AI", tabs)
	}
	k := vpDev{Serial: "K1"}
	k.ID, k.Class, k.Venue, k.Online, k.SeenAt = uuid.New(), product.ClassKiosk, &in.ID, true, now
	in.Devices = append(in.Devices, k)
	tabs := buildVenueP1(in).Tabs
	if len(tabs) != 3 {
		t.Fatalf("tabs = %+v", tabs)
	}
	all := tabs[0]
	if all.Total != 3 || all.Online != 1 || all.Offline != 2 || all.Alerts != 2 || all.POn+all.POff+all.PAl != 100 {
		t.Errorf("All = %+v", all)
	}
	if tabs[1].Class != product.ClassT7 || tabs[2].Class != product.ClassKiosk || tabs[2].POn != 100 {
		t.Errorf("types = %+v, want TSAI (2) then kiosk (1, all online)", tabs[1:])
	}
}
