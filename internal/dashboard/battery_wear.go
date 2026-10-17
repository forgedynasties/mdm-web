package dashboard

import (
	"context"
	"encoding/json"
	"sort"
	"strconv"
	"sync"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
)

// ── Battery wear (B1 + B2, picked 17 Oct) ─────────────────────────────────────
//
// The T7's gauge has no cycle count or health value (see the firmware client's
// BatteryWear.java), so wear is read from the charge climbs it sends: how long the
// constant-current phase takes to lift the cell from 3.7 to 4.1 V. A pack that holds less
// climbs faster. On live (15–17 Oct) the same device repeats within 1–3 % night to night
// and the fleet sits around 5 h 32 m.
//
// What this does NOT do yet is call a battery worn. No pack of known capacity has been
// measured, so there is no calibrated line; the page shows the measurement and how it
// compares with the fleet and with the device's own first weeks. A "Battery wearing"
// alert and a line in the venue report wait for a bench capacity test of the two fast
// lab units (AT070AABU00044 at 84 %, AT070AABU00387 at 87 % of the fleet).

const (
	wearFirstRungMV  = 3700 // the ladder the client times: 3700, 3800, … 4100 mV
	wearRungs        = 5
	wearMaxScreenS   = 60 // screen-on seconds allowed during a clean climb
	wearMaxGapS      = 60 // widest gap between samples the client may have interpolated over
	wearHotC         = 50.0
	wearRecent       = 5                   // a device's figure is the mean of its last few clean climbs
	wearWindow       = 30 * 24 * time.Hour // the fleet comparison looks this far back
	wearFastFleet    = 0.92                // below this share of the fleet median: "faster than fleet"
	wearFastPct      = 92                  // the same line, as the page writes it
	wearBaseClimbs   = 3                   // a device's own baseline: its first clean climbs…
	wearBaseMinSpan  = 21 * 24 * time.Hour // …and the trend against it only after this much history
	wearCardReadings = 14
)

// climbReading is one clean climb: when, and the seconds from 3.7 to 4.1 V.
type climbReading struct {
	At   time.Time
	Secs int
	MaxC float64
	Hot  bool
}

// readClimb returns the climb as a reading when it is clean enough to compare: the full
// ladder from the first rung, the screen off for practically all of it, sampled densely,
// and the charger steady (the client already voids a flapping one).
func readClimb(at time.Time, payload json.RawMessage) (climbReading, bool) {
	var p struct {
		MV0  int       `json:"mv0"`
		Step int       `json:"step"`
		S    []int     `json:"s"`
		Scr  []int     `json:"scr"`
		Gap  int       `json:"gap_s"`
		Temp []float64 `json:"temp_c"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return climbReading{}, false
	}
	if p.MV0 != wearFirstRungMV || p.Step != 100 || len(p.S) != wearRungs || p.Gap > wearMaxGapS {
		return climbReading{}, false
	}
	if len(p.Scr) == wearRungs && p.Scr[wearRungs-1] > wearMaxScreenS {
		return climbReading{}, false
	}
	secs := p.S[wearRungs-1]
	if secs <= 0 {
		return climbReading{}, false
	}
	r := climbReading{At: at, Secs: secs}
	if len(p.Temp) == 2 {
		r.MaxC = p.Temp[1]
		r.Hot = p.Temp[1] >= wearHotC
	}
	return r, true
}

// wearRow is one device on the Wear tab.
type wearRow struct {
	DeviceID uuid.UUID
	Serial   string
	Venue    string
	Readings []climbReading // oldest first, the window's clean climbs
	Latest   climbReading
	MeanSecs int
	VsFleet  int // percent of the fleet median, 0 when there is no median
	Fast     bool
	Hot      bool // the latest climb ran at or above wearHotC
}

// fleetWear is the Wear tab: every device with a clean climb in the window, fastest
// (least charge held) first, and the fleet median the rows are compared with.
type fleetWear struct {
	Rows       []wearRow
	MedianSecs int
	Devices    int
	FastCount  int
	WindowDays int
	MinS, MaxS int // the spread strip's scale
}

func meanRecent(rs []climbReading) int {
	n := min(len(rs), wearRecent)
	if n == 0 {
		return 0
	}
	sum := 0
	for _, r := range rs[len(rs)-n:] {
		sum += r.Secs
	}
	return sum / n
}

func medianInt(xs []int) int {
	if len(xs) == 0 {
		return 0
	}
	s := append([]int(nil), xs...)
	sort.Ints(s)
	if len(s)%2 == 1 {
		return s[len(s)/2]
	}
	return (s[len(s)/2-1] + s[len(s)/2]) / 2
}

// computeFleetWear builds the Wear tab from the window's climbs (oldest first).
func computeFleetWear(climbs []db.DeviceBatteryClimb) fleetWear {
	byDev := map[uuid.UUID]*wearRow{}
	var order []uuid.UUID
	for _, c := range climbs {
		rd, ok := readClimb(c.At, c.Payload)
		if !ok {
			continue
		}
		row, seen := byDev[c.DeviceID]
		if !seen {
			row = &wearRow{DeviceID: c.DeviceID, Serial: c.Serial, Venue: c.Venue}
			byDev[c.DeviceID] = row
			order = append(order, c.DeviceID)
		}
		row.Readings = append(row.Readings, rd)
	}
	fw := fleetWear{WindowDays: int(wearWindow.Hours() / 24)}
	var means []int
	for _, id := range order {
		row := byDev[id]
		row.Latest = row.Readings[len(row.Readings)-1]
		row.Hot = row.Latest.Hot
		row.MeanSecs = meanRecent(row.Readings)
		means = append(means, row.MeanSecs)
	}
	fw.MedianSecs = medianInt(means)
	fw.Devices = len(order)
	for _, id := range order {
		row := byDev[id]
		if fw.MedianSecs > 0 {
			row.VsFleet = int(float64(row.MeanSecs)*100/float64(fw.MedianSecs) + 0.5)
			row.Fast = float64(row.MeanSecs) < wearFastFleet*float64(fw.MedianSecs)
		}
		if row.Fast {
			fw.FastCount++
		}
		for _, r := range row.Readings {
			if fw.MinS == 0 || r.Secs < fw.MinS {
				fw.MinS = r.Secs
			}
			fw.MaxS = max(fw.MaxS, r.Secs)
		}
		fw.Rows = append(fw.Rows, *row)
	}
	sort.SliceStable(fw.Rows, func(i, j int) bool { return fw.Rows[i].MeanSecs < fw.Rows[j].MeanSecs })
	return fw
}

// wearPos places seconds on the spread strip, 0–100.
func (fw fleetWear) Pos(secs int) int {
	lo, hi := fw.MinS, fw.MaxS
	if lo == 0 || hi <= lo {
		return 50
	}
	// A little air at both ends so the extreme dots aren't cut by the strip's edge.
	pad := (hi - lo) / 10
	lo, hi = lo-pad, hi+pad
	return (secs - lo) * 100 / (hi - lo)
}

// batteryWearCard is the device page's Battery card (New view).
type batteryWearCard struct {
	Readings     []climbReading // last wearCardReadings clean climbs, oldest first
	Clean, Total int            // clean climbs out of all climbs on record
	MeanSecs     int
	FleetSecs    int
	VsFleet      int  // percent of the fleet median
	Fast         bool // below wearFastFleet of the fleet
	HasOwn       bool // enough history for the trend against its own first climbs
	OwnSecs      int  // the baseline: mean of its first wearBaseClimbs clean climbs
	VsOwn        int  // percent of its own baseline
	MaxBarSecs   int  // the bars' scale
}

// computeWearCard reads a device's climbs (any order) against the fleet median.
func computeWearCard(climbs []db.BatteryClimb, fleetSecs int) batteryWearCard {
	card := batteryWearCard{Total: len(climbs), FleetSecs: fleetSecs}
	var rs []climbReading
	for _, c := range climbs {
		if rd, ok := readClimb(c.At, c.Payload); ok {
			rs = append(rs, rd)
		}
	}
	sort.Slice(rs, func(i, j int) bool { return rs[i].At.Before(rs[j].At) })
	card.Clean = len(rs)
	if len(rs) == 0 {
		return card
	}
	card.MeanSecs = meanRecent(rs)
	if fleetSecs > 0 {
		card.VsFleet = int(float64(card.MeanSecs)*100/float64(fleetSecs) + 0.5)
		card.Fast = float64(card.MeanSecs) < wearFastFleet*float64(fleetSecs)
	}
	if len(rs) >= wearBaseClimbs+wearRecent && rs[len(rs)-1].At.Sub(rs[0].At) >= wearBaseMinSpan {
		sum := 0
		for _, r := range rs[:wearBaseClimbs] {
			sum += r.Secs
		}
		card.OwnSecs = sum / wearBaseClimbs
		card.HasOwn = true
		card.VsOwn = int(float64(card.MeanSecs)*100/float64(card.OwnSecs) + 0.5)
	}
	if len(rs) > wearCardReadings {
		rs = rs[len(rs)-wearCardReadings:]
	}
	card.Readings = rs
	card.MaxBarSecs = fleetSecs
	for _, r := range rs {
		card.MaxBarSecs = max(card.MaxBarSecs, r.Secs)
	}
	return card
}

// BarPct is a reading's bar height on the card, percent of the tallest.
func (c batteryWearCard) BarPct(secs int) int {
	if c.MaxBarSecs <= 0 {
		return 0
	}
	return secs * 100 / c.MaxBarSecs
}

// ── The fleet median, cached: the device page asks for it on every view. ─────────

var wearCache struct {
	sync.Mutex
	at time.Time
	fw fleetWear
}

const wearCacheTTL = 10 * time.Minute

// fleetWearNow is the Wear tab's data, at most wearCacheTTL old. Errors return the last
// good answer (or an empty one) — wear is never worth failing a page over.
func (h *Handler) fleetWearNow(ctx context.Context) fleetWear {
	wearCache.Lock()
	defer wearCache.Unlock()
	if !wearCache.at.IsZero() && time.Since(wearCache.at) < wearCacheTTL {
		return wearCache.fw
	}
	climbs, err := h.db.BatteryClimbsSince(ctx, time.Now().Add(-wearWindow))
	if err != nil {
		return wearCache.fw
	}
	wearCache.fw, wearCache.at = computeFleetWear(climbs), time.Now()
	return wearCache.fw
}

// fmtClimb renders climb seconds as "5 h 32 m".
func fmtClimb(secs int) string {
	m := (secs + 30) / 60
	if m < 60 {
		return strconv.Itoa(m) + " m"
	}
	return strconv.Itoa(m/60) + " h " + strconv.Itoa(m%60) + " m"
}

// batteryWearFor is the device page's Battery card, or nil when the device sends no
// climbs: only the firmware client logs them, and only a device with a battery has any.
func (h *Handler) batteryWearFor(ctx context.Context, d *db.Device) *batteryWearCard {
	if d.IsDPC() || !d.HasBattery() {
		return nil
	}
	climbs, err := h.db.BatteryClimbsFor(ctx, d.ID, 400)
	if err != nil || len(climbs) == 0 {
		return nil
	}
	card := computeWearCard(climbs, h.fleetWearNow(ctx).MedianSecs)
	return &card
}

// visibleWear narrows the Wear tab to the devices the caller may see. The median stays the
// fleet's: it is the yardstick, not a list of devices.
func visibleWear(fw fleetWear, visible func(uuid.UUID) bool) fleetWear {
	rows := fw.Rows[:0:0]
	fw.FastCount = 0
	for _, r := range fw.Rows {
		if visible(r.DeviceID) {
			rows = append(rows, r)
			if r.Fast {
				fw.FastCount++
			}
		}
	}
	fw.Rows = rows
	return fw
}
