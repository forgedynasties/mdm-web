package dashboard

import (
	"bytes"
	"fmt"
	"html/template"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"mdm/internal/db"
)

// d1Nugget is Nugget's prod page from a1Sample: 14 devices (13 on the library's 17.0.0, one
// on 16.9.0), and 10 devices without it, 6 of which run stagev2 and 1 uatv2.
func d1Nugget(now time.Time) d1Inputs {
	s := a1Sample(now)
	var devs []db.DeviceAppRow
	for i := 0; i < 13; i++ {
		venue := "Flights Vegas"
		if i%3 == 0 {
			venue = "Huanchaco Peruvian"
		}
		devs = append(devs, db.DeviceAppRow{Serial: fmt.Sprintf("AT070AABU%05d", i+100), Version: "17.0.0", Venue: venue})
	}
	devs = append(devs, db.DeviceAppRow{Serial: "AT070AABU00900", Version: "16.9.0", Venue: "Flights Vegas"})
	var missing []string
	stage, uat := map[string]bool{}, map[string]bool{}
	for i := 0; i < 10; i++ {
		sn := fmt.Sprintf("AT070AABU%05d", i+500)
		missing = append(missing, sn)
		if i < 6 {
			stage[sn] = true
		}
		if i == 6 {
			uat[sn] = true
		}
	}
	return d1Inputs{Package: "aio.app.nugget", Cov: s.Cov.Of("aio.app.nugget"), Fleet: 216, Apps: s.Apps, Families: s.Families,
		Siblings:   map[string]map[string]bool{"aio.app.nugget.stagev2": stage, "aio.app.nugget.uatv2": uat},
		SiblingCov: map[string]pkgCoverage{"aio.app.nugget.stagev2": s.Cov.Of("aio.app.nugget.stagev2"), "aio.app.nugget.uatv2": s.Cov.Of("aio.app.nugget.uatv2")},
		Devices:    devs, Missing: missing, OtherCount: 1, CanSend: true}
}

func TestAppD1Builder(t *testing.T) {
	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	v := buildAppD1(d1Nugget(now))

	if v.Name != "Nugget" || v.Variant != "prod" || v.FamilyName != "Nugget" || v.LibVersion != "17.0.0" || v.Devices != 14 {
		t.Fatalf("head = %+v", v)
	}
	b := v.Banner
	if b.Headline != "1 thing to look at" || len(b.Todos) != 1 {
		t.Fatalf("banner = %+v", b)
	}
	if td := b.Todos[0]; td.Title != "Reconcile 1 device on another version" || td.HxGet != "/apps/install-targets?pkg=aio.app.nugget" || !strings.Contains(td.Sub, "16.9.0 on 1") {
		t.Errorf("reconcile todo = %+v", td)
	}
	if v.OtherChannelCount != 7 || v.MissingCount != 10 {
		t.Errorf("install warning = %d of %d", v.OtherChannelCount, v.MissingCount)
	}
	if len(v.Channels) != 3 || !v.Channels[0].Current || v.Channels[0].Devices != 14 {
		t.Errorf("channels = %+v", v.Channels)
	}
	if len(v.LibVersions) != 1 || !v.LibVersions[0].Latest || v.LibVersions[0].Version != "17.0.0" {
		t.Errorf("library versions = %+v", v.LibVersions)
	}
	if len(v.Rows) != 14 || v.Rows[0].Version != "17.0.0" || len(v.VerOpts) != 2 || len(v.Venues) != 2 {
		t.Errorf("rows %d, ver opts %v, venues %v", len(v.Rows), v.VerOpts, v.Venues)
	}
	if c := v.Cards[0]; c.B.Value != "10" || c.B.Delta != "7 on other channels" || c.B.Tone != "bad" {
		t.Errorf("on devices card = %+v", c)
	}
	if c := v.Cards[1]; c.A.Value != "13" || c.A.Label != "on 17.0.0" || c.A.Delta != "of 14 reporting" {
		t.Errorf("version card = %+v", c)
	}
	if v.Ring.PctN != 92 || v.Ring.Tone != "" {
		t.Errorf("ring = %+v", v.Ring)
	}

	// Key Attestation: devices with no version get their own item; not-in-library too.
	s := a1Sample(now)
	ka := buildAppD1(d1Inputs{Package: "io.github.vvb2060.keyattestation", Cov: s.Cov.Of("io.github.vvb2060.keyattestation"), Fleet: 216, Apps: s.Apps, Families: s.Families})
	var titles []string
	for _, td := range ka.Banner.Todos {
		titles = append(titles, td.Title)
	}
	if strings.Join(titles, " | ") != "Reconcile 46 devices on another version | 94 devices report no version" || ka.Banner.Todos[0].HxGet != "" || ka.Banner.Todos[1].Ver != "none" {
		t.Errorf("KA todos = %+v", ka.Banner.Todos)
	}
	kiosk := buildAppD1(d1Inputs{Package: "aio.app.kiosk", Cov: s.Cov.Of("aio.app.kiosk"), Fleet: 216, Apps: s.Apps, Families: s.Families})
	if kiosk.InLibrary || kiosk.Banner.Todos[0].Title != "Not in the library" || !kiosk.Banner.Todos[0].AddApps || kiosk.Family != nil {
		t.Errorf("kiosk = %+v", kiosk.Banner)
	}
}

func TestAppD1Renders(t *testing.T) {
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
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/app_detail.html", "../../templates/settings.html"))

	now := time.Date(2026, 10, 21, 12, 0, 0, 0, time.UTC)
	in := d1Nugget(now)
	data := map[string]any{"Role": "admin", "App": buildAppD1(in), "Families": in.Families, "AppFamilyMode": "auto"}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "app_detail.html", data); err != nil {
		t.Fatalf("app_detail.html failed to render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`<h1 class="cc-h1">Nugget</h1>`, `<h2>1 thing to look at</h2>`, `>Reconcile 1 on another version</button>`,
		`hx-get="/apps/install-targets?pkg=aio.app.nugget" hx-target="#d1-targets"`, `<nav class="d1-tabs" aria-label="Channels">`, `href="/apps/aio.app.nugget.stagev2"><b>stagev2</b>`, `class="d1-tab on" href="/apps/aio.app.nugget" aria-current="page"><b>prod</b><small>17.0.0</small>`, `prod in the library <span>1</span>`, `prod on devices <span>14</span>`,
		`action="/setup/apps/`, `>APK</a>`, `data-d1-set="ver" data-val="16.9.0"`, `data-d1-set="venue" data-val="Huanchaco Peruvian"`,
		`class="d1-r" data-ver="17.0.0"`, `id="lib-add-modal"`, `id="lib-card"`, `name="next" value="/apps/aio.app.nugget"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("app page is missing %q", want)
		}
	}
	// No fleet-wide "Install on N devices" or channel-overlap warning on app pages (user,
	// 30 Oct 2026: "remove this from everywhere on apps").
	if strings.Contains(out, ">Install on ") || strings.Contains(out, "already run another") {
		t.Error("the Install on N devices button or its channel warning is back")
	}
	if p := os.Getenv("D1_RENDER_OUT"); p != "" {
		os.WriteFile(p, buf.Bytes(), 0o644)
	}
}

// A channel no device runs still shows its library build on its tab (30 Oct 2026: qa and
// poss read "not in library" because only installed packages had coverage).
func TestLibNewest(t *testing.T) {
	now := time.Date(2026, 10, 30, 0, 0, 0, 0, time.UTC)
	apps := []db.App{
		{PackageName: "aio.app.nugget.qa", VersionName: "55.0.0-qa", CreatedAt: now.AddDate(0, -1, 0)},
		{PackageName: "aio.app.nugget.qa", VersionName: "56.0.0-qa", CreatedAt: now},
		{PackageName: "aio.app.nugget.poss", CreatedAt: now},
	}
	if got := libNewest(apps, "aio.app.nugget.qa"); got != "56.0.0-qa" {
		t.Errorf("qa newest = %q", got)
	}
	if got := libNewest(apps, "aio.app.nugget.poss"); got != "unversioned" {
		t.Errorf("poss newest = %q", got)
	}
	if got := libNewest(apps, "aio.app.nugget.uatv2"); got != "" {
		t.Errorf("a package the library lacks = %q", got)
	}
}
