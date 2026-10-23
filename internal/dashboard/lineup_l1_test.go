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

	"mdm/internal/config"
	"mdm/internal/db"
	"mdm/internal/product"
)

// luSample is a Tableside AI role: one model on prod v2.1.034 with a newer v2.1.035 and
// devices on another line, the Firmware MDM client with auto-update off and some behind,
// and Nugget on most devices.
func luSample(now time.Time) luInputs {
	pub := now.Add(-26 * time.Hour)
	m := pipeModel{Key: "t7", Label: "AIO T7", Maker: "AIO", Mode: "prod", Firmware: 154, Line: "2.1",
		Channel:  &db.FirmwareChannel{Model: "t7", Version: "v2.1.034", SetBy: "tariq.jamil@aioapp.com", SetAt: now.AddDate(0, 0, -2)},
		Releases: []db.Release{{ID: 35, Version: "v2.1.035", PublishedAt: &pub}, {ID: 34, Version: "v2.1.034"}},
		Newer:    []db.Release{{ID: 35, Version: "v2.1.035", PublishedAt: &pub}},
		On:       131, Behind: 7, OtherLine: 22, Total: 160}
	cl := &ClientSlotView{Slot: "firmware-qcom", Label: "Firmware MDM", Hosted: true,
		Build:   config.AgentAPKBuild{Version: "1.13.10", VersionCode: 110},
		Devices: 160, UpToDate: 131, Behind: 23, Unknown: 6}
	return luInputs{
		RoleKey: "t7", RoleLabel: "Tableside AI", Models: []pipeModel{m},
		HW:      []hwModel{{Key: "t7", Display: "AIO T7", Known: true, CanFw: true, Devices: 160, Firmware: 154, FwMode: "prod", Runs: 4}},
		App:     lineupApp{Base: "aio.app.nugget", Name: "Nugget", On: 144, Total: 160, Builds: []lineupBuild{{Env: "Prod", EnvTone: "ok", Devices: 120, Pct: 83}, {Env: "UAT", EnvTone: "info", Devices: 24, Pct: 17}}},
		Clients: []*ClientSlotView{cl}, Full: 125, Deployed: 160,
		Moves:   []db.RoleMoveDay{{Day: now.AddDate(0, 0, -1), Firmware: 9, Client: 3}, {Day: now, Firmware: 4}},
		History: map[string][]db.FirmwareProdChange{"t7": {
			{Model: "t7", Version: "v2.1.030", By: "tariq.jamil@aioapp.com", At: now.AddDate(0, 0, -20)},
			{Model: "t7", Version: "v2.1.034", By: "tariq.jamil@aioapp.com", At: now.AddDate(0, 0, -2)},
		}},
		CanEdit: true, Now: now,
	}
}

func TestLineupPaneBuilders(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	p := buildLineupPane(luSample(now))

	b := p.Banner
	if b.Tone != "warn" || b.Headline != "3 things to decide" || len(b.Todos) != 3 {
		t.Fatalf("banner = %+v", b)
	}
	if b.Todos[0].Title != "v2.1.035 is newer than prod v2.1.034" || !b.Todos[0].Promote || b.Todos[0].ReleaseID != 35 {
		t.Errorf("first row should promote v2.1.035: %+v", b.Todos[0])
	}
	if !strings.HasPrefix(b.Todos[1].Title, "22 deployed devices on another firmware line") {
		t.Errorf("second row = %q", b.Todos[1].Title)
	}
	if b.Todos[2].Title != "23 behind Firmware MDM 1.13.10" || b.Todos[2].Href != "/clients?client=firmware-qcom" {
		t.Errorf("auto-update off and devices behind is a decision: %+v", b.Todos[2])
	}
	if b.Kicker != "Tableside AI · AIO T7 · 160 devices" {
		t.Errorf("kicker = %q", b.Kicker)
	}

	if c := p.Cards[0]; c.A.Value != "82%" || c.A.Label != "on prod v2.1.034" || c.A.Delta != "▲ 13 this week" || c.B.Value != "7" {
		t.Errorf("firmware card = %+v", c)
	}
	if c := p.Cards[1]; c.A.Value != "131" || c.B.Value != "Off" {
		t.Errorf("client card = %+v", c)
	}
	if c := p.Cards[2]; c.Title != "App · Nugget" || c.B.Value != "16" || len(c.Bar) != 2 {
		t.Errorf("app card = %+v", c)
	}
	if r := p.Rail; r.Pct != 78 || r.Tone != "warn" || r.MovedWeek != 16 || len(r.Bars) != 2 || len(r.Looks) != 3 {
		t.Errorf("rail = %+v", r)
	}

	tl := p.Timelines["t7"]
	if len(tl.Rows) != 3 {
		t.Fatalf("timeline rows = %+v", tl.Rows)
	}
	if tl.Rows[0].Name != "v2.1.030" || tl.Rows[0].Tone != "ok" || tl.Rows[1].Name != "v2.1.034" || tl.Rows[1].Tone != "run" ||
		tl.Rows[1].Count != "131 / 160" || tl.Rows[2].Name != "v2.1.035" || tl.Rows[2].Tone != "warn" {
		t.Errorf("timeline = %+v", tl.Rows)
	}
	if tl.Days[6] != "Today" {
		t.Errorf("axis = %v", tl.Days)
	}

	// A role with nothing to decide gets the calm banner.
	in := luSample(now)
	in.Models[0].Newer, in.Models[0].OtherLine = nil, 0
	in.Clients[0].Auto = config.ClientAutoUpdate{On: true, WindowStart: "02:00", WindowEnd: "05:00"}
	calm := buildLuBanner(in)
	if calm.Tone != "calm" || calm.Headline != "Tableside AI is on its lineup" ||
		calm.Calm != "Nothing to decide: prod firmware v2.1.034 · Firmware MDM 1.13.10 · Nugget." {
		t.Errorf("calm = %+v", calm)
	}
	// A viewer who cannot promote gets a link to the release instead of the form.
	in = luSample(now)
	in.CanEdit = false
	if t0 := buildLuBanner(in).Todos[0]; t0.Promote || t0.Href != "/releases/35" {
		t.Errorf("no promote without the right: %+v", t0)
	}
}

func TestLineupFullOn(t *testing.T) {
	rid := uuid.New()
	mk := func(build string, code int64, app bool) (db.Device, bool) {
		extra := `{"agent_version_code":` + strings.TrimSpace(func() string {
			if code == 0 {
				return "0"
			}
			return "110"
		}()) + `,"agent_package":"com.aioapp.mdm"}`
		return db.Device{ID: uuid.New(), SerialNumber: "AT070AABU" + build, BuildID: build, Product: product.KeyT7,
			DeviceClass: "t7", RestaurantID: &rid, AgentKind: product.KindFirmware, LatestExtra: []byte(extra)}, app
	}
	var devices []db.Device
	has := map[uuid.UUID]bool{}
	for _, c := range []struct {
		build string
		code  int64
		app   bool
	}{{"v2.1.034", 110, true}, {"v2.1.030", 110, true}, {"v2.0.96l", 110, true}, {"v2.1.034", 110, false}, {"v2.1.034", 0, true}} {
		d, app := mk(c.build, c.code, c.app)
		devices = append(devices, d)
		has[d.ID] = app
	}
	models := []pipeModel{{Key: "t7", Mode: "prod", Line: "2.1", Channel: &db.FirmwareChannel{Version: "v2.1.034"}}}
	views := map[string]*ClientSlotView{}
	for i := range devices {
		views[agentSlotFor(&devices[i])] = &ClientSlotView{Hosted: true, Build: config.AgentAPKBuild{VersionCode: 110}}
	}
	full, deployed := lineupFullOn(devices, "t7", models, views, "aio.app.nugget", has, func(*db.Device) bool { return false })
	// Only the first: the others are behind, on another line, without the app, or report no client.
	if deployed != 5 || full != 1 {
		t.Errorf("full = %d of %d, want 1 of 5", full, deployed)
	}
}

// TestLineupRolePaneRenders executes pipeline.html's role pane with the sample.
func TestLineupRolePaneRenders(t *testing.T) {
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
	funcs["canAdmin"] = func(role string) bool { return role == "admin" }
	funcs["canOperate"] = func(string) bool { return true }
	funcs["classShort"] = classShort
	funcs["classLabel"] = product.ClassLabel
	funcs["modelPic"] = modelPic
	funcs["pct"] = func(a, b int) int { return 0 }
	funcs["add"] = func(a, b int) int { return a + b }
	funcs["sub"] = func(a, b int) int { return a - b }
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/pipeline.html", "../../templates/clients.html"))

	now := time.Now()
	in := luSample(now)
	data := map[string]any{
		"Role": "admin", "CanEdit": true, "RoleKey": "t7", "RoleLabel": "Tableside AI", "HWDevices": 160,
		"Roles":   []pipeRole{{Key: "t7", Label: "Tableside AI", Status: "160 devices · 1 model", Tone: "ok", On: true}, {Key: "kds", Label: "Kitchen display", Status: "4 devices · 1 model", Tone: "ok"}},
		"Clients": []pipeClient{{Slot: "dpc", Label: "Standard client", Status: "all on 0.4.0", Tone: "ok"}},
		"NoneCount": 2, "RunCount": 4, "Tab": "products", "BehindTotal": 23,
		"ClientStrips": lineupClientStrips(in.Clients),
		"LU": buildLineupPane(in), "HWModels": in.HW, "Models": in.Models, "App": in.App,
		"FwRollout": config.ClientAutoUpdate{WindowStart: "02:00", WindowEnd: "05:00"},
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "pipeline.html", data); err != nil {
		t.Fatalf("pipeline.html failed to render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`class="lu1-chip on"`, `<span class="ic tone-ok">T7</span>`,
		// Lineup has Products and Setup; MDM clients and Host apps are pages (S1, 23 Oct).
		`aria-current="page">Products</a>`, `Setup <i>2</i>`,
		`<b>Firmware MDM 1.13.10</b><small>131 of 160 on it · 23 behind · Auto-update off for every product</small>`,
		`href="/clients?client=firmware-qcom">Manage in MDM clients →</a>`,
		`3 things to decide`, `Tableside AI · AIO T7 · 160 devices`,
		`action="/pipeline/firmware"`, `name="release_id" value="35"`, `>Promote to prod</button>`,
		`href="/clients?client=firmware-qcom">Open client</a>`,
		`82%</b><small>on prod v2.1.034</small>`, `▲ 13 this week`, `App · Nugget`,
		`class="lu1-m" id="model-t7"`, `Firmware: Prod`, `Firmware MDM 154`, `160 devices`,
		`id="fw-t7"`, `class="oc-tb run"`, `131 / 160`, `class="oc-tb warn"`, `href="/releases/35"`,
		`78%</b><small>fully on lineup</small>`, `125 of 160 deployed devices`, `Needs a look <span>3</span>`,
		`16 without Nugget`, `When devices move`, `02:00–05:00 in each device's timezone`,
		`href="/host-apps?app=t7">Set on Host apps →</a>`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("Lineup role pane is missing %q", want)
		}
	}
	if strings.Contains(out, `class="pl-side"`) {
		t.Error("the side list is back")
	}
	for _, gone := range []string{`Production runs</b>`, `Standard client</b>`, `Keep devices on it`, `Switch on</button>`} {
		if strings.Contains(out, gone) {
			t.Errorf("Products tab still shows %q (clients and setup have their own tabs)", gone)
		}
	}

	// The MDM clients tab: a card per client, the chosen one marked.
	views := map[string]*ClientSlotView{
		"firmware-qcom": in.Clients[0],
		"dpc":           {Slot: "dpc", Label: "Standard MDM", Hosted: true, Build: config.AgentAPKBuild{Version: "0.4.0", VersionCode: 24}, Devices: 12, UpToDate: 12, Auto: config.ClientAutoUpdate{On: true}},
	}
	dev := []db.Device{{ID: uuid.New(), DeviceClass: "kds", Product: "d2s", AgentKind: product.KindDPC}}
	cards := lineupClientCards(views, dev, "dpc")
	if len(cards) != 2 || cards[0].On || !cards[1].On || cards[1].Auto != "Auto-update on · any time" || len(cards[1].Products) != 1 {
		t.Fatalf("cards = %+v", cards)
	}
	sel := views["dpc"]
	data["Tab"], data["View"], data["ClientCards"], data["Client"], data["Selected"] = "clients", "clients", cards, sel, sel
	buf.Reset()
	if err := tmpl.ExecuteTemplate(&buf, "pipeline.html", data); err != nil {
		t.Fatalf("clients tab failed to render: %v", err)
	}
	out = buf.String()
	for _, want := range []string{`<h1 class="cc-h1">MDM clients</h1>`, `class="lu1-cl on"`, `0.4.0<span>hosted · code 24</span>`, `Auto-update on · any time`, `Kitchen display 1`, `MDM clients · Standard MDM`} {
		if !strings.Contains(out, want) {
			t.Errorf("clients tab is missing %q", want)
		}
	}
	if strings.Contains(out, `class="lu1-chips"`) {
		t.Error("the product chips belong to the Products tab only")
	}

	// Setup: models without a role, then the production runs.
	data["Tab"], data["View"], data["Client"], data["Selected"] = "setup", "setup", nil, nil
	data["None"] = []hwModel{{Key: "rk3566_box", Display: "RK3566 box", Devices: 3}}
	data["ClassKeys"] = product.Classes()
	buf.Reset()
	if err := tmpl.ExecuteTemplate(&buf, "pipeline.html", data); err != nil {
		t.Fatalf("setup tab failed to render: %v", err)
	}
	out = buf.String()
	for _, want := range []string{`aria-current="page">Setup`, `Not attached</h2>`, `rk3566_box`, `Production runs</h2>`, `Lineup · Setup`} {
		if !strings.Contains(out, want) {
			t.Errorf("setup tab is missing %q", want)
		}
	}
}
