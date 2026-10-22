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
	"mdm/internal/product"
)

func TestWifiOpWords(t *testing.T) {
	now := time.Now()
	run := wifiOpOf(&db.WifiOp{Action: "connect", SSID: "AIO-Kitchen", Status: "running", CreatedAt: now.Add(-24 * time.Second)}, now)
	if !run.Running || !strings.Contains(run.Detail, "0:24 of 1:30") {
		t.Errorf("running = %+v", run)
	}
	ok := wifiOpOf(&db.WifiOp{Action: "connect", SSID: "AIO-Kitchen", Status: "completed", Output: `{"ok":true,"after_s":18}`}, now)
	if !ok.Ok || ok.Title != "On AIO-Kitchen" || !strings.Contains(ok.Detail, "18 s") {
		t.Errorf("done = %+v", ok)
	}
	back := wifiOpOf(&db.WifiOp{Action: "connect", SSID: "AIO-Kitchen", Status: "failed", Output: `{"error":"switch_failed","reason":"auth_failed","reverted_to":"AIO-Guest"}`}, now)
	if back.Title != "Back on AIO-Guest" || !strings.Contains(back.Detail, "password was refused") {
		t.Errorf("reverted = %+v", back)
	}
}

func TestNormalizeWifiConnect(t *testing.T) {
	b, msg := product.NormalizeCommandPayload("wifi_config", json.RawMessage(`{"action":"connect","ssid":"AIO-Kitchen","password":"ignored-here"}`))
	if msg != "" || string(b) != `{"action":"connect","ssid":"AIO-Kitchen"}` {
		t.Errorf("connect = %s, %q", b, msg)
	}
}

func TestWifiCardRenders(t *testing.T) {
	src, _ := os.ReadFile("handlers.go")
	funcs := template.FuncMap{}
	for _, re := range []*regexp.Regexp{regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`), regexp.MustCompile(`funcMap\["([a-zA-Z][a-zA-Z0-9]*)"\]`)} {
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			funcs[m[1]] = func(...any) any { return "" }
		}
	}
	tmpl := template.Must(template.New("").Funcs(funcs).ParseGlob("../../templates/*.html"))
	v := deviceWifiView{Serial: "S1", Supported: true, CanSwitch: true, CanManage: true, ScanKnown: true, Current: "on AIO-Guest · 5 GHz",
		Nets: []wifiNetView{{SSID: "AIO-Guest", Security: "WPA2", Connected: true, Signal: -58, InRange: true, Bars: 3},
			{SSID: "AIO-Kitchen", Security: "WPA2", Signal: -67, InRange: true, Bars: 2, CanForget: true},
			{SSID: "AIO-Setup", Security: "OPEN", CanForget: true}},
		Op: &wifiOpView{Running: true, Title: "Switching to AIO-Kitchen…"}}
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, "dd-wifi-card", v); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, w := range []string{`id="dd-wifi"`, `hx-trigger="every 4s"`, `in range · -67 dBm`, `not seen nearby`, `name="action" value="connect"`,
		`goes back to <b>AIO-Guest</b>`, `Switching to AIO-Kitchen…`, `name="action" value="remove"`} {
		if !strings.Contains(out, w) {
			t.Errorf("card missing %q", w)
		}
	}
	if strings.Count(out, `value="connect"`) != 2 {
		t.Error("switch offered for the connected network, or missing for the others")
	}
}
