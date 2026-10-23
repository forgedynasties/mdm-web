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
	"mdm/internal/product"
)

// luAppsSample: Tableside AI with nugget 17.0.0 in the library, one unit on prod 18.0.0,
// one downloading, one failed, one offline, one next in line, one with no app list; a
// kiosk app with no build.
func luAppsSample(now time.Time) luAppsInputs {
	venue := uuid.New()
	dev := func(sn, product string, seen time.Duration) db.Device {
		return db.Device{ID: uuid.New(), SerialNumber: sn, Product: product, AgentKind: "firmware", LastSeenAt: now.Add(-seen), RestaurantID: &venue}
	}
	devs := []db.Device{
		dev("T-PROD", "t7", time.Minute), dev("T-DL", "t7", time.Minute), dev("T-FAIL", "t7", time.Minute),
		dev("T-OFF", "t7", 2*time.Hour), dev("T-NEXT", "t7", time.Minute), dev("T-UNK", "t7", time.Minute),
		dev("K-1", "kiosk22", time.Minute),
	}
	lib := map[string]*db.App{"aio.app.nugget": {PackageName: "aio.app.nugget", VersionName: "17.0.0", ApkURL: "u17", CreatedAt: now.Add(-40 * 24 * time.Hour)}}
	pct := 41
	return luAppsInputs{
		Roles:    []string{"t7", "kiosk", "kds"},
		RoleApps: []db.RoleApp{{Role: "t7", BasePackage: "aio.app.nugget", AutoInstall: true}, {Role: "kiosk", BasePackage: "aio.app.kiosk", AutoInstall: true}},
		Lib:      lib, Devices: devs,
		Installed: map[uuid.UUID]map[string]string{
			devs[0].ID: {"aio.app.nugget": "18.0.0"},
			devs[1].ID: {"aio.app.nugget.uatv2": "117-uatv2"},
			devs[3].ID: {"aio.app.nugget.uatv2": "48.0.0-uatv2", "aio.app.nugget.stagev2": "20.0.0-stagev2"},
			devs[6].ID: {"aio.app.nugget.uatv2": "1"}, // another product's app on a kiosk: not its
		},
		Missing: map[string]map[uuid.UUID]bool{
			"aio.app.nugget": {devs[1].ID: true, devs[2].ID: true, devs[3].ID: true, devs[4].ID: true},
			"aio.app.kiosk":  {devs[6].ID: true},
		},
		Installs: map[uuid.UUID]db.InstallState{
			devs[1].ID: {CommandID: uuid.New(), Status: "downloading", Progress: &pct, SentAt: now.Add(-8 * time.Minute)},
			devs[2].ID: {CommandID: uuid.New(), Status: "failed", Output: "error: ProtocolException: unexpected end of stream",
				SentAt: time.Date(2026, 10, 23, 11, 20, 0, 0, time.UTC), UpdatedAt: time.Date(2026, 10, 23, 11, 50, 0, 0, time.UTC)},
		},
		Venues:     map[uuid.UUID]string{venue: "Flights Vegas"},
		SeenWithin: 3 * time.Minute, Now: now,
	}
}

func TestLineupAppsBuilder(t *testing.T) {
	now := time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)
	cards, p := buildLineupApps(luAppsSample(now))
	if len(cards) != 2 || cards[0].Role != "t7" || !cards[0].On || cards[1].Role != "kiosk" {
		t.Fatalf("cards = %+v", cards)
	}
	if c := cards[0]; c.OnProd != 1 || c.Total != 6 || c.Running != 1 || c.Build != "17.0.0" || c.Note != "1 failed" {
		t.Errorf("t7 card = %+v", c)
	}
	if c := cards[1]; c.Bar[2].N != 0 || c.Build != "" || c.Note != "Upload a build to install on 1" || c.NoteTone != "warn" {
		t.Errorf("kiosk card = %+v", c)
	}
	if p == nil || len(p.Rows) != 6 {
		t.Fatalf("pane = %+v", p)
	}
	want := []struct{ sn, key, word string }{
		{"T-FAIL", "failed", "Failed"}, {"T-DL", "run", "Downloading 41%"}, {"T-NEXT", "wait", "Next in line"},
		{"T-OFF", "off", "Offline"}, {"T-UNK", "unknown", "No app list"}, {"T-PROD", "ok", "On prod"},
	}
	for i, w := range want {
		if r := p.Rows[i]; r.Serial != w.sn || r.Key != w.key || r.Word != w.word {
			t.Errorf("row %d = %+v, want %v", i, r, w)
		}
	}
	if r := p.Rows[0]; r.Detail != "ProtocolException: unexpected end of stream · retries 12:05" || !r.Retry {
		t.Errorf("failed row = %+v", r)
	}
	if r := p.Rows[3]; r.Has != ".stagev2 20.0.0-stagev2, .uatv2 48.0.0-uatv2" {
		t.Errorf("has now = %q", r.Has)
	}
	if p.Newer != "18.0.0" || p.NewerN != 1 || p.Pct != 16 || p.PctAfter != 33 {
		t.Errorf("pane = newer %q×%d pct %d→%d", p.Newer, p.NewerN, p.Pct, p.PctAfter)
	}
	in := luAppsSample(now)
	in.StateF = "off"
	if _, p2 := buildLineupApps(in); len(p2.Rows) != 1 || p2.Rows[0].Serial != "T-OFF" || p2.TotalShown != 6 {
		t.Errorf("filtered = %+v", p2.Rows)
	}
}

func TestLineupAppsRenders(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	funcs["canAdmin"] = func(role string) bool { return role == "admin" }
	funcs["canOperate"] = func(string) bool { return true }
	funcs["classShort"] = classShort
	funcs["classLabel"] = product.ClassLabel
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/pipeline.html", "../../templates/clients.html"))
	cards, pane := buildLineupApps(luAppsSample(time.Date(2026, 10, 23, 12, 0, 0, 0, time.UTC)))
	data := map[string]any{"Role": "admin", "Tab": "apps", "View": "apps", "AppsNoBuild": 1, "AppCards": cards, "AppsPane": pane,
		"AppChoicesLU": []lineupAppChoice{{Package: "aio.app.nugget", Label: "Nugget"}}}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "pipeline.html", data); err != nil {
		t.Fatalf("render: %v", err)
	}
	out := buf.String()
	for _, w := range []string{`<h1 class="cc-h1">Host apps</h1>`, `1 with no build in the library`, `class="lua-c on"`, `aio.app.nugget · 17.0.0`,
		`Upload a build to install on 1`, `Tableside AI · host app installs`, `action="/products/role-app-retry"`,
		`Downloading 41%`, `Auto-install on`, `1 unit already run 18.0.0`, `href="/apps/aio.app.nugget">Upload 18.0.0</a>`,
		`--a:16;--b:33`, `State: <b>All</b>`, `href="/host-apps?app=t7&amp;state=failed"`, `<option value="aio.app.nugget" selected>Nugget</option>`} {
		if !strings.Contains(out, w) {
			t.Errorf("missing %q", w)
		}
	}
	if strings.Contains(out, `class="lu1-tabs"`) {
		t.Error("Host apps is a page of its own: no Lineup tabs on it")
	}
}
