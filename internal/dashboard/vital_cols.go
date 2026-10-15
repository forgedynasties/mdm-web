package dashboard

import (
	"encoding/json"
	"fmt"

	"mdm/internal/db"
)

// vitalCol is one column of the device pane's vitals (static/device-meters-demos.html,
// Me, picked 15 Oct): a level drawn as an upright tube, the value beside it and a word
// under it.
type vitalCol struct {
	Key, Label, Value, Sub string
	Tone                   string // ok | warn | danger | muted
	Pct                    int    // tube fill, 0–100
}

func clampPct(v float64) int {
	switch {
	case v < 0:
		return 0
	case v > 100:
		return 100
	}
	return int(v + .5)
}

// vitalColumns builds the columns for a device: power, temperature, RAM and Wi-Fi, each
// only when the device reports it. d is *db.Device or db.Device (both reach templates).
func vitalColumns(dv any, showBattery, flapping bool) []vitalCol {
	var d *db.Device
	switch v := dv.(type) {
	case *db.Device:
		d = v
	case db.Device:
		d = &v
	}
	if d == nil {
		return nil
	}
	raw := d.LatestExtra
	var out []vitalCol

	switch {
	case extractBatteryMissing(raw):
		out = append(out, vitalCol{Key: "battery", Label: "Battery", Value: "None", Sub: "no pack detected", Tone: "danger"})
	case showBattery:
		c := vitalCol{Key: "battery", Label: "Battery", Value: fmt.Sprintf("%d%%", d.BatteryPct), Pct: clampPct(float64(d.BatteryPct)), Tone: "ok"}
		switch {
		case d.BatteryPct < 20:
			c.Tone = "danger"
		case d.BatteryPct < 50:
			c.Tone = "warn"
		}
		switch {
		case flapping:
			c.Sub, c.Tone = "charger flapping", "warn"
		case extractCharging(raw):
			c.Sub = "charging"
		default:
			c.Sub = "on battery"
		}
		out = append(out, c)
	case d.HasBattery():
		out = append(out, vitalCol{Key: "battery", Label: "Battery", Value: "—", Sub: "no reading yet", Tone: "muted"})
	default:
		out = append(out, vitalCol{Key: "power", Label: "Power", Value: "AC", Sub: "mains powered", Pct: 100, Tone: "muted"})
	}

	if t, src, ok := deviceTempC(raw); ok {
		lvl := tempLevel(t, src)
		c := vitalCol{Key: "temp", Label: "Temp", Value: fmt.Sprintf("%.1f°", t), Pct: clampPct(t / 60 * 100), Tone: lvl, Sub: "normal"}
		switch lvl {
		case "warn":
			c.Sub = "warm"
		case "danger":
			c.Sub = "hot"
		}
		if src == tempSrcCPU {
			c.Sub += " · CPU"
		}
		out = append(out, c)
	}

	var ram struct {
		Used  int `json:"used"`
		Total int `json:"total"`
	}
	var m map[string]json.RawMessage
	if json.Unmarshal(raw, &m) == nil {
		if v, ok := m["ram_usage_mb"]; ok && json.Unmarshal(v, &ram) == nil && ram.Total > 0 {
			p := float64(ram.Used) * 100 / float64(ram.Total)
			c := vitalCol{Key: "ram", Label: "RAM", Value: fmt.Sprintf("%d%%", clampPct(p)), Pct: clampPct(p), Tone: "muted",
				Sub: fmt.Sprintf("of %.1f GB", float64(ram.Total)/1024)}
			switch {
			case p >= 90:
				c.Tone = "danger"
			case p >= 75:
				c.Tone = "warn"
			}
			out = append(out, c)
		}
	}

	if rssi, ok := extractExtraFloat(raw, "wifi_rssi"); ok && rssi < 0 {
		c := vitalCol{Key: "wifi", Label: "Wi-Fi", Value: fmt.Sprintf("%.0f", rssi), Pct: clampPct((rssi + 90) / 60 * 100), Tone: "ok", Sub: "dBm · strong"}
		switch {
		case rssi < -75:
			c.Tone, c.Sub = "warn", "dBm · weak"
		case rssi < -60:
			c.Tone, c.Sub = "muted", "dBm · fair"
		}
		out = append(out, c)
	}
	return out
}
