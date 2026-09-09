package dashboard

import (
	"fmt"
	"sort"
	"strings"

	"mdm/internal/db"

	"github.com/google/uuid"
)

// healthReason is one thing pulling the fleet health score down, in fleet points.
// It mirrors GroupHealth.computeScore exactly: every penalty a venue takes is
// scaled by that venue's share of the fleet, which is how the ring's device-
// weighted mean is built. Deterministic, no AI involved.
type healthReason struct {
	Venue   string
	VenueID uuid.UUID
	What    string  // "4 of 9 devices offline"
	Points  float64 // fleet points lost (before rounding)
	Do      string  // what to do about it
	Href    string  // where to go
	Class   string  // bad | warn
}

// PointsLabel renders the loss as "−3.4".
func (r healthReason) PointsLabel() string {
	if r.Points < 0.05 {
		return "−0"
	}
	return fmt.Sprintf("−%.1f", r.Points)
}

// Pct is the reason's size relative to the biggest reason, for the bar widths.
func (r healthReason) Pct(total float64) int {
	if total <= 0 {
		return 0
	}
	return int(r.Points/total*100 + 0.5)
}

// explainFleetHealth breaks the fleet score down into the venue-level penalties
// that produced it. crashes is open crash counts by restaurant (may be nil).
func explainFleetHealth(groups []db.GroupHealth, crashes map[uuid.UUID]int) []healthReason {
	den := 0
	for _, g := range groups {
		den += g.DeviceCount
	}
	if den == 0 {
		return nil
	}
	var out []healthReason
	add := func(g db.GroupHealth, penalty float64, what, do, href, class string) {
		if penalty <= 0 {
			return
		}
		out = append(out, healthReason{
			Venue: g.Name, VenueID: g.GroupID, What: what,
			Points: penalty * float64(g.DeviceCount) / float64(den),
			Do: do, Href: href, Class: class,
		})
	}
	for _, g := range groups {
		if g.DeviceCount == 0 {
			continue
		}
		site := "/restaurants/" + g.GroupID.String()
		if g.OfflineCount > 0 {
			pen := float64(g.OfflineCount) / float64(g.DeviceCount) * 40
			add(g, pen, fmt.Sprintf("%d of %d devices offline", g.OfflineCount, g.DeviceCount),
				"Check power and Wi-Fi on site; send Reboot from Actions once they are back.", site+"?status=offline", "bad")
		}
		if g.OpenCritical > 0 {
			add(g, float64(g.OpenCritical*15), fmt.Sprintf("%d critical alert%s open", g.OpenCritical, plural(g.OpenCritical)),
				"Open the alert; it says what to do. Resolve it when done.", "/alerts?status=open", "bad")
		}
		if g.OpenWarning > 0 {
			add(g, float64(g.OpenWarning*4), fmt.Sprintf("%d warning%s open", g.OpenWarning, plural(g.OpenWarning)),
				"Work through the warnings or resolve the ones that no longer apply.", "/alerts?status=open", "warn")
		}
		if g.ChargingAvg != nil && *g.ChargingAvg < 0.3 {
			add(g, 15, fmt.Sprintf("devices on charge only %d%% of the time", int(*g.ChargingAvg*100+0.5)),
				"Stations are off their pads most of the day. Ask the site to dock them between services.", site, "warn")
		}
		if g.BatteryDelta != nil && *g.BatteryDelta < -10 {
			add(g, 15, fmt.Sprintf("overnight battery peak down %d points vs last week", int(-*g.BatteryDelta+0.5)),
				"Batteries are not filling overnight. Check pads and cables; a unit may need a new battery.", site, "warn")
		}
		if g.TempMax != nil && *g.TempMax >= 45 {
			what := fmt.Sprintf("a device peaked at %.0f°C", *g.TempMax)
			href := site
			if g.TempMaxSerial != nil && *g.TempMaxSerial != "" {
				what = fmt.Sprintf("%s peaked at %.0f°C", *g.TempMaxSerial, *g.TempMax)
				href = "/devices/" + *g.TempMaxSerial
			}
			add(g, 15, what, "Move it out of direct sun or away from the kitchen pass; make sure the pad vents are clear.", href, "warn")
		}
		if g.DistinctBuilds > 1 {
			add(g, float64((g.DistinctBuilds-1)*5), fmt.Sprintf("%d different builds in one venue", g.DistinctBuilds),
				"Bring the stragglers up to the venue's build from Updates.", "/updates", "warn")
		}
		if c := crashes[g.GroupID]; c > 0 {
			// Crashes are not in computeScore, but they are the thing people ask
			// about; list them at zero points so the reason still shows.
			add(g, 0.01, fmt.Sprintf("%d crash%s this week", c, map[bool]string{true: "es", false: ""}[c != 1]),
				"Open the device's History tab for the crash summaries.", site, "warn")
		}
	}
	sort.SliceStable(out, func(i, j int) bool { return out[i].Points > out[j].Points })
	return out
}

// healthExplainText is the plain-text form for the daily digest and emails.
func healthExplainText(score, delta int, reasons []healthReason) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Fleet health %d", score)
	if delta != 0 {
		fmt.Fprintf(&b, " (%+d vs last week)", delta)
	}
	b.WriteString(".")
	if len(reasons) == 0 {
		b.WriteString(" Nothing is pulling the score down.")
		return b.String()
	}
	n := len(reasons)
	if n > 5 {
		n = 5
	}
	for _, r := range reasons[:n] {
		fmt.Fprintf(&b, "\n• %s: %s (%s). %s", r.Venue, r.What, r.PointsLabel(), r.Do)
	}
	if len(reasons) > n {
		fmt.Fprintf(&b, "\n… and %d more.", len(reasons)-n)
	}
	return b.String()
}

// whyScore lists, for one venue or group, the penalties behind its own score
// (venue points, not fleet-weighted), for the "?" popover next to every score.
// Accepts a GroupHealth or a pointer to one; anything else yields nothing.
func whyScore(v any) []string {
	var g db.GroupHealth
	switch x := v.(type) {
	case db.GroupHealth:
		g = x
	case *db.GroupHealth:
		if x == nil {
			return nil
		}
		g = *x
	default:
		return nil
	}
	if g.DeviceCount == 0 {
		return nil
	}
	var out []string
	if g.OfflineCount > 0 {
		out = append(out, fmt.Sprintf("%d of %d offline (−%d)", g.OfflineCount, g.DeviceCount, int(float64(g.OfflineCount)/float64(g.DeviceCount)*40)))
	}
	if g.OpenCritical > 0 {
		out = append(out, fmt.Sprintf("%d critical alert%s (−%d)", g.OpenCritical, plural(g.OpenCritical), g.OpenCritical*15))
	}
	if g.OpenWarning > 0 {
		out = append(out, fmt.Sprintf("%d warning%s (−%d)", g.OpenWarning, plural(g.OpenWarning), g.OpenWarning*4))
	}
	if g.ChargingAvg != nil && *g.ChargingAvg < 0.3 {
		out = append(out, fmt.Sprintf("on charge only %d%% of the time (−15)", int(*g.ChargingAvg*100+0.5)))
	}
	if g.BatteryDelta != nil && *g.BatteryDelta < -10 {
		out = append(out, fmt.Sprintf("overnight battery down %d vs last week (−15)", int(-*g.BatteryDelta+0.5)))
	}
	if g.TempMax != nil && *g.TempMax >= 45 {
		if g.TempMaxSerial != nil && *g.TempMaxSerial != "" {
			out = append(out, fmt.Sprintf("%s peaked at %.0f°C (−15)", *g.TempMaxSerial, *g.TempMax))
		} else {
			out = append(out, fmt.Sprintf("a device peaked at %.0f°C (−15)", *g.TempMax))
		}
	}
	if g.DistinctBuilds > 1 {
		out = append(out, fmt.Sprintf("%d different builds (−%d)", g.DistinctBuilds, (g.DistinctBuilds-1)*5))
	}
	return out
}
