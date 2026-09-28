package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"time"

	"mdm/internal/db"

	"github.com/google/uuid"
)

// reportWindow is one week a venue report can cover: Monday to Sunday, or Monday to
// today for the week in progress.
type reportWindow struct {
	From, To time.Time // UTC days, inclusive
	Days     int       // days in the window that have begun
	Current  bool      // the week in progress; its figures still move
	Label    string
	Selected bool
}

// Value is the week's key in a URL: the Monday it starts on. A Monday is stable for
// the week in progress too, where the Sunday has not happened yet.
func (w reportWindow) Value() string { return w.From.Format("2006-01-02") }

// reportWeeksShown is how far back the week picker reaches: this week and two before.
const reportWeeksShown = 3

// reportWeeks lists the weeks a report may be opened for, newest first. The first is
// the week in progress; the rest are finished weeks (see lastFullWeek).
func reportWeeks(now time.Time) []reportWindow {
	lastFrom, lastTo := lastFullWeek(now)
	today := now.UTC().Truncate(24 * time.Hour)
	thisFrom := lastFrom.AddDate(0, 0, 7)
	out := []reportWindow{{
		From: thisFrom, To: today, Current: true,
		Days:  int(today.Sub(thisFrom).Hours()/24) + 1,
		Label: "This week · " + thisFrom.Format("2 Jan") + " – today",
	}}
	for i := 0; i < reportWeeksShown-1; i++ {
		from, to := lastFrom.AddDate(0, 0, -7*i), lastTo.AddDate(0, 0, -7*i)
		name := "Last week"
		if i > 0 {
			name = fmt.Sprintf("%d weeks ago", i+1)
		}
		out = append(out, reportWindow{
			From: from, To: to, Days: 7,
			Label: name + " · " + from.Format("2 Jan") + " – " + to.Format("2 Jan"),
		})
	}
	return out
}

// pickReportWeek resolves ?week= (a Monday) against the weeks on offer. Anything else
// — missing, malformed, older than the picker reaches — falls back to last week, the
// most recent week that has finished.
func pickReportWeek(week string, now time.Time) (reportWindow, []reportWindow) {
	weeks := reportWeeks(now)
	pick := 1
	for i, w := range weeks {
		if w.Value() == week {
			pick = i
		}
	}
	weeks[pick].Selected = true
	return weeks[pick], weeks
}

// venueReport is everything the page, the PDF and the email quote for one venue and
// one week, built by one function so the three cannot disagree.
type venueReport struct {
	Win         reportWindow
	Metrics     *db.SiteMetrics
	Bars        []reportBar
	DeviceWeeks []db.DeviceWeek

	// Per-device averages for the header tiles. Divided by the devices that actually
	// reported, not by the venue's device count, so one never-deployed tablet cannot
	// halve the venue's uptime.
	AvgPoweredMinutes float64
	AvgPluggedMinutes float64
	AvgPluggedPct     int
	AvgPadMinutes     float64
	AvgStandbyMinutes float64
}

// WindowHours is the ceiling the per-device hour tiles are read against.
func (v venueReport) WindowHours() int { return v.Win.Days * 24 }

// buildVenueReport reads a venue's week. visible filters the device rows; nil keeps
// every device, which is what the tokenised PDF link — one venue's own summary —
// shows.
func (h *Handler) buildVenueReport(ctx context.Context, id uuid.UUID, win reportWindow, visible func(uuid.UUID) bool) (venueReport, error) {
	v := venueReport{Win: win}
	if m, err := h.db.SiteMetricsFor(ctx, id, win.Days, win.To); err == nil {
		v.Metrics = &m
	}
	// Bars are drawn per device against a 24-hour day, like the venue page, so a site
	// with more devices does not simply read as taller.
	if daily, err := h.db.SiteMetricsDaily(ctx, id, win.Days, win.To); err == nil {
		for _, x := range daily {
			n := x.Devices
			if n < 1 {
				n = 1
			}
			per := x.PoweredMinutes / float64(n)
			v.Bars = append(v.Bars, reportBar{
				Label: x.Day.Format("Mon"),
				Hours: per / 60,
				Pct:   pctCapped(per, 24*60),
			})
		}
	}
	weeks, err := h.db.RestaurantDeviceWeeks(ctx, id, win.Days, win.To)
	if err != nil {
		return v, err
	}
	for _, x := range weeks {
		if visible == nil || visible(x.DeviceID) {
			v.DeviceWeeks = append(v.DeviceWeeks, x)
		}
	}
	if n := len(v.DeviceWeeks); n > 0 {
		var plugged, powered, pad, standby float64
		standbyDevices := 0
		for _, x := range v.DeviceWeeks {
			plugged += x.PluggedMinutes
			powered += x.PoweredMinutes
			pad += x.PadMinutes
			if x.HasStandby() {
				standby += x.StandbyMinutes()
				standbyDevices++
			}
		}
		v.AvgPluggedMinutes = plugged / float64(n)
		v.AvgPluggedPct = pctCapped(v.AvgPluggedMinutes, float64(win.Days)*24*60)
		v.AvgPoweredMinutes = powered / float64(n)
		v.AvgPadMinutes = pad / float64(n)
		if standbyDevices > 0 {
			v.AvgStandbyMinutes = standby / float64(standbyDevices)
		}
	}
	return v, nil
}

// reportViewerFilter is the device filter for a signed-in viewer, or nil when they
// can see every device — the case the PDF cache can serve.
func (h *Handler) reportViewerFilter(r *http.Request) func(uuid.UUID) bool {
	acc := h.access(r)
	if !acc.hidesDevices() && !acc.hidesDPC() {
		return nil
	}
	return acc.visible
}
