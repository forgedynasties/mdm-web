package dashboard

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"

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
	Flapping   bool // charging "2": a flapping charger (db/charger_flap.go)
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
		// A battery reading when there is one; a battery-less TV box has only its SoC.
		if s.TempC != nil {
			p.TempC, p.HasTemp = *s.TempC, true
		} else if s.CPUTempC != nil {
			p.TempC, p.HasTemp = *s.CPUTempC, true
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
			p.Charging, p.HasCharge, p.Flapping = v == "true", true, v == "2"
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

// shapedChartSeries answers a chart window from the shaped tables, or reports that it
// cannot. A window is only answerable when both halves cover it: the numbers come from
// device_samples and the pad markers and charge runs from device_state_events, so
// serving a window the events do not reach would quietly drop those series from an old
// chart rather than fail.
func (h *Handler) shapedChartSeries(ctx context.Context, deviceID uuid.UUID, from, until time.Time) ([]shapedPoint, bool) {
	coverFrom, ok, err := h.db.ShapedCoverage(ctx, deviceID)
	if err != nil || !ok || from.Before(coverFrom) {
		return nil, false
	}
	samples, err := h.db.GetDeviceSamples(ctx, deviceID, from, until)
	if err != nil || len(samples) == 0 {
		return nil, false
	}
	// Only the two keys the chart draws. Everything else in the event stream is for
	// other readers and would be loaded for nothing.
	events, err := h.db.GetStateTimeline(ctx, deviceID, []string{"charging", "wlc_status"}, from, until)
	if err != nil {
		return nil, false
	}
	return mergeShapedSeries(samples, events), true
}

// shapedChargeRuns folds the per-point charging state into the runs the chart draws,
// identical in shape to chargeRuns over check-ins: consecutive points in the same state
// merge, and a gap longer than the device's expected reporting interval breaks the run
// rather than drawing a line across hours of silence.
func shapedChargeRuns(points []shapedPoint, gapMs int64) []chargeRun {
	runs := []chargeRun{}
	for _, p := range points {
		x := p.At.UnixMilli()
		var st *bool
		if p.HasCharge {
			c := p.Charging
			st = &c
		}
		if n := len(runs); n > 0 {
			last := &runs[n-1]
			if last.sameCharge(st, p.Flapping) && x-last.To <= gapMs {
				last.To = x
				continue
			}
		}
		runs = append(runs, chargeRun{From: x, To: x, C: st, F: p.Flapping})
	}
	return runs
}

// buildChartBody renders the chart payload from shaped points. Deliberately the same
// series, the same field names and the same decimation as the check-in path: this is a
// change of source, not of what the chart shows, and the two are diffed against each
// other on real data before either is trusted.
// batteryAnchor is the last battery reading BEFORE the chart window, in the chart's own
// {x,y} shape. The client seeds its cycle walk with it so a cycle whose charge happened
// off the left edge is still counted and banded (see cyclesIn in device.html).
type batteryAnchor struct {
	X int64 `json:"x"`
	Y int   `json:"y"`
}

func buildChartBody(device *db.Device, pts []shapedPoint, anchor *batteryAnchor) ([]byte, error) {
	const maxPoints = 2500
	type bpt struct {
		X   int64 `json:"x"`
		Y   int   `json:"y"`
		Wlc *int  `json:"wlc"`
	}
	type pt struct {
		X int64   `json:"x"`
		Y float64 `json:"y"`
	}
	hasBattery := device.HasBattery()
	battery := make([]bpt, 0, len(pts))
	temp := make([]pt, 0, len(pts))
	ram := make([]pt, 0, len(pts))
	for _, p := range pts {
		x := p.At.UnixMilli()
		if hasBattery {
			var wlc *int
			if p.HasWlc {
				v := p.WlcStatus
				wlc = &v
			}
			battery = append(battery, bpt{X: x, Y: p.BatteryPct, Wlc: wlc})
		}
		if p.HasTemp {
			temp = append(temp, pt{X: x, Y: p.TempC})
		}
		if p.HasRAM {
			ram = append(ram, pt{X: x, Y: p.RAMPct})
		}
	}
	var charge []chargeRun
	if hasBattery && device.HasCharging() {
		charge = shapedChargeRuns(pts, chartGapMs(device.PollIntervalMs))
	}
	if len(temp) > maxPoints {
		temp = decimateExtremes(temp, maxPoints, func(p pt) float64 { return p.Y })
	}
	if len(ram) > maxPoints {
		ram = decimateExtremes(ram, maxPoints, func(p pt) float64 { return p.Y })
	}
	if len(battery) > maxPoints {
		battery = decimateExtremes(battery, maxPoints, func(p bpt) float64 { return float64(p.Y) })
	}
	return json.Marshal(map[string]any{"battery": battery, "temp": temp, "ram": ram, "charge": charge, "battery_before": anchor})
}

// shapedCheckins delegates to the db-level builder, keeping one implementation of the
// merge shared with the public API.
func (h *Handler) shapedCheckins(ctx context.Context, deviceID uuid.UUID, from, until time.Time) ([]db.Checkin, bool) {
	out, ok, err := h.db.ShapedCheckins(ctx, deviceID, from, until, 0)
	if err != nil {
		return nil, false
	}
	return out, ok
}
