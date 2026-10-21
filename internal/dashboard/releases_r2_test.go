package dashboard

import (
	"bytes"
	"html/template"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"mdm/internal/db"
)

// r2Sample is v2.1.035: published, still dev, on 3 devices, one update running with a
// failed device, a full package and an incremental, crashing more than the day before.
func r2Sample() r2Inputs {
	id := 35
	rel := &db.Release{ID: id, Version: "v2.1.035", Product: "t7", Status: "published", IsDev: true}
	at := time.Date(2026, 10, 20, 9, 0, 0, 0, time.UTC)
	row := versionRow{Version: "v2.1.035", ReleaseID: &id, Product: "t7", Status: "published", Tracked: true, IsDev: true,
		ReleasedAt: &at, DeviceCount: 3, Changelog: "Staff mode pill on the guest screen."}
	return r2Inputs{
		Row: row, Rel: rel, ProductLabel: "AIO T7", FleetTotal: 188, InstallsWeek: 3, ShowCrashes: true, CanRelease: true, CanOTA: true,
		Packages: []db.OTAPackage{{ID: 1, Type: "full", Status: "active", PayloadSize: 2 << 30, CreatedAt: at},
			{ID: 2, Type: "incremental", SourceBuildID: "v2.1.034", Status: "active", PayloadSize: 212 << 20, CreatedAt: at}},
		Rollouts: []rolloutView{{URL: "/releases/35/deployments/401", ID: 401, Version: "v2.1.035", Status: "active",
			CreatedAt: at, CreatedBy: "ali", Total: 12, Installed: 3, Installing: 2, Failed: 1}},
		Crashes: db.BuildCrashCounts{Day: 9, PrevDay: 2, Week: 14, DayDevices: 2, WeekDevices: 3},
	}
}

func TestReleaseR2Builders(t *testing.T) {
	p := buildReleaseR2(r2Sample())
	b := p.Banner
	if b.Tone != "bad" || b.Headline != "3 things for this release" || len(b.Todos) != 3 {
		t.Fatalf("banner = %+v", b)
	}
	if b.Kicker != "v2.1.035 · AIO T7 · published 20 Oct · dev" {
		t.Errorf("kicker = %q", b.Kicker)
	}
	want := []string{"1 device failed in update #401", "9 crashes on this build in 24 h", "Still a dev release"}
	for i, w := range want {
		if b.Todos[i].Title != w {
			t.Errorf("row %d = %q, want %q", i, b.Todos[i].Title, w)
		}
	}
	if d := b.Todos[2]; d.Post != "/releases/35/dev" || d.Field != "dev" || d.Value != "0" || d.Confirm == "" {
		t.Errorf("release to everyone posts dev=0 with a confirm: %+v", d)
	}
	if on := p.Cards[0]; on.A.Value != "3" || on.A.Delta != "▲ 3 this week" || on.B.Value != "8" {
		t.Errorf("on devices = %+v", on)
	}
	if up := p.Cards[1]; up.Title != "Update #401" || up.A.Value != "3/12" || up.B.Value != "1" || up.B.Tone != "bad" {
		t.Errorf("update card = %+v", up)
	}
	if cr := p.Cards[2]; cr.A.Value != "9" || cr.A.Delta != "▲ 7" || cr.A.Tone != "bad" || cr.B.Value != "14" {
		t.Errorf("crash card = %+v", cr)
	}
	if len(p.Packages) != 2 || !p.Packages[0].Full || p.Packages[0].Size != "2.0 GB" || p.Packages[1].From != "v2.1.034" || p.Packages[1].Size != "212 MB" {
		t.Errorf("packages = %+v", p.Packages)
	}

	// A draft with its full package in offers Publish; a viewer who can't release doesn't get it.
	in := r2Sample()
	in.Row.Status, in.Rel.Status, in.Row.IsDev, in.Rollouts, in.Crashes = "draft", "draft", false, nil, db.BuildCrashCounts{}
	d := buildR2Banner(in)
	if len(d.Todos) != 1 || d.Todos[0].Action != "Publish" || d.Todos[0].Post != "/releases/35/publish" {
		t.Errorf("draft banner = %+v", d)
	}
	in.CanRelease = false
	if c := buildR2Banner(in); c.Tone != "calm" || c.Headline != "Nothing waiting on this release" || c.Calm != "On 3 devices." {
		t.Errorf("calm banner = %+v", c)
	}
	// Someone who sees only part of the fleet gets no crash numbers.
	in = r2Sample()
	in.ShowCrashes = false
	p = buildReleaseR2(in)
	if p.Cards[2].Empty == "" || p.Cards[2].Href != "" {
		t.Errorf("scoped crash card = %+v", p.Cards[2])
	}
	for _, td := range p.Banner.Todos {
		if strings.Contains(td.Title, "crashes") {
			t.Error("a scoped viewer gets a crash row")
		}
	}
}

func TestReleasesR2Renders(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	funcs["add"] = func(a, b int) int { return a + b }
	funcs["canRelease"] = func(role string) bool { return role == "admin" }
	funcs["canOTA"] = func(string) bool { return true }
	funcs["canSeeDev"] = func(string) bool { return true }
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/releases.html"))

	in := r2Sample()
	row := in.Row
	data := map[string]any{
		"Role": "admin", "Versions": []versionRow{row}, "FleetTotal": 188, "Sel": &row, "Rel": in.Rel,
		"Packages": in.Packages, "Rollouts": in.Rollouts, "Active": in.Rollouts, "CanPush": true, "HasFull": true,
		"RolloutCounts": map[string]rolloutCount{"v2.1.035": {N: 1, Active: 1}},
		"R2":            buildReleaseR2(in),
	}
	var buf bytes.Buffer
	if err := tmpl.ExecuteTemplate(&buf, "releases.html", data); err != nil {
		t.Fatalf("releases.html failed to render: %v", err)
	}
	out := buf.String()
	for _, want := range []string{
		`class="ov2-ban is-bad r2-ban"`, `3 things for this release`, `v2.1.035 · AIO T7 · published 20 Oct · dev`,
		`href="/releases/35/deployments/401">Open update</a>`, `href="/releases/35/crashes">See crashes</a>`,
		`action="/releases/35/dev"`, `name="dev" value="0"`, `name="redirect" value="/releases?release=35"`, `>Release to everyone</button>`,
		`>On devices</a>`, `▲ 3 this week`, `>Update #401</a>`, `3/12</b><small>installed</small>`, `>Crashes</a>`, `▲ 7`,
		`Packages <span>2</span>`, `2.0 GB`, `from <span class="mono">v2.1.034</span>`, `Staff mode pill on the guest screen.`,
		`Updates <span>1</span>`, `href="/releases/35/deployments/401">#401</a>`,
		// The header's own actions stay.
		`title="Only super admins can see this release now"`, `Show devices`, `href="/updates/new?release=35"`, `title="Open the release page"`,
	} {
		if !strings.Contains(out, want) {
			t.Errorf("releases page is missing %q", want)
		}
	}
	if strings.Contains(out, `<span class="k">Packages</span>`) {
		t.Error("the old key/value Packages row is back")
	}
}
