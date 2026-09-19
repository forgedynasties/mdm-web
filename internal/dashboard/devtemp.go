package dashboard

import (
	"encoding/json"
	"fmt"

	"mdm/internal/db"
)

// Where a device's temperature reading comes from. They run in different ranges, so
// each has its own thresholds.
const (
	tempSrcBattery = "battery" // battery_temp_c: battery-broadcast thermistor (T7, kiosks, phones)
	tempSrcCPU     = "cpu"     // cpu_temp_c: SoC sensor, sent by battery-less TV boxes
)

// deviceTempC is the temperature the dashboard shows for a device. A device that
// reports battery_temp_c is shown that. A battery-less TV box / dongle reports its
// SoC as cpu_temp_c instead, because its battery broadcast carries a meaningless 0.
func deviceTempC(raw json.RawMessage) (temp float64, src string, ok bool) {
	if t, ok := extractBatteryTempC(raw); ok {
		return t, tempSrcBattery, true
	}
	if t, ok := extractExtraFloat(raw, "cpu_temp_c"); ok {
		return t, tempSrcCPU, true
	}
	return 0, "", false
}

// tempLevel grades a reading as "ok", "warn" or "danger" (the CSS classes the
// dashboard uses), on the bands the fleet's temperature filter shares (db.*Temp*).
// Battery: warm from 40 °C, and at or below 0 °C the reading is either a
// cold-soaked pack or a dead sensor. CPU: an SoC idles around 55–60 °C; warn at
// 70 °C, where the RK3528 thermal HAL starts severe throttling, danger at 85.
func tempLevel(temp float64, src string) string {
	if src == tempSrcCPU {
		switch {
		case temp >= db.CPUTempDanger:
			return "danger"
		case temp >= db.CPUTempWarn:
			return "warn"
		}
		return "ok"
	}
	switch {
	case temp >= db.BatteryTempDanger:
		return "danger"
	case temp >= db.BatteryTempWarn:
		return "warn"
	case temp <= -10:
		return "danger"
	case temp <= 0:
		return "warn"
	}
	return "ok"
}

// deviceTempStr formats deviceTempC for display ("" when there is no reading).
// Templates label a CPU reading via deviceTempSrc so it is not read as a battery's.
func deviceTempStr(raw json.RawMessage) string {
	t, _, ok := deviceTempC(raw)
	if !ok {
		return ""
	}
	return fmt.Sprintf("%.1f°C", t)
}

// deviceTempSrc is where deviceTempC's reading comes from ("battery", "cpu", or "").
func deviceTempSrc(raw json.RawMessage) string {
	_, src, _ := deviceTempC(raw)
	return src
}

// deviceTempClass is tempLevel for a device's latest reading ("" when none).
func deviceTempClass(raw json.RawMessage) string {
	t, src, ok := deviceTempC(raw)
	if !ok {
		return ""
	}
	return tempLevel(t, src)
}

func extractExtraFloat(raw json.RawMessage, key string) (float64, bool) {
	if len(raw) == 0 {
		return 0, false
	}
	var m map[string]json.RawMessage
	if err := json.Unmarshal(raw, &m); err != nil {
		return 0, false
	}
	v, ok := m[key]
	if !ok {
		return 0, false
	}
	var f float64
	if err := json.Unmarshal(v, &f); err != nil {
		return 0, false
	}
	return f, true
}
