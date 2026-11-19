package dashboard

import (
	"fmt"
	"math"

	"mdm/internal/db"
)

// Bento device page (E1, picked 18 Nov 2026): the Overview's panes for one device. A radar of
// the device's health parts, the Today card (tabs Graph · Needs · Activity · Apps, each a list
// and the picked item explained), a coral action card, rings and a few facts. The numbers come
// from what the page already holds; nothing new is queried.

// bentoPart is one spoke of the device's health radar. Tab is the device tab it opens.
type bentoPart struct {
	N        int
	Name     string
	Pct      int
	Tone     string // g | y | o | r
	Tab, Why string
}

// bentoRing is one dial of the Vitals card.
type bentoRing struct {
	Label, Text, Dash, Circ, Tone, Tab string
}

type bentoView struct {
	Score   int
	Tone    string
	Parts   []bentoPart
	Radar   ovRadar
	HasRing bool
	Rings   []bentoRing
}

// deviceBentoOf scores the device's parts and lays out the radar and rings. cols are the
// vital columns the Overview tab already draws.
func deviceBentoOf(d *db.Device, online bool, health deviceHealth, cols []vitalCol, lockedApplicable, locked, clientCurrent bool) bentoView {
	var parts []bentoPart
	add := func(name string, pct int, tab, why string) {
		pct = clampPct(float64(pct))
		parts = append(parts, bentoPart{N: len(parts) + 1, Name: name, Pct: pct, Tone: ovTone(pct), Tab: tab, Why: why})
	}
	if online {
		add("Online", 100, "network", "Connected now")
	} else {
		add("Online", 0, "network", "Not connected")
	}
	if d.HasBattery() && d.BatteryPct >= 0 {
		add("Battery", d.BatteryPct, "summary", "Charge now")
	}
	if rssi, ok := extractExtraFloat(d.LatestExtra, "wifi_rssi"); ok && rssi < 0 {
		add("Signal", int(math.Round((rssi+95)*100/45)), "network", fmt.Sprintf("%.0f dBm", rssi))
	}
	if p, ok := ramPctFromExtra(d.LatestExtra); ok {
		add("Memory", int(100-p), "summary", fmt.Sprintf("%.0f%% in use", p))
	}
	if health.Scored > 0 {
		add("Compliance", health.Passing*100/health.Scored, "health", fmt.Sprintf("%d of %d checks", health.Passing, health.Scored))
	}
	if lockedApplicable {
		if locked {
			add("Lock", 100, "about", "Locked to its app")
		} else {
			add("Lock", 0, "about", "Not locked")
		}
	}
	if clientCurrent {
		add("Client current", 100, "about", "On the hosted MDM client")
	} else {
		add("Client current", 40, "about", "A newer MDM client is hosted")
	}
	v := bentoView{Parts: parts}
	sum := 0
	for _, p := range parts {
		sum += p.Pct
	}
	if len(parts) > 0 {
		v.Score = sum / len(parts)
	}
	v.Tone = "g"
	switch {
	case v.Score < 55:
		v.Tone = "r"
	case v.Score < 75:
		v.Tone = "o"
	}
	if len(parts) >= 3 {
		ov := make([]ovPart, len(parts))
		for i, p := range parts {
			ov[i] = ovPart{N: p.N, Name: p.Name, Pct: p.Pct, Tone: p.Tone}
		}
		v.Radar = ovRadarOf(ov)
		v.HasRing = true
	}
	const circ = 2 * math.Pi * 33
	for _, c := range cols {
		tone := "g"
		switch c.Tone {
		case "warn":
			tone = "o"
		case "danger":
			tone = "r"
		}
		tab := "summary"
		if c.Key == "wifi" {
			tab = "network"
		}
		v.Rings = append(v.Rings, bentoRing{Label: c.Label, Text: c.Value, Dash: fmt.Sprintf("%.1f", circ*float64(c.Pct)/100), Circ: fmt.Sprintf("%.1f", circ), Tone: tone, Tab: tab})
		if len(v.Rings) == 4 {
			break
		}
	}
	return v
}

// needOne wraps one to-do as a list, so a detail pane can draw it with dd-todo-rows.
func needOne(n deviceNeed) []deviceNeed { return []deviceNeed{n} }
