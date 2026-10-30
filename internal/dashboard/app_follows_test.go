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
)

func TestPickFollowInstalls(t *testing.T) {
	now := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	lib := newestBuilds([]db.App{
		{PackageName: "aio.app.kiosk.qa", VersionName: "1000-qa", ApkURL: "kq", CreatedAt: now},
		{PackageName: "aio.app.kiosk.qa", VersionName: "999-qa", ApkURL: "kq-old", CreatedAt: now.Add(-time.Hour)},
		{PackageName: "aio.app.pos.uatv2", VersionName: "985.0.0-uatv2", ApkURL: "pu", CreatedAt: now},
	}, map[string]bool{"aio.app.kiosk.qa": true, "aio.app.pos.uatv2": true, "aio.app.nugget.qa": true})
	if lib["aio.app.kiosk.qa"].ApkURL != "kq" || lib["aio.app.nugget.qa"] != nil {
		t.Fatalf("library picks = %+v", lib)
	}
	dev := func(sn string, seen time.Duration) db.Device {
		return db.Device{ID: uuid.New(), SerialNumber: sn, Product: "t7", AgentKind: "firmware", LastSeenAt: now.Add(-seen)}
	}
	devs := []db.Device{
		dev("MISSING", time.Minute),  // follows kiosk.qa, lacks it
		dev("OLDER", time.Minute),    // has 999-qa
		dev("CURRENT", time.Minute),  // has 1000-qa, also follows pos.uatv2 which it lacks
		dev("OFFLINE", time.Hour),    // lacks it, not online
		dev("BUSY", time.Minute),     // install on its way
		dev("NOLIST", time.Minute),   // never reported its apps
		dev("SENT", time.Minute),     // was sent kq lately
		dev("NOBUILD", time.Minute),  // follows a package with no build
		dev("NEWER", time.Minute),    // runs a newer build than the library's
		dev("STRANGER", time.Minute), // follows nothing
	}
	follows := map[uuid.UUID][]string{}
	for _, d := range devs[:9] {
		follows[d.ID] = []string{"aio.app.kiosk.qa"}
	}
	follows[devs[2].ID] = []string{"aio.app.kiosk.qa", "aio.app.pos.uatv2"}
	follows[devs[7].ID] = []string{"aio.app.nugget.qa"}
	have := map[uuid.UUID]map[string]string{
		devs[1].ID: {"aio.app.kiosk.qa": "999-qa"},
		devs[2].ID: {"aio.app.kiosk.qa": "1000-qa"},
		devs[8].ID: {"aio.app.kiosk.qa": "1001-qa"},
	}
	reported := map[uuid.UUID]bool{}
	for i, d := range devs {
		if i != 5 {
			reported[d.ID] = true
		}
	}
	p := followPick{follows: follows, lib: lib, have: have, reported: reported,
		skip:   map[uuid.UUID]bool{devs[4].ID: true},
		recent: map[uuid.UUID]map[string]bool{devs[6].ID: {"kq": true}}}
	got := pickFollowInstalls(devs, p, 3*time.Minute, now)
	var names []string
	for _, j := range got {
		names = append(names, j.Device.SerialNumber+":"+j.App.ApkURL)
	}
	if strings.Join(names, " ") != "MISSING:kq OLDER:kq CURRENT:pu" {
		t.Fatalf("picked %v", names)
	}
	if got[0].Upgrade || !got[1].Upgrade {
		t.Errorf("upgrade flags %+v %+v", got[0], got[1])
	}
	p.batch = 1
	if n := len(pickFollowInstalls(devs, p, 3*time.Minute, now)); n != 1 {
		t.Errorf("batch 1 picked %d", n)
	}
}

func TestKeyClashDecisions(t *testing.T) {
	if keyClashAction(false, "aio.app.kiosk.qa", true) != "uninstall" {
		t.Error("a Lab device replaces the app")
	}
	for _, c := range []struct {
		venue bool
		pkg   string
		un    bool
	}{{true, "aio.app.kiosk", true}, {false, "", true}, {false, "aio.app.kiosk.qa", false}} {
		if keyClashAction(c.venue, c.pkg, c.un) != "gave_up" {
			t.Errorf("keyClashAction(%v) should give up", c)
		}
	}
	if keyClashNext("completed", time.Minute) != "install" || keyClashNext("", time.Minute) != "" ||
		keyClashNext("", 3*time.Hour) != "gave_up" || keyClashNext("failed", time.Minute) != "gave_up" {
		t.Error("keyClashNext")
	}
}

func TestMayUploadPackage(t *testing.T) {
	prod := map[string]bool{"aio.app.kiosk": true, "aio.app.nugget.uatv2": true}
	if mayUploadPackage("engineer", "aio.app.kiosk", prod) || mayUploadPackage("engineer", "aio.app.nugget.uatv2", prod) {
		t.Error("an Engineer must not add a build the prod auto-install sends")
	}
	if !mayUploadPackage("engineer", "aio.app.kiosk.qa", prod) || !mayUploadPackage("fleet_manager", "aio.app.kiosk", prod) ||
		!mayUploadPackage("admin", "aio.app.kiosk", prod) {
		t.Error("channel builds for Engineers, everything for Ops lead and super admin")
	}
	if !strings.Contains(prodUploadRefusal("aio.app.kiosk"), "Ops lead") {
		t.Error("refusal names who may")
	}
}

func TestInstallChannelsAndFollowState(t *testing.T) {
	f := LibFamily{ID: "f", Name: "KIOSK", Variants: []LibVariant{
		{Label: "qa", Package: "aio.app.kiosk.qa", Versions: []LibVersion{{App: db.App{VersionName: "1000-qa", ApkURL: "kq"}}}},
		{Label: "prod", Package: "aio.app.kiosk", Versions: []LibVersion{{App: db.App{VersionName: "55.0.0", ApkURL: "kp"}}, {App: db.App{VersionName: "54.0.0", ApkURL: "kp54"}}}},
		{Label: "uatv2", Package: "aio.app.kiosk.uatv2", Versions: []LibVersion{{App: db.App{VersionName: "x"}}}}, // no APK
	}}
	var chs []installChannel
	if err := json.Unmarshal([]byte(installChannelsJSON(f)), &chs); err != nil {
		t.Fatal(err)
	}
	if len(chs) != 2 || chs[0].Label != "prod" || len(chs[0].URLs) != 2 || chs[1].Package != "aio.app.kiosk.qa" {
		t.Fatalf("channels = %+v", chs)
	}
	v := followView{Channel: "qa", Lib: "1000-qa", Have: "1000-qa"}
	if s, _ := followState(v, true, db.InstallProgress{}); s != "ok" {
		t.Error("current = ok")
	}
	if s, _ := followState(v, true, db.InstallProgress{Status: "downloading"}); s != "run" {
		t.Error("pending = run")
	}
	v.Have = "999-qa"
	if s, _ := followState(v, true, db.InstallProgress{}); s != "wait" {
		t.Error("older = wait")
	}
	if s, _ := followState(followView{Lib: ""}, false, db.InstallProgress{}); s != "wait" {
		t.Error("no build = wait")
	}
}

// The Apps card's Follows strip, the drop zone and a followed row's chip render.
func TestFollowsRender(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	funcs["canOperate"] = func(string) bool { return true }
	funcs["canAppLibrary"] = func(string) bool { return true }
	funcs["supports"] = func(*db.Device, string) bool { return true }
	funcs["iconSrc"] = func(string) string { return "/icon/x" }
	tm := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}`))
	tm = template.Must(tm.ParseFiles("../../templates/device.html"))
	fs := []followView{{Package: "aio.app.kiosk.qa", Name: "KIOSK", Channel: "qa", Icon: "i", State: "ok", Pct: -1, Title: "Up to date"},
		{Package: "aio.app.pos.uatv2", Name: "POS", Channel: "uatv2", State: "run", Pct: 42}}
	d := &db.Device{SerialNumber: "R95Y603F27X"}
	var buf bytes.Buffer
	if err := tm.ExecuteTemplate(&buf, "dd-follows", map[string]any{"Follows": fs, "Device": d, "Role": "engineer"}); err != nil {
		t.Fatal(err)
	}
	out := buf.String()
	for _, want := range []string{`class="dd-follows"`, `KIOSK <b>qa</b><i class="dd-fdot ok">`, `<small>42 %</small>`,
		`hx-post="/devices/R95Y603F27X/follows"`, `"action":"unfollow"`, `onclick="followModalOpen()"`, `data-apk-drop`} {
		if !strings.Contains(out, want) {
			t.Errorf("follows strip is missing %q in %s", want, out)
		}
	}
	buf.Reset()
	if err := tm.ExecuteTemplate(&buf, "dd-follows", map[string]any{"Device": d, "Role": "engineer"}); err != nil {
		t.Fatal(err)
	}
	// Demo C (30 Oct 2026): the strip is always there for someone who may install, so a
	// device that follows nothing still offers "+ Follow an app".
	if out := buf.String(); !strings.Contains(out, "nothing yet") || !strings.Contains(out, "+ Follow an app") {
		t.Errorf("empty strip: %s", out)
	}
	buf.Reset()
	row := map[string]any{"R": struct {
		PackageName, AppName, VersionName, Icon, Mem, CPU, Screen string
	}{PackageName: "aio.app.kiosk.qa", AppName: "KIOSK"}, "Root": map[string]any{
		"Uninstalling": map[string]bool{}, "NuggetIDByPackage": map[string]string{}, "AppList": struct{ Limited bool }{true},
		"FollowSet": followSet(fs), "Role": "engineer", "Device": d}, "Sys": false}
	if err := tm.ExecuteTemplate(&buf, "dd-app-row", row); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `<span class="dd-fchip ok" title="Up to date">follows qa</span>`) {
		t.Errorf("row chip missing: %s", buf.String())
	}
	buf.Reset()
	row["R"] = struct {
		PackageName, AppName, VersionName, Icon, Mem, CPU, Screen string
	}{PackageName: "com.android.chrome", AppName: "Chrome"}
	if err := tm.ExecuteTemplate(&buf, "dd-app-row", row); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(buf.String(), "dd-fchip") {
		t.Error("an app nobody follows has no chip")
	}
}
