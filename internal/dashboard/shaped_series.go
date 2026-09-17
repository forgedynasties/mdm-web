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
			p.TempC, p.HasTemp = *s.TempC, true
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
			same := (last.C == nil && st == nil) || (last.C != nil && st != nil && *last.C == *st)
			if same && x-last.To <= gapMs {
				last.To = x
				continue
			}
		}
		runs = append(runs, chargeRun{From: x, To: x, C: st})
	}
	return runs
}

// buildChartBody renders the chart payload from shaped points. Deliberately the same
// series, the same field names and the same decimation as the check-in path: this is a
// change of source, not of what the chart shows, and the two are diffed against each
// other on real data before either is trusted.
func buildChartBody(device *db.Device, pts []shapedPoint) ([]byte, error) {
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
	return json.Marshal(map[string]any{"battery": battery, "temp": temp, "ram": ram, "charge": charge})
}

// buildChartBodyFromCheckins is the check-in path's series construction, lifted out so
// the two sources can be rendered side by side and diffed on real data.
func buildChartBodyFromCheckins(device *db.Device, asc []db.Checkin) ([]byte, error) {
	pts := make([]shapedPoint, 0, len(asc))
	for _, c := range asc {
		p := shapedPoint{At: c.CreatedAt, BatteryPct: c.BatteryPct, HasBattery: true}
		if t, ok := extractBatteryTempC(c.Extra); ok {
			p.TempC, p.HasTemp = t, true
		}
		if rp, ok := ramPctFromExtra(c.Extra); ok {
			p.RAMPct, p.HasRAM = rp, true
		}
		if w := wlcIntFromExtra(c.Extra); w != nil {
			p.WlcStatus, p.HasWlc = *w, true
		}
		if ch := chargingFromExtra(c.Extra); ch != nil {
			p.Charging, p.HasCharge = *ch, true
		}
		pts = append(pts, p)
	}
	return buildChartBody(device, pts)
}

// shapedCoversExport reports whether every selected device's shaped history reaches
// back to the start of the window. One device short of it is enough to send the whole
// export to checkins: a CSV where some devices' rows come from one source and some
// from another would be the hardest kind of discrepancy to notice, since it would look
// like a per-device data difference rather than a source difference.
func (h *Handler) shapedCoversExport(ctx context.Context, deviceIDs []uuid.UUID, from time.Time) bool {
	for _, id := range deviceIDs {
		coverFrom, ok, err := h.db.ShapedCoverage(ctx, id)
		if err != nil || !ok || from.Before(coverFrom) {
			return false
		}
	}
	return true
}

// checkinShapeStateKeys are the states a rebuilt check-in row carries: the two the
// device page's chart draws, plus the ones its vitals row reads. Loading every key a
// device has ever changed would pull in fields nothing here asks for.
var checkinShapeStateKeys = []string{"charging", "wlc_status", "wifi", "ip_address", "timezone", "screen_on"}

// shapedCheckins answers a window with check-in shaped rows built from the shaped
// tables, newest first — the order the check-in queries return and the order both
// callers reverse. Reports false when the shaped tables do not cover the window, so
// the caller falls back to the snapshots.
//
// It rebuilds each row's Extra rather than changing what reads it. The device page's
// first paint renders battery, WLC, temperature and RAM straight out of Extra in the
// template, and chargeRuns reads charging from it; handing those the same object they
// already expect means this changes where the numbers come from and nothing else. It
// is the same choice made for the CSV export and the daily rollup, and for the same
// reason: the formatting stays in one place and cannot drift between two sources.
func (h *Handler) shapedCheckins(ctx context.Context, deviceID uuid.UUID, from, until time.Time) ([]db.Checkin, bool) {
	coverFrom, ok, err := h.db.ShapedCoverage(ctx, deviceID)
	if err != nil || !ok || from.Before(coverFrom) {
		return nil, false
	}
	samples, err := h.db.GetDeviceSamples(ctx, deviceID, from, until)
	if err != nil || len(samples) == 0 {
		return nil, false
	}
	events, err := h.db.GetStateTimeline(ctx, deviceID, checkinShapeStateKeys, from, until)
	if err != nil {
		return nil, false
	}

	cur := map[string]string{}
	next := 0
	out := make([]db.Checkin, len(samples))
	for i, s := range samples {
		for next < len(events) && !events[next].At.After(s.At) {
			cur[events[next].Key] = events[next].Value
			next++
		}
		c := db.Checkin{CreatedAt: s.At, Extra: db.ShapedExtra(s, cur)}
		if s.BatteryPct != nil {
			c.BatteryPct = int(*s.BatteryPct)
		}
		out[len(samples)-1-i] = c // samples come oldest first; callers want newest first
	}
	return out, true
}
