package api

import (
	"encoding/json"
	"testing"
)

// TestIsDPCPayload covers the gate that the "ignore DPC check-ins" setting uses.
// A false negative here would keep storing the rows the setting exists to stop; a
// false positive would silently drop a firmware device's telemetry.
func TestIsDPCPayload(t *testing.T) {
	cases := []struct {
		name  string
		extra string
		want  bool
	}{
		{"dpc keyframe", `{"agent_type":"dpc","capabilities":["reboot"],"model":"SM-T220"}`, true},
		{"dpc delta", `{"agent_type":"dpc","uptime_seconds":91,"battery_temp_c":31.0}`, true},
		{"firmware client", `{"wlc_status":"charging","storage_free_gb":12,"screen_on":true}`, false},
		{"firmware names its own kind", `{"agent_type":"firmware","wifi_rssi":-54}`, false},
		{"empty object", `{}`, false},
		{"absent extra", ``, false},
		{"malformed json", `{"agent_type":`, false},
		{"agent_type not a string", `{"agent_type":3}`, false},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			if got := isDPCPayload(json.RawMessage(c.extra)); got != c.want {
				t.Errorf("isDPCPayload(%s) = %v, want %v", c.extra, got, c.want)
			}
		})
	}
}
