package dashboard

import (
	"encoding/json"
	"strings"
	"testing"

	"mdm/internal/db"
)

func TestDeviceTemp(t *testing.T) {
	for _, tc := range []struct {
		extra, str, src, class string
	}{
		{`{"battery_temp_c":38.5}`, "38.5°C", tempSrcBattery, "ok"},
		{`{"battery_temp_c":47}`, "47.0°C", tempSrcBattery, "warn"},
		// A TV box's SoC idles near 60 °C: fine for a CPU, "hot" for a battery.
		{`{"cpu_temp_c":58.3}`, "58.3°C", tempSrcCPU, "ok"},
		{`{"cpu_temp_c":72}`, "72.0°C", tempSrcCPU, "warn"},
		{`{"cpu_temp_c":90}`, "90.0°C", tempSrcCPU, "danger"},
		// A battery reading wins when both are present.
		{`{"battery_temp_c":30,"cpu_temp_c":65}`, "30.0°C", tempSrcBattery, "ok"},
		{`{}`, "", "", ""},
	} {
		raw := json.RawMessage(tc.extra)
		if got := deviceTempStr(raw); got != tc.str {
			t.Errorf("%s: str %q, want %q", tc.extra, got, tc.str)
		}
		if got := deviceTempSrc(raw); got != tc.src {
			t.Errorf("%s: src %q, want %q", tc.extra, got, tc.src)
		}
		if got := deviceTempClass(raw); got != tc.class {
			t.Errorf("%s: class %q, want %q", tc.extra, got, tc.class)
		}
	}
}

func TestPostureCompliance(t *testing.T) {
	d := db.Device{LatestExtra: json.RawMessage(`{"su_present":true,"dev_options_enabled":false,"adb_tcp":true,
		"unknown_sources":true,"unknown_source_apps":["com.apkpure.aegon"],"play_protect":false,
		"accessibility_services":["com.oranth.accessibility/com.oranth.accessibility.MyAccessibilityService","com.ok/.Svc"]}`)}
	rules := []db.ComplianceRule{
		{Kind: "forbid_root", Enabled: true}, {Kind: "forbid_dev_options", Enabled: true},
		{Kind: "forbid_network_adb", Enabled: true}, {Kind: "forbid_unknown_sources", Enabled: true},
		{Kind: "require_play_protect", Enabled: true},
		{Kind: "allowed_accessibility", Param: "com.ok", Enabled: true},
		{Kind: "forbid_apps", Param: "cm.aptoide.pt, com.apkpure.aegon", Enabled: true},
	}
	got := complianceIssues(rules, d, map[string]bool{"com.apkpure.aegon": true})
	var texts []string
	for _, is := range got {
		texts = append(texts, is.Text)
	}
	want := []string{
		"Rooted: su binary present",
		"ADB listening on the network",
		"Unknown sources allowed (com.apkpure.aegon)",
		"Play Protect off",
		"Unexpected accessibility service: com.oranth.accessibility/com.oranth.accessibility.MyAccessibilityService",
		"Forbidden app installed: com.apkpure.aegon",
	}
	if strings.Join(texts, "|") != strings.Join(want, "|") {
		t.Errorf("issues:\n got %q\nwant %q", texts, want)
	}
	// A device that never reported posture is unknown, not failing.
	if n := len(complianceIssues(rules, db.Device{}, nil)); n != 0 {
		t.Errorf("unreported posture produced %d issues", n)
	}
}
