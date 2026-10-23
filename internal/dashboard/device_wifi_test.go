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
	for _, w := range []string{`id="dd-wifi"`, `hx-trigger="every 4s" hx-target="this"`, `in range · -67 dBm`, `not seen nearby`, `name="action" value="connect"`,
		`goes back to <b>AIO-Guest</b>`, `Switching to AIO-Kitchen…`, `name="action" value="remove"`} {
		if !strings.Contains(out, w) {
			t.Errorf("card missing %q", w)
		}
	}
	if strings.Count(out, `value="connect"`) != 2 {
		t.Error("switch offered for the connected network, or missing for the others")
	}
}

// Add network W1: the venue's networks the device lacks, in range first, then by name.
func TestVenueWifiOptions(t *testing.T) {
	net := func(ssid, sec string) db.VenueWifiNetwork {
		return db.VenueWifiNetwork{ID: uuid.New(), SSID: ssid, Security: sec}
	}
	nets := []db.VenueWifiNetwork{net("Bistro-Patio", "wpa2"), net("Bistro-Back", "wpa2"), net("Bistro-Kitchen", "wpa3"), net("AIO-Guest", "wpa2")}
	saved := []savedNetwork{{SSID: "aio-guest", Connected: true}}
	best := map[string]int{"Bistro-Kitchen": -48, "Bistro-Back": -82}
	opts, allOn := venueWifiOptions(nets, saved, best)
	if allOn || len(opts) != 3 {
		t.Fatalf("opts = %+v allOn %v", opts, allOn)
	}
	if opts[0].SSID != "Bistro-Kitchen" || !opts[0].InRange || opts[0].Security != "WPA3" ||
		opts[1].SSID != "Bistro-Back" || !opts[1].Weak || opts[2].SSID != "Bistro-Patio" || opts[2].InRange {
		t.Errorf("order/signal = %+v", opts)
	}
	if _, allOn := venueWifiOptions(nets[3:], saved, best); !allOn {
		t.Error("every venue network on the device should read allOn")
	}
}

func TestWifiCardVenueList(t *testing.T) {
	src, _ := os.ReadFile("handlers.go")
	funcs := template.FuncMap{}
	for _, re := range []*regexp.Regexp{regexp.MustCompile(`(?m)^\s*"([a-zA-Z][a-zA-Z0-9]*)":\s`), regexp.MustCompile(`funcMap\["([a-zA-Z][a-zA-Z0-9]*)"\]`)} {
		for _, m := range re.FindAllStringSubmatch(string(src), -1) {
			funcs[m[1]] = func(...any) any { return "" }
		}
	}
	tmpl := template.Must(template.New("").Funcs(funcs).ParseGlob("../../templates/*.html"))
	v := deviceWifiView{Serial: "S1", Supported: true, CanSwitch: true, CanManage: true, ScanKnown: true, VenueID: "v1", VenueName: "AIO Test Bistro",
		VenueNets: []wifiVenueOpt{{ID: "n1", SSID: "Bistro-Kitchen", Security: "WPA2", Signal: -48, InRange: true}, {ID: "n2", SSID: "Bistro-Patio", Security: "WPA2"}},
		Op:        &wifiOpView{Ok: true, Title: "Saved Bistro-Kitchen", SwitchTo: "Bistro-Kitchen"}}
	var b bytes.Buffer
	if err := tmpl.ExecuteTemplate(&b, "dd-wifi-card", v); err != nil {
		t.Fatal(err)
	}
	out := b.String()
	for _, w := range []string{`value="venue_add"`, `Saved for AIO Test Bistro`, `value="n1" checked`, `in range · -48 dBm`, `not seen nearby`,
		`Another network…`, `id="wf-manual"`, `Switch to Bistro-Kitchen now`} {
		if !strings.Contains(out, w) {
			t.Errorf("card missing %q", w)
		}
	}
	// No venue networks: the typed form shows, with the way to save some.
	v.VenueNets, v.Op = nil, nil
	b.Reset()
	if err := tmpl.ExecuteTemplate(&b, "dd-wifi-card", v); err != nil {
		t.Fatal(err)
	}
	if out := b.String(); strings.Contains(out, `value="venue_add"`) || !strings.Contains(out, `/restaurants/v1?open=wifi`) {
		t.Errorf("empty venue list: %s", out[:200])
	}
}
