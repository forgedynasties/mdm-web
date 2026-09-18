package dashboard

import (
	"encoding/json"
	"testing"
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
