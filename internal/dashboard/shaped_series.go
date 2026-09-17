package dashboard

import (
	"time"

	"mdm/internal/db"
)

// shapedPoint is one chart point assembled from the shaped tables: the numbers from
// device_samples, and the state fields as they stood at that instant, carried forward
// from device_state_events.
type shapedPoint struct {
	At         time.Time
	BatteryPct int
	HasBattery bool
	TempC      float64
	HasTemp    bool
	RAMPct     float64
	HasRAM     bool
	WlcStatus  int
	HasWlc     bool
	Charging   bool
	HasCharge  bool
}

// mergeShapedSeries walks the samples and the state timeline together, carrying each
// state forward until the next event for that key. Both arrive ordered by time, so this
// is one pass over each rather than a lookup per sample — the state stream is tiny
// (nothing is written while a state holds) and the sample stream can be a hundred
// thousand rows.
//
// A state with no event at or before a sample is reported as unknown rather than as a
// default: before the first event for a key we genuinely do not know, and drawing an
// invented "false" there is how a chart ends up lying about when a device was charging.
func mergeShapedSeries(samples []db.DeviceSample, events []db.StateAt) []shapedPoint {
	out := make([]shapedPoint, 0, len(samples))
	cur := map[string]string{}
	next := 0

	for _, s := range samples {
		// Apply every event at or before this sample's instant.
		for next < len(events) && !events[next].At.After(s.At) {
			cur[events[next].Key] = events[next].Value
			next++
		}

		p := shapedPoint{At: s.At}
		if s.BatteryPct != nil {
			p.BatteryPct, p.HasBattery = int(*s.BatteryPct), true
		}
		if s.TempC != nil {
			p.TempC, p.HasTemp = float64(*s.TempC), true
		}
		// Percent computed here from used and total, exactly as the old path computed it
		// from the same two numbers in extra — so the chart and the CSV, which prints
		// those two columns, can never quote different figures.
		if s.RAMUsedMB != nil && s.RAMTotalMB != nil && *s.RAMTotalMB > 0 {
			p.RAMPct, p.HasRAM = float64(*s.RAMUsedMB)*100/float64(*s.RAMTotalMB), true
		}
		if v, ok := cur["wlc_status"]; ok {
			if n, ok := atoiState(v); ok {
				p.WlcStatus, p.HasWlc = n, true
			}
		}
		if v, ok := cur["charging"]; ok {
			p.Charging, p.HasCharge = v == "true", true
		}
		out = append(out, p)
	}
	return out
}

// atoiState parses a state event's stored text back to an int. Values are written by
// jsonScalar, so a number arrives as plain digits with no quotes.
func atoiState(s string) (int, bool) {
	if s == "" {
		return 0, false
	}
	n := 0
	neg := false
	for i, c := range s {
		if i == 0 && c == '-' {
			neg = true
			continue
		}
		if c < '0' || c > '9' {
			return 0, false
		}
		n = n*10 + int(c-'0')
	}
	if neg {
		n = -n
	}
	return n, true
}
