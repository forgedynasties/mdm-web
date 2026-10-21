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

	"mdm/internal/db"
)

func TestBriViewStates(t *testing.T) {
	// Tableside AI with no level of its own: 122, locked, default; device on it.
	cfg := &db.DeviceConfig{DeviceClass: "t7"}
	v := briViewFor(cfg, json.RawMessage(`{"screen_brightness":122,"screen_brightness_locked":true}`))
	if v.Level != 122 || !v.Locked || !v.Default || v.Drift || !v.Reports || v.Pct != 100 || v.Max != 122 {
		t.Errorf("default = %+v", v)
	}
	// Drifted: the device reports 59, unlocked.
	v = briViewFor(cfg, json.RawMessage(`{"screen_brightness":59,"screen_brightness_locked":false}`))
	if !v.Drift || v.Reported != 59 {
		t.Errorf("drift = %+v", v)
	}
	// Set for this device by ali.
	lvl, at := 80, time.Now().Add(-2*time.Hour)
	cfg = &db.DeviceConfig{DeviceClass: "t7", ScreenBrightness: &lvl, ScreenBrightnessLocked: true, ScreenBrightnessSetBy: "ali", ScreenBrightnessSetAt: &at}
	v = briViewFor(cfg, nil)
	if v.Level != 80 || v.Default || v.SetBy != "ali" || v.SetAgo != "2h ago" || v.Reports {
		t.Errorf("own level = %+v", v)
	}
	// A kiosk on the default is not managed.
	if v = briViewFor(&db.DeviceConfig{DeviceClass: "kiosk"}, nil); v.Level != 0 || v.DefaultApplies || v.Max != 255 {
		t.Errorf("kiosk = %+v", v)
	}
	// Free (not locked) never drifts.
	cfg = &db.DeviceConfig{DeviceClass: "t7", ScreenBrightness: &lvl}
	if v = briViewFor(cfg, json.RawMessage(`{"screen_brightness":30,"screen_brightness_locked":false}`)); v.Drift {
		t.Errorf("free level drifted: %+v", v)
	}
}

func TestBrightnessRowRenders(t *testing.T) {
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
	funcs["canAdminOrOperator"] = func(r string) bool { return r == "admin" || r == "operator" }
	tmpl := template.Must(template.New("").Funcs(funcs).Parse(`{{define "header"}}{{end}}{{define "footer"}}{{end}}`))
	tmpl = template.Must(tmpl.ParseFiles("../../templates/device.html"))
	render := func(b briView, role string) string {
		var buf bytes.Buffer
		if err := tmpl.ExecuteTemplate(&buf, "dev-brightness", map[string]any{"B": b, "Serial": "AT070AABU00044", "Role": role}); err != nil {
			t.Fatal(err)
		}
		return buf.String()
	}
	out := render(briView{Level: 122, Locked: true, Default: true, DefaultApplies: true, Max: 122, Pct: 100, Reports: true, Reported: 122, ReportedLocked: true}, "operator")
	for _, want := range []string{`class="bri-chip lock"`, `122 · Locked`, `Default for every Tableside AI tablet · device at 122`,
		`class="bri-pen"`, `<form class="bri-edit" hidden`, `max="122" value="122"`, `data-lock="1" aria-pressed="true"`, `Save for this device`} {
		if !strings.Contains(out, want) {
			t.Errorf("default row is missing %q", want)
		}
	}
	if strings.Contains(out, "Back to default") {
		t.Error("a device on the default has no Back to default")
	}
	out = render(briView{Level: 80, Locked: false, DefaultApplies: true, Max: 122, Pct: 65, SetBy: "ali", SetAgo: "2h ago"}, "admin")
	for _, want := range []string{`class="bri-chip custom"`, `80 · Free`, `Set for this device by ali, 2h ago · applies from client 1.14.0`,
		`value="80"`, `data-lock="0" aria-pressed="true"`, `Back to default (122)`} {
		if !strings.Contains(out, want) {
			t.Errorf("own-level row is missing %q", want)
		}
	}
	out = render(briView{Level: 122, Locked: true, Default: true, DefaultApplies: true, Max: 122, Reports: true, Reported: 59, Drift: true, SentAgo: "4m ago"}, "viewer")
	for _, want := range []string{`class="bri-chip drift"`, `device at 59, unlocked · putting it back (last sent 4m ago)`, `bri-pen role-locked`} {
		if !strings.Contains(out, want) {
			t.Errorf("drift row (viewer) is missing %q", want)
		}
	}
	if strings.Contains(out, `class="bri-edit"`) {
		t.Error("a viewer gets no editor")
	}
}
