package dashboard

import (
	"encoding/json"
	"fmt"
	"html/template"
	_ "image/gif"
	_ "image/jpeg"
	"net/http"
	"sort"
	"strings"
	"time"

	"mdm/internal/db"
)

// FleetHealth renders the Tier 3 fleet/group health overview: a fleet summary plus a
// per-group scorecard ranked worst-first.
// FleetHealth is the detailed-analysis page: the cached fleet report on top, then —
// per signal category — the actual open alerts that back it (serial linked to the
// device page, fired-at, and the value-rich summary), then the per-group scorecard.
// Every serial/timestamp/value in the evidence comes straight from the alerts table,
// so the page can never cite a device the model invented. The "Detailed analysis"
// button on the hourly-report card points here.
func (h *Handler) FleetHealth(w http.ResponseWriter, r *http.Request) {
	// Scorecard window: 7 days by default, or 1 day (today vs yesterday) via ?days=1.
	windowDays := 7
	if r.URL.Query().Get("days") == "1" {
		windowDays = 1
	}
	if h.access(r).hidesDevices() {
		http.Redirect(w, r, "/devices", http.StatusFound)
		return
	}
	groups, err := h.db.GetRestaurantHealth(r.Context(), h.connectedSlice(), windowDays)
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	summary, _ := h.db.GetSummary(r.Context(), h.connectedSlice())
	openAlerts, _ := h.db.CountOpenAlerts(r.Context(), h.access(r).hidesIdentityAlerts())
	alerts, _ := h.db.ListAlerts(r.Context(), "open", 500)
	serials, _ := h.db.ListAllSerials(r.Context())

	// Fleet score: device-weighted mean of the per-restaurant scores (same basis as
	// the overview ring), so the hero number agrees across pages.
	score := 100
	if summary.Total > 0 {
		num, den := 0, 0
		for _, g := range groups {
			num += g.Score * g.DeviceCount
			den += g.DeviceCount
		}
		if den > 0 {
			score = num / den
		} else {
			// No venue has devices yet (or no venues at all): fall back to the
			// online-ratio proxy so the ring still reflects reality.
			score -= 40 * (summary.Total - summary.RecentlyActive) / summary.Total
		}
	}
	scoreClass := "danger"
	switch {
	case score >= 80:
		scoreClass = "ok"
	case score >= 50:
		scoreClass = "warn"
	}
	// Restaurants needing a look = anything not scoring "ok" (or with a device).
	attention := 0
	for _, g := range groups {
		if g.DeviceCount > 0 && g.ScoreClass != "ok" {
			attention++
		}
	}
	verdict := "All restaurants healthy"
	if attention == 1 {
		verdict = "1 restaurant needs a look"
	} else if attention > 1 {
		verdict = fmt.Sprintf("%d restaurants need a look", attention)
	}

	hot := hotSerialsFromHealth(groups, h.alertThresholds(r.Context()).TempC)
	healthy := 0
	for _, g := range groups {
		if g.DeviceCount > 0 && g.ScoreClass == "ok" {
			healthy++
		}
	}

	// Crash surfaces: 24h rollup (signal tile + hero facts), the worst devices for
	// the crashes panel, a 7-day sparkline, and per-restaurant counts for the chips.
	crashStats, _ := h.db.GetFleetCrashStats(r.Context(), 6)
	crashRows := make([]crashRow, 0, len(crashStats.ByDevice))
	for _, c := range crashStats.ByDevice {
		label, class := crashKindBadge(c.Kind)
		crashRows = append(crashRows, crashRow{
			Serial: c.Serial, Restaurant: c.Restaurant,
			KindLabel: label, KindClass: class,
			BuildID: c.BuildID, Ago: agoShort(c.LatestAt), Count: c.Count,
		})
	}
	// Memory-pressure signal count (open alerts of that type).
	memory := 0
	for _, a := range alerts {
		if a.Type == "memory_pressure" {
			memory++
		}
	}
	// Triage list is worst-first (GetRestaurantHealth returns name order).
	sort.SliceStable(groups, func(i, j int) bool { return groups[i].Score < groups[j].Score })
	statusWord := map[string]string{"ok": "Healthy", "warn": "Watch", "danger": "Critical"}[scoreClass]

	data := map[string]any{
		"Title":           "Fleet Health",
		"Groups":          groups,
		"TotalDevices":    summary.Total,
		"OnlineDevices":   summary.RecentlyActive,
		"OfflineDevices":  summary.Total - summary.RecentlyActive,
		"LowBattery":      summary.LowBattery,
		"HotCount":        len(hot),
		"HealthyCount":    healthy,
		"OpenAlerts":      openAlerts,
		"UniqueBuilds":    summary.UniqueBuilds,
		"Evidence":        groupAlertsBySignal(alerts),
		"OpenAlertsCount": len(alerts),
		"DeviceSerials":   serialsJSON(serials),
		"HotSerials":      serialsJSON(hot),
		"WindowDays":      windowDays,
		"Score":           score,
		"HealthReasons":   explainFleetHealth(groups, crashStats.ByRestaurant),
		"ScoreClass":      scoreClass,
		// 2π·r40 = 251.3; the hero ring animates to this offset.
		"RingOffset": fmt.Sprintf("%.1f", 251.3*float64(100-score)/100),
		"Verdict":    verdict,
		"Attention":  attention,
		"StatusWord": statusWord,
		// Crash surfaces.
		"Crashes24h":        crashStats.Total24h,
		"CrashDevices24h":   crashStats.Devices24h,
		"CrashWorstBuild":   crashStats.WorstBuild,
		"CrashWorstBuildN":  crashStats.WorstBuildN,
		"CrashList":         crashRows,
		"CrashSpark":        crashSparkBars(crashStats.Daily),
		"RestaurantCrashes": crashStats.ByRestaurant,
		"MemoryCount":       memory,
	}
	// Fleet-wide 14-day series backing the report's evidence charts.
	if d14, err := h.db.GetFleetDailyStats(r.Context(), 14); err == nil {
		data["SeriesJSON"] = fleetSeriesJSON(d14, crashStats.Daily, summary.Total)
	}
	// Show the same cached fleet report as the main page (latest of hourly or manual).
	if s, err := h.db.GetAISummary(r.Context(), "fleet"); err == nil && s.Summary != "" {
		data["AISummary"] = s.Summary
		data["AISummaryAt"] = s.GeneratedAt.UTC().Format(time.RFC3339)
		data["AISummaryPreview"] = summarizePreview(s.Summary)
		data["ReportRestaurants"] = restaurantLinksJSON(groups)
	}
	h.render(w, r, "health.html", data)
}

// fleetSeriesJSON packs the 14-day fleet series (active / low battery / hot per
// day, battery average) plus crashes per day for the report evidence charts.
func fleetSeriesJSON(d14 []db.FleetDailyStat, crashes []int, total int) template.JS {
	type pt struct {
		Day    string   `json:"day"`
		Active int      `json:"active"`
		Low    int      `json:"low"`
		Hot    int      `json:"hot"`
		Batt   *float32 `json:"batt"`
	}
	series := make([]pt, 0, len(d14))
	for _, ds := range d14 {
		series = append(series, pt{ds.Day.Format("Jan 2"), ds.Active, ds.LowBattery, ds.Hot, ds.BatteryAvg})
	}
	b, err := json.Marshal(map[string]any{"days": series, "crashes": crashes, "total": total})
	if err != nil {
		return template.JS("null")
	}
	return template.JS(b)
}

// reportSignals maps the fleet report's four signal categories to the alert types
// that evidence them. Order is worst-first display order. Each signal lines up with a
// ReportIssue.Area value, so the narrative issue and the device-level evidence below
// it speak the same vocabulary.
var reportSignals = []struct {
	Key, Label string
	Types      []string
}{
	{"heat", "Overheating", []string{"overheating"}},
	{"memory", "Memory pressure", []string{"memory_pressure"}},
}

// reportEvidence is one signal category plus the open alerts that evidence it. Every
// alert carries its own serial, fired-at, and a number-rich summary, so the template
// can link each unit to its device page with the real values behind the call-out.
type reportEvidence struct {
	Key, Label string
	Alerts     []db.Alert
}

// groupAlertsBySignal buckets the open alerts into the four report signal categories
// for the detailed-analysis evidence tables.
func groupAlertsBySignal(alerts []db.Alert) []reportEvidence {
	evidence := make([]reportEvidence, 0, len(reportSignals))
	for _, sig := range reportSignals {
		var matched []db.Alert
		for _, a := range alerts {
			for _, t := range sig.Types {
				if a.Type == t {
					matched = append(matched, a)
					break
				}
			}
		}
		evidence = append(evidence, reportEvidence{sig.Key, sig.Label, matched})
	}
	return evidence
}

// ── Fleet Health crash presentation ─────────────────────────────────────────

// crashRow is one row of the health page's crashes panel: a device that has been
// crashing, with its latest kind badge, count, build and a relative time.
type crashRow struct {
	Serial     string
	Restaurant string
	KindLabel  string
	KindClass  string
	BuildID    string
	Ago        string
	Count      int
}

// sparkBar is one bar of the hero's 7-day crashes-per-day sparkline.
type sparkBar struct {
	Label string
	Pct   int
	Class string // "hi" (danger) | "md" (warn) | "" — colour by height
}

// crashKindBadge maps a raw DropBox event kind to a short label + CSS class:
// ANR, native tombstone, or everything-else-is-a-crash.
func crashKindBadge(kind string) (label, class string) {
	k := strings.ToLower(kind)
	switch {
	case strings.Contains(k, "anr"):
		return "ANR", "anr"
	case strings.Contains(k, "tombstone") || strings.Contains(k, "native"):
		return "Native", "tomb"
	default:
		return "Crash", "crash"
	}
}

// agoShort renders a compact relative time ("just now", "14m ago", "3h ago").
func agoShort(t time.Time) string {
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return fmt.Sprintf("%dm ago", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh ago", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd ago", int(d.Hours())/24)
	}
}

// absInt returns the absolute value of an int.
func absInt(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

// tileDelta formats a today-vs-yesterday change for an overview signal tile.
// badWhenUp says whether an increase is bad (offline/crashes) or good (online).
// Returns Show=false when there's no change so the tile reads "steady".
func tileDelta(today, yesterday int, badWhenUp bool) map[string]any {
	d := today - yesterday
	if d == 0 {
		return map[string]any{"Show": false}
	}
	up := d > 0
	return map[string]any{"Show": true, "N": absInt(d), "Up": up, "Bad": up == badWhenUp}
}

// crashSparkBars turns 7 daily counts into scaled, colour-coded bars. The tallest
// day sets the scale; empty days keep a small stub so the axis still reads. The
// last bar is labelled "Today", earlier ones by weekday.
func crashSparkBars(daily []int) []sparkBar {
	n := len(daily)
	max := 1
	for _, v := range daily {
		if v > max {
			max = v
		}
	}
	bars := make([]sparkBar, n)
	for i, v := range daily {
		pct := v * 100 / max
		if pct < 6 {
			pct = 6
		}
		class := ""
		if v > 0 {
			switch {
			case float64(v) >= 0.7*float64(max):
				class = "hi"
			case float64(v) >= 0.4*float64(max):
				class = "md"
			}
		}
		label := "Today"
		if i < n-1 {
			label = time.Now().AddDate(0, 0, -(n - 1 - i)).Format("Mon")
		}
		bars[i] = sparkBar{Label: label, Pct: pct, Class: class}
	}
	return bars
}

// hotSerialsFromHealth returns the serial of each restaurant's hottest unit whose
// window max temp reached the overheating threshold. These are exactly the devices a
// heat call-out names, so the report card deep-links their serials straight to the
// spike (?focus=temp) instead of the device's default recent window.
func hotSerialsFromHealth(groups []db.GroupHealth, tempC float64) []string {
	var out []string
	for _, g := range groups {
		if g.TempMax != nil && *g.TempMax >= tempC && g.TempMaxSerial != nil && *g.TempMaxSerial != "" {
			out = append(out, *g.TempMaxSerial)
		}
	}
	return out
}

// peakDayForFocus returns the rolled-up day holding the extreme of the focused metric
// (hottest temp, highest RAM, lowest battery), so the device chart can be centered on
// the incident. ok is false when no day has data for that metric.
func peakDayForFocus(stats []db.DeviceDailyStat, focus string) (time.Time, bool) {
	var best *db.DeviceDailyStat
	for i := range stats {
		s := &stats[i]
		switch focus {
		case "ram":
			if s.RAMPctPeak != nil && (best == nil || *s.RAMPctPeak > *best.RAMPctPeak) {
				best = s
			}
		case "battery":
			if s.BatteryMin != nil && (best == nil || *s.BatteryMin < *best.BatteryMin) {
				best = s
			}
		default: // temp
			if s.TempMax != nil && (best == nil || *s.TempMax > *best.TempMax) {
				best = s
			}
		}
	}
	if best == nil {
		return time.Time{}, false
	}
	return best.Day, true
}

// serialsJSON marshals the device serial list for the report card's client-side
// serial linkifier (it turns any serial named in the report prose into a device link).
func serialsJSON(serials []string) template.JS {
	b, err := json.Marshal(serials)
	if err != nil {
		return template.JS("[]")
	}
	return template.JS(b)
}

// restaurantLinksJSON emits [{"name","id"}] for the restaurants the Daily Report may
// name, so the report card can turn each restaurant name in the prose into a link to
// that venue's page. Only named restaurants (Deployed) are included; longest name
// first so a shorter name can't shadow a longer one when the client linkifies.
func restaurantLinksJSON(groups []db.GroupHealth) template.JS {
	type link struct {
		Name string `json:"name"`
		ID   string `json:"id"`
	}
	var out []link
	for _, g := range groups {
		if g.Name == "" {
			continue
		}
		out = append(out, link{Name: g.Name, ID: g.GroupID.String()})
	}
	sort.Slice(out, func(i, j int) bool { return len(out[i].Name) > len(out[j].Name) })
	b, err := json.Marshal(out)
	if err != nil {
		return template.JS("[]")
	}
	return template.JS(b)
}
