package dashboard

import (
	"bytes"
	"encoding/json"
	"html/template"
	"os"
	"regexp"
	"strings"
	"testing"

	"mdm/internal/db"
)

func TestNuggetAndroidIDs(t *testing.T) {
	x := json.RawMessage(`{"agent_version":"1.8.6","app_android_ids":[
		{"package":"aio.app.nugget.qa","uid":10170,"android_id":"bbbb"},
		{"package":"aio.app.nugget","uid":10169,"android_id":"36218dc1d56c00b7"},
		{"package":"com.other.app","android_id":"zzzz"},
		{"package":"aio.app.nugget.internal","android_id":"  "}]}`)
	got := nuggetAndroidIDs(x)
	if len(got) != 2 || got[0].Package != "aio.app.nugget" || got[0].AndroidID != "36218dc1d56c00b7" || got[1].Package != "aio.app.nugget.qa" {
		t.Fatalf("got %+v", got)
	}
	if m := nuggetIDByPackage(x); m["aio.app.nugget"] != "36218dc1d56c00b7" || len(m) != 2 {
		t.Errorf("by package: %+v", m)
	}
	for _, empty := range []json.RawMessage{nil, json.RawMessage(`{}`), json.RawMessage(`not json`)} {
		if len(nuggetAndroidIDs(empty)) != 0 {
			t.Errorf("%s should give none", empty)
		}
	}
}

// The Apps tab shows the Android ID on the Nugget tile (and only there), and the tile carries
// it for the app-info panel. Rendered with stubbed helpers, like the other page render tests.
func TestAppsListShowsNuggetAndroidID(t *testing.T) {
	src, err := os.ReadFile("handlers.go")
	if err != nil {
		t.Fatal(err)
	}
	funcs := template.FuncMap{}
	for _, m := range regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`).FindAllStringSubmatch(string(src), -1) {
		funcs[m[1]] = func(...any) any { return "" }
	}
	funcs["canOperate"] = func(string) bool { return false }
	tmpl, err := template.New("").Funcs(funcs).ParseFiles("../../templates/device.html")
	if err != nil {
		t.Fatal(err)
	}
	data := map[string]any{
		"Device": db.Device{SerialNumber: "AT070AABU00231"},
		"Role":   "admin",
		"InstalledPackages": []db.DevicePackage{
			{PackageName: "aio.app.nugget", AppName: "Nugget", VersionName: "17.0.0"},
			{PackageName: "aio.app.kds", AppName: "Kitchen Display", VersionName: "3.2.0"},
		},
		"Uninstalling":      map[string]bool{},
		"NuggetIDByPackage": nuggetIDByPackage(json.RawMessage(`{"app_android_ids":[{"package":"aio.app.nugget","android_id":"36218dc1d56c00b7"}]}`)),
	}
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, "device-apps-list", data); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	if !strings.Contains(out, `data-aid="36218dc1d56c00b7"`) || !strings.Contains(out, ">ID 36218dc1d56c00b7<") {
		t.Errorf("Nugget tile lacks its Android ID:\n%s", out)
	}
	if strings.Count(out, "data-aid=") != 1 {
		t.Errorf("only the Nugget tile should carry an Android ID, got %d", strings.Count(out, "data-aid="))
	}
}
