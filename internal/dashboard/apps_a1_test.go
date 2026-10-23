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

// a1Sample is the library on 21 Oct 2026 in miniature: Key Attestation with devices on
// older builds, Nugget in three channels (stagev2 on no library build), Speedtest, an app
// never installed, and two role apps (kiosk, KDS) with no APK here.
func a1Sample(now time.Time) a1Inputs {
	app := func(name, pkg, ver string) db.App {
		return db.App{ID: uuid.New(), Name: name, PackageName: pkg, VersionName: ver, ApkURL: "https://x/" + pkg + ".apk", CreatedAt: now.AddDate(0, 0, -40)}
	}
	apps := []db.App{
		app("Key Attestation", "io.github.vvb2060.keyattestation", "1.8.4"),
		app("Nugget", "aio.app.nugget", "17.0.0"),
		app("Nugget", "aio.app.nugget.stagev2", "18.0.0-stagev2"),
		app("Nugget", "aio.app.nugget.uatv2", "17.1.0-uatv2"),
		app("Speedtest", "org.zwanoo.android.speedtest", "7.0.0"),
		app("STT Four Way", "com.aioapp.sttfourway", "1.0"),
	}
	fams, _ := buildLibrary(apps, nil, "auto")
	installs := []db.PackageInstall{
		{PackageName: "io.github.vvb2060.keyattestation", Version: "1.8.4", Devices: 25},
		{PackageName: "io.github.vvb2060.keyattestation", Version: "1.8.2", Devices: 46},
		{PackageName: "io.github.vvb2060.keyattestation", Version: "", Devices: 94},
		{PackageName: "aio.app.nugget", Version: "17.0.0", Devices: 13},
		{PackageName: "aio.app.nugget", Version: "16.9.0", Devices: 1},
		{PackageName: "aio.app.nugget.stagev2", Version: "17.9.0-stagev2", Devices: 23},
		{PackageName: "aio.app.nugget.stagev2", Version: "", Devices: 3},
		{PackageName: "aio.app.nugget.uatv2", Version: "17.1.0-uatv2", Devices: 5},
		{PackageName: "org.zwanoo.android.speedtest", Version: "7.0.0", Devices: 12},
		{PackageName: "aio.app.kiosk", Version: "3.0.0", Devices: 3},
		{PackageName: "aio.app.kiosk.uatv2", Version: "3.1.0", Devices: 2},
		{PackageName: "aio.app.someother", Version: "1.0", Devices: 4},
	}
	cov := buildCoverage(installs, apps, db.FleetAppTotals{Devices: 216, ReportingApps: 200})
	var days []db.InstallDay
	for i := 13; i >= 0; i-- {
		d := db.InstallDay{Day: now.AddDate(0, 0, -i).Truncate(24 * time.Hour)}
		if i == 0 {
			d.Sent, d.Failed = 3, 1
		}
		if i == 2 {
			d.Sent = 2
		}
		if i == 9 {
			d.Sent = 1
		}
		days = append(days, d)
	}
	return a1Inputs{Families: fams, Apps: apps, Cov: &cov, Installs: days, AddedLast: 2, Now: now,
		RoleApps: []db.RoleApp{{Role: "t7", BasePackage: "aio.app.nugget"}, {Role: "kiosk", BasePackage: "aio.app.kiosk"},
			{Role: "kds", BasePackage: "aio.app.kds"}, {Role: "dongle", BasePackage: ""}}}
}

func TestAppsA1Builder(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	v := buildAppsA1(a1Sample(now))

	b := v.Banner
	if b.Headline != "4 things to look at" || len(b.Todos) != 3 || b.More != 1 {
		t.Fatalf("banner = %+v", b)
	}
	if td := b.Todos[0]; td.Title != "2 role apps have no APK here" || !td.AddApps || !strings.HasPrefix(td.Sub, "Self-order kiosk and Kitchen display") {
		t.Errorf("todo 0 = %+v", td)
	}
	if td := b.Todos[1]; td.Title != "Key Attestation: 46 devices on another version" || !strings.Contains(td.Sub, "1.8.4; 94 more report no version") || td.Href != "/apps/io.github.vvb2060.keyattestation" || td.Action != "Reconcile" {
		t.Errorf("todo 1 = %+v", td)
	}
	if td := b.Todos[2]; td.Title != "Nugget: 1 device on another version" {
		t.Errorf("todo 2 = %+v", td)
	}
	if !strings.Contains(b.Kicker, "% of installs on the library version") {
		t.Errorf("kicker = %q", b.Kicker)
	}

	byName := map[string]a1Row{}
	for _, r := range v.Rows {
		byName[r.Name] = r
	}
	if v.Rows[0].Name != "Key Attestation" {
		t.Errorf("first row = %q, want the most installed", v.Rows[0].Name)
	}
	ka := byName["Key Attestation"]
	if ka.Devices != 165 || ka.OnLib != 25 || ka.Other != 46 || ka.Unknown != 94 || ka.Note != "25 on 1.8.4 · 46 other · 94 no version" || ka.Single == "" || !ka.Look {
		t.Errorf("Key Attestation = %+v", ka)
	}
	ng := byName["Nugget"]
	if ng.Role != "t7" || ng.LibVersion != "17.0.0" || ng.Devices != 45 || ng.Single != "" || len(ng.Channels) != 3 || ng.Channels[0].Label != "stagev2" {
		t.Errorf("Nugget = %+v", ng)
	}
	if ng.Note != "18 on library builds · 24 other · 3 no version" {
		t.Errorf("Nugget note = %q", ng.Note)
	}
	if st := byName["STT Four Way"]; st.Devices != 0 || st.Note != "on no device" {
		t.Errorf("STT = %+v", st)
	}
	kiosk := byName["Self-order kiosk app"]
	if kiosk.InLibrary || kiosk.Devices != 5 || kiosk.Note != "incl. uatv2" || kiosk.Href != "/apps/aio.app.kiosk" {
		t.Errorf("kiosk row = %+v", kiosk)
	}
	if v.Hidden != 1 {
		t.Errorf("hidden = %d, want aio.app.someother", v.Hidden)
	}

	if c := v.Cards[0]; c.A.Value != "4" || c.A.Label != "apps · 6 APKs" || c.A.Delta != "▲ 2 this month" || c.B.Value != "1" {
		t.Errorf("library card = %+v", c)
	}
	if c := v.Cards[1]; c.A.Value != "5" || c.A.Delta != "▲ 4 vs last week" || c.B.Value != "1" || c.B.Tone != "bad" || c.LineA == "" {
		t.Errorf("installs card = %+v", c)
	}
	// On library: KA 25 + Nugget 13 + uatv2 5 + Speedtest 12 = 55; known 55 + 46 + 1 + 23 = 125.
	if c := v.Cards[2]; c.A.Value != "55" || c.A.Delta != "of 125 reporting" || c.B.Value != "97" {
		t.Errorf("versions card = %+v", c)
	}
	rl := v.Rail
	if rl.PctN != 44 || rl.Tone != "danger" || len(rl.Bars) != 7 || !rl.Bars[6].Cur || rl.RolesMissing != 2 || len(rl.Roles) != 3 {
		t.Errorf("rail = %+v", rl)
	}
	if rl.Roles[2].Label != "Tableside AI" || rl.Roles[2].Version != "17.0.0" || !rl.Roles[0].Missing {
		t.Errorf("roles = %+v", rl.Roles)
	}
}

func TestAppsA1Calm(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	apps := []db.App{{ID: uuid.New(), Name: "Speedtest", PackageName: "org.zwanoo.android.speedtest", VersionName: "7.0.0"}}
	fams, _ := buildLibrary(apps, nil, "auto")
	cov := buildCoverage([]db.PackageInstall{{PackageName: "org.zwanoo.android.speedtest", Version: "7.0.0", Devices: 9}}, apps, db.FleetAppTotals{Devices: 9})
	v := buildAppsA1(a1Inputs{Families: fams, Apps: apps, Cov: &cov, Now: now})
	if v.Banner.Tone != "calm" || len(v.Banner.Todos) != 0 || v.Rail.PctN != 100 || v.Rail.Tone != "" {
		t.Errorf("calm = %+v / %+v", v.Banner, v.Rail)
	}
}

func TestAppsA1Renders(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	for _, k := range []string{"canAppLibrary", "canOperate", "canAdmin"} {
		funcs[k] = func(string) bool { return true }
	}
	funcs["iconSrc"] = func(s string) string { return s }
	funcs["add"] = func(a, b int) int { return a + b }
	funcs["sub"] = func(a, b int) int { return a - b }
	funcs["pct"] = func(a, b int) int {
		if b == 0 {
			return 0
		}
		return a * 100 / b
	}
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/apps.html", "../../templates/settings.html", "../../templates/_apps_hub.html"))

	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	in := a1Sample(now)
	data := map[string]any{
		"Role": "admin", "Families": in.Families, "Apps": in.Apps, "Coverage": in.Cov, "AppFamilyMode": "suggest",
		"Suggestions": []LibSuggestion{{Name: "Hardware Testing", BasePackage: "com.example.hw", Packages: []string{"com.example.hw", "com.example.hw.new"},
			Variants: map[string]string{"com.example.hw": "prod", "com.example.hw.new": "new"}}},
		"A1": buildAppsA1(in),
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "apps.html", data); err != nil {
		t.Fatalf("apps.html failed to render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`<h1 class="cc-h1">Apps</h1>`, `<h2>4 things to look at</h2>`, `onclick="libAddOpen()">Add apps</button>`,
		`>Key Attestation: 46 devices on another version`, `class="a1-r"`, `data-href="/apps/aio.app.nugget"`, `href="/apps/aio.app.nugget">Install on…</a>`, `hx-get="/apps/install-targets?pkg=io.github.vvb2060.keyattestation"`,
		`<span class="a1-ver none">not in library</span>`, `>Add APK</button>`, `class="a1-role"`, `stagev2 26`,
		`data-a1-set="show" data-val="look"`, `id="lib-card"`, `id="lib-add-modal"`, `id="lib-apk-file"`,
		`/apps/families`, `>no APK</span>`, `installs sent per day`, `1 more package on devices, not in the library`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("apps page is missing %q", want)
		}
	}
	if strings.Contains(out, `lf-modal`) {
		t.Error("the family dialogs are back on the list (their tools live on the app page)")
	}
	if strings.Contains(out, `class="lib-drawer"`) {
		t.Error("the tile grid is still on the Apps list")
	}
	if p := os.Getenv("A1_RENDER_OUT"); p != "" {
		os.WriteFile(p, buf.Bytes(), 0o644)
	}
}
