package dashboard

import (
	"fmt"
	"math"
	"net/http"
	"net/url"
	"path"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// Overview C (picked 21 Oct 2026, from the "Check Box" demos): filter pills that scope
// the page to a device type and/or a venue and a window; a banner that is a short to-do
// list (W3); three delta cards (Devices, Stability, Battery); a pill chart of devices by
// type; and a Rollouts timeline of the last seven days. The rail, the venue strip and
// Clean up stay from O2. Rollouts, Clean up and the rail's ring, greeting and bars are
// fleet-wide; everything else follows the filters.

// ovDev is one device as the C builders read it, from the live wall query or the
// sneak-peek sample.
type ovDev struct {
	ID       uuid.UUID
	Class    string
	Venue    *uuid.UUID
	Online   bool
	SeenAt   time.Time
	Battery  int
	HasBatt  bool
	TempC    *float64
	Unplaced bool
}

func ovDevsFromWall(wall []db.WallDevice, connected map[uuid.UUID]bool) []ovDev {
	out := make([]ovDev, 0, len(wall))
	for _, w := range wall {
		out = append(out, ovDev{ID: w.ID, Class: w.Class, Venue: w.RestaurantID, Online: connected[w.ID],
			SeenAt: w.LastSeenAt, Battery: w.BatteryPct, HasBatt: w.HasBattery, TempC: w.TempC,
			Unplaced: w.RestaurantID == nil})
	}
	return out
}

// ── Scope (the filter pills) ────────────────────────────────────────────────

type ovOpt struct {
	Label  string
	Href   string
	Active bool
}

type ovScope struct {
	Class, ClassLabel string
	Venue             *uuid.UUID
	VenueName         string
	Window            string // 24h | 7d | 30d
	WindowLabel       string
	VsLabel           string // "vs yesterday"
	Days              int    // 1, 7 or 30
	On                bool   // a type or a venue is chosen
	Label             string // "Kitchen displays at Harbour Grill"
	Types             []ovOpt
	Venues            []ovOpt
	Windows           []ovOpt
	ClearHref         string
}

func (s ovScope) dbScope() db.OverviewScope { return db.OverviewScope{Class: s.Class, Venue: s.Venue} }

// keeps reports whether a device is inside the scope.
func (s ovScope) keeps(class string, venue *uuid.UUID) bool {
	if s.Class != "" && class != s.Class {
		return false
	}
	if s.Venue != nil && (venue == nil || *venue != *s.Venue) {
		return false
	}
	return true
}

type ovVenueOpt struct {
	ID   uuid.UUID
	Name string
}

// parseOvScope reads ?type=, ?venue= and ?window=. An unknown type or a venue the
// viewer can't see is ignored rather than an error, so an old link still opens.
// base is the page the pills link to: "/" for the Overview, "/sneak-peek" for the preview.
func parseOvScope(r *http.Request, base string, classes []string, venues []ovVenueOpt) ovScope {
	q := r.URL.Query()
	sc := ovScope{Window: "24h"}
	if t := q.Get("type"); t != "" {
		for _, c := range classes {
			if c == t {
				sc.Class, sc.ClassLabel = c, product.ClassLabel(c)
			}
		}
	}
	if v := q.Get("venue"); v != "" {
		if id, err := uuid.Parse(v); err == nil {
			for _, o := range venues {
				if o.ID == id {
					id := id
					sc.Venue, sc.VenueName = &id, o.Name
				}
			}
		}
	}
	switch q.Get("window") {
	case "7d":
		sc.Window = "7d"
	case "30d":
		sc.Window = "30d"
	}
	sc.Days, sc.WindowLabel, sc.VsLabel = 1, "24 h", "vs yesterday"
	switch sc.Window {
	case "7d":
		sc.Days, sc.WindowLabel, sc.VsLabel = 7, "7 days", "vs the week before"
	case "30d":
		sc.Days, sc.WindowLabel, sc.VsLabel = 30, "30 days", "vs the 30 days before"
	}
	sc.On = sc.Class != "" || sc.Venue != nil
	switch {
	case sc.Class != "" && sc.Venue != nil:
		sc.Label = pluralClass(sc.ClassLabel) + " at " + sc.VenueName
	case sc.Class != "":
		sc.Label = pluralClass(sc.ClassLabel)
	case sc.Venue != nil:
		sc.Label = sc.VenueName
	}

	venueID := ""
	if sc.Venue != nil {
		venueID = sc.Venue.String()
	}
	href := func(class, venue, window string) string {
		v := url.Values{}
		if class != "" {
			v.Set("type", class)
		}
		if venue != "" {
			v.Set("venue", venue)
		}
		if window != "" && window != "24h" {
			v.Set("window", window)
		}
		if len(v) == 0 {
			return base
		}
		return base + "?" + v.Encode()
	}
	sc.Types = append(sc.Types, ovOpt{"All types", href("", venueID, sc.Window), sc.Class == ""})
	for _, c := range classes {
		sc.Types = append(sc.Types, ovOpt{product.ClassLabel(c), href(c, venueID, sc.Window), c == sc.Class})
	}
	sc.Venues = append(sc.Venues, ovOpt{"All venues", href(sc.Class, "", sc.Window), sc.Venue == nil})
	for _, o := range venues {
		sc.Venues = append(sc.Venues, ovOpt{o.Name, href(sc.Class, o.ID.String(), sc.Window), venueID == o.ID.String()})
	}
	for _, w := range [][2]string{{"24h", "Last 24 hours"}, {"7d", "Last 7 days"}, {"30d", "Last 30 days"}} {
		sc.Windows = append(sc.Windows, ovOpt{w[1], href(sc.Class, venueID, w[0]), w[0] == sc.Window})
	}
	sc.ClearHref = href("", "", sc.Window)
	return sc
}

// pluralClass: "Kitchen display" → "Kitchen displays", "Tableside AI" stays.
func pluralClass(label string) string {
	switch {
	case label == "" || strings.HasSuffix(label, "AI") || strings.HasSuffix(label, "s"):
		return label
	case strings.HasSuffix(label, "box"):
		return label + "es"
	}
	return label + "s"
}

// ── Delta cards ─────────────────────────────────────────────────────────────

type ovMetric struct {
	Value string
	Label string
	Delta string // "▲ 1.2 pts"; "" hides it
	Tone  string // good | bad | flat
}

type ovStat struct {
	Title  string
	Href   string
	A, B   ovMetric
	LineA  string // SVG path, viewBox 0 0 300 58
	LineB  string
	Fill   string
	Empty  string // shown instead of the numbers (e.g. no battery devices in scope)
	Links  []ovOpt
}

// delta formats a change. badWhenUp flips the colours (crashes up is bad).
func ovDelta(d float64, unit string, badWhenUp bool, decimals int) (string, string) {
	if math.Abs(d) >= 10 {
		decimals = 0 // "▲ 33 pts", not "▲ 33.3 pts"
	}
	r := math.Pow(10, float64(decimals))
	d = math.Round(d*r) / r
	if d == 0 {
		return "", "flat"
	}
	arrow, up := "▲", d > 0
	if !up {
		arrow = "▼"
	}
	num := fmt.Sprintf("%.*f", decimals, math.Abs(d))
	tone := "good"
	if up == badWhenUp {
		tone = "bad"
	}
	if unit == "pts" && num == "1" {
		unit = "pt"
	}
	if unit != "" {
		num += " " + unit
	}
	return arrow + " " + num, tone
}

// windows splits the trend into the current window and the one before it (each `n`
// days), returning per-day series for the current one plus the spark span.
func ovWindows(days []db.ScopedDay, n int) (cur, prev []db.ScopedDay) {
	if len(days) >= n {
		cur = days[len(days)-n:]
	}
	if len(days) >= 2*n {
		prev = days[len(days)-2*n : len(days)-n]
	}
	return
}

func ovAvg(ds []db.ScopedDay, f func(db.ScopedDay) float64) (float64, bool) {
	if len(ds) == 0 {
		return 0, false
	}
	t := 0.0
	for _, d := range ds {
		t += f(d)
	}
	return t / float64(len(ds)), true
}

func ovSum(ds []db.ScopedDay, f func(db.ScopedDay) int) int {
	t := 0
	for _, d := range ds {
		t += f(d)
	}
	return t
}

// ovSparkDays is how many days the card lines show for a window.
func ovSparkDays(window string) int {
	switch window {
	case "7d":
		return 14
	case "30d":
		return 30
	}
	return 7
}

// ovTrendDays is how many days to fetch: the spark span, and two windows to compare.
func ovTrendDays(window string) int {
	switch window {
	case "7d":
		return 14
	case "30d":
		return 60
	}
	return 8
}

// ovLines draws two series on one scale as SVG paths for a 300×58 box, plus the area
// under the first.
func ovLines(a, b []float64) (pa, pb, fill string) {
	if len(a) < 2 {
		return "", "", ""
	}
	lo, hi := math.Inf(1), math.Inf(-1)
	for _, s := range [][]float64{a, b} {
		for _, v := range s {
			lo, hi = math.Min(lo, v), math.Max(hi, v)
		}
	}
	const w, h, pad = 300.0, 58.0, 6.0
	y := func(v float64) float64 {
		if hi == lo {
			return h / 2
		}
		return h - pad - (v-lo)/(hi-lo)*(h-2*pad-6)
	}
	line := func(s []float64) string {
		if len(s) < 2 {
			return ""
		}
		var sb strings.Builder
		step := w / float64(len(s)-1)
		for i, v := range s {
			c := "L"
			if i == 0 {
				c = "M"
			}
			fmt.Fprintf(&sb, "%s%.1f %.1f ", c, float64(i)*step, y(v))
		}
		return strings.TrimSpace(sb.String())
	}
	pa, pb = line(a), line(b)
	if pa != "" {
		fill = pa + fmt.Sprintf(" L%.0f %.0f L0 %.0f Z", w, h, h)
	}
	return
}

func ovSeries(ds []db.ScopedDay, f func(db.ScopedDay) float64) []float64 {
	out := make([]float64, len(ds))
	for i, d := range ds {
		out[i] = f(d)
	}
	return out
}

// buildOvStats builds the Devices, Stability and Battery cards for the scope.
func buildOvStats(tr db.ScopedTrend, devs []ovDev, sc ovScope, now time.Time) []ovStat {
	total, online, hot, low, battN, battSum := 0, 0, 0, 0, 0, 0
	for _, d := range devs {
		if !sc.keeps(d.Class, d.Venue) {
			continue
		}
		total++
		if d.Online {
			online++
		}
		if d.TempC != nil && *d.TempC >= 45 && now.Sub(d.SeenAt) < 24*time.Hour {
			hot++
		}
		// Battery: devices that have one and reported in the last day, so a unit in a
		// drawer for a month doesn't count as low today.
		if d.HasBatt && now.Sub(d.SeenAt) < 24*time.Hour {
			battN++
			battSum += d.Battery
			if d.Battery < 20 {
				low++
			}
		}
	}
	cur, prev := ovWindows(tr.Days, sc.Days)
	spark := tr.Days
	if n := ovSparkDays(sc.Window); len(spark) > n {
		spark = spark[len(spark)-n:]
	}
	// Links carry the scope to the Fleet list: /devices?class=…&restaurant=…&<extra>.
	fleet := func(k, v string) string {
		u := url.Values{}
		if sc.Class != "" {
			u.Set("class", sc.Class)
		}
		if sc.Venue != nil {
			u.Set("restaurant", sc.Venue.String())
		}
		if k != "" {
			u.Set(k, v)
		}
		if len(u) == 0 {
			return "/devices"
		}
		return "/devices?" + u.Encode()
	}

	// Devices.
	dev := ovStat{Title: "Devices", Href: fleet("", ""),
		Links: []ovOpt{{Label: "Open Fleet", Href: fleet("", "")}, {Label: "Offline devices", Href: fleet("status", "offline")}}}
	pct := 0.0
	if total > 0 {
		pct = float64(online) * 100 / float64(total)
	}
	dev.A = ovMetric{Value: fmt.Sprintf("%.0f%%", pct), Label: "online · " + sc.VsLabel, Tone: "flat"}
	dev.B = ovMetric{Value: fmt.Sprintf("%d", total-online), Label: "offline now", Tone: "flat"}
	ac, okc := ovAvg(cur, func(d db.ScopedDay) float64 { return float64(d.Active) })
	ap, okp := ovAvg(prev, func(d db.ScopedDay) float64 { return float64(d.Active) })
	if okc && okp && total > 0 {
		dev.A.Delta, dev.A.Tone = ovDelta((ac-ap)*100/float64(total), "pts", false, 1)
		dev.B.Delta, dev.B.Tone = ovDelta(ap-ac, "", true, 0)
	}
	dev.LineA, dev.LineB, dev.Fill = ovLines(
		ovSeries(spark, func(d db.ScopedDay) float64 { return float64(d.Active) }),
		ovSeries(spark, func(d db.ScopedDay) float64 { return math.Max(0, float64(total-d.Active)) }))

	// Stability.
	stab := ovStat{Title: "Stability", Href: "/fleet-health",
		Links: []ovOpt{{Label: "Fleet health", Href: "/fleet-health"}, {Label: "Hot devices", Href: fleet("temp", "hot")}}}
	crashes, crashesPrev := tr.Crashes24h, tr.CrashesPrev
	if sc.Days > 1 {
		crashes = ovSum(cur, func(d db.ScopedDay) int { return d.Crashes })
		crashesPrev = ovSum(prev, func(d db.ScopedDay) int { return d.Crashes })
	}
	stab.A = ovMetric{Value: fmt.Sprintf("%d", crashes), Label: "crashes · " + sc.WindowLabel, Tone: "flat"}
	if sc.Days == 1 || len(prev) > 0 {
		stab.A.Delta, stab.A.Tone = ovDelta(float64(crashes-crashesPrev), "", true, 0)
	}
	stab.B = ovMetric{Value: fmt.Sprintf("%d", hot), Label: "hot devices now", Tone: "flat"}
	hc, okc := ovAvg(cur, func(d db.ScopedDay) float64 { return float64(d.Hot) })
	hp, okp := ovAvg(prev, func(d db.ScopedDay) float64 { return float64(d.Hot) })
	if okc && okp {
		stab.B.Delta, stab.B.Tone = ovDelta(hc-hp, "", true, 0)
	}
	stab.LineA, stab.LineB, stab.Fill = ovLines(
		ovSeries(spark, func(d db.ScopedDay) float64 { return float64(d.Crashes) }),
		ovSeries(spark, func(d db.ScopedDay) float64 { return float64(d.Hot) }))

	// Battery.
	bat := ovStat{Title: "Battery", Href: fleet("battery", "low"),
		Links: []ovOpt{{Label: "Low battery", Href: fleet("battery", "low")}, {Label: "Battery wear", Href: "/battery"}}}
	if battN == 0 {
		bat.Empty = "No battery devices here: these run on mains power."
	} else {
		bat.A = ovMetric{Value: fmt.Sprintf("%d", low), Label: "below 20%", Tone: "flat"}
		bat.B = ovMetric{Value: fmt.Sprintf("%d%%", battSum/battN), Label: "average charge", Tone: "flat"}
		lc, okc := ovAvg(cur, func(d db.ScopedDay) float64 { return float64(d.LowBattery) })
		lp, okp := ovAvg(prev, func(d db.ScopedDay) float64 { return float64(d.LowBattery) })
		if okc && okp {
			bat.A.Delta, bat.A.Tone = ovDelta(lc-lp, "", true, 0)
		}
		avg := func(d db.ScopedDay) float64 {
			if d.BatteryAvg == nil {
				return 0
			}
			return float64(*d.BatteryAvg)
		}
		bc, okc := ovAvg(cur, avg)
		bp, okp := ovAvg(prev, avg)
		if okc && okp && bc > 0 && bp > 0 {
			bat.B.Delta, bat.B.Tone = ovDelta(bc-bp, "pts", false, 0)
		}
		bat.LineA, bat.LineB, bat.Fill = ovLines(ovSeries(spark, avg),
			ovSeries(spark, func(d db.ScopedDay) float64 { return float64(d.LowBattery) }))
	}
	return []ovStat{dev, stab, bat}
}

// ── Pill chart: devices by type ─────────────────────────────────────────────

type ovPill struct {
	Class, Short, Label string
	Online, Offline     int
	Alerts              int // devices with an open warning or critical alert
	HOn, HOff, HAl      int // pill heights, px
	Href                string
}

// ovPillShort is the column label under each pill stack.
func ovPillShort(class string) string {
	switch class {
	case product.ClassT7:
		return "TSAI"
	case product.ClassKiosk:
		return "Kiosk"
	case product.ClassDongle:
		return "Menu"
	case "kds":
		return "KDS"
	case "pos":
		return "POS"
	case "mpos":
		return "Hand"
	case "payment":
		return "Pay"
	case "tablet":
		return "Tablet"
	}
	l := product.ClassLabel(class)
	if i := strings.IndexByte(l, ' '); i > 0 {
		return l[:i]
	}
	return l
}

// ovPillChart is the card: a column per type and the total the footer shows.
type ovPillChart struct {
	Cols  []ovPill
	Total int
}

func ovPillH(n int) int { return int(math.Round(26 + math.Sqrt(float64(n))*4.5)) }

// buildOvPills counts each device type inside the scope. Devices with no type are
// left out, as on O2's chips (20 Oct, user).
func buildOvPills(devs []ovDev, alerts []db.Alert, sc ovScope) ovPillChart {
	alerted := map[uuid.UUID]bool{}
	for _, a := range alerts {
		if a.DeviceID != nil && (a.Severity == "critical" || a.Severity == "warning") {
			alerted[*a.DeviceID] = true
		}
	}
	by := map[string]*ovPill{}
	for _, d := range devs {
		if d.Class == "" || !sc.keeps(d.Class, d.Venue) {
			continue
		}
		p := by[d.Class]
		if p == nil {
			href := "/devices?class=" + url.QueryEscape(d.Class)
			if sc.Venue != nil {
				href += "&restaurant=" + sc.Venue.String()
			}
			p = &ovPill{Class: d.Class, Short: ovPillShort(d.Class), Label: product.ClassLabel(d.Class), Href: href}
			by[d.Class] = p
		}
		if d.Online {
			p.Online++
		} else {
			p.Offline++
		}
		if alerted[d.ID] {
			p.Alerts++
		}
	}
	var out ovPillChart
	for _, c := range product.Classes() {
		if p := by[c]; p != nil {
			p.HOn, p.HOff, p.HAl = ovPillH(p.Online), ovPillH(p.Offline), ovPillH(p.Alerts)
			out.Cols = append(out.Cols, *p)
			out.Total += p.Online + p.Offline
		}
	}
	// Biggest type first, as the fleet reads it (TSAI before the handful of POS).
	sort.SliceStable(out.Cols, func(i, j int) bool {
		return out.Cols[i].Online+out.Cols[i].Offline > out.Cols[j].Online+out.Cols[j].Offline
	})
	return out
}

// ── Rollouts timeline ───────────────────────────────────────────────────────

type ovBar struct {
	Name, Sub, Icon string
	Tone            string // ok | run | warn | q
	Left, Width     string // percent of the 7-day track
	Count           string // "131 / 160"
	Href, Title     string
	Initials        string // who started it, "" for the auto-update policy and the system
	Hue             int
}

type ovRollouts struct {
	Bars    []ovBar
	Days    []string // axis labels, oldest first, the last is "Today"
	NowPct  string
	Running int
	Stalled int
	Devices int
}

const ovRolloutRows = 6

func ovWho(by string) (label, initials string, hue int) {
	switch strings.TrimSpace(by) {
	case "auto-update":
		return "Auto-update", "", 0
	case "", "API key", "system":
		return "System", "", 0
	}
	name := by
	if i := strings.IndexByte(name, '@'); i > 0 {
		name = name[:i]
	}
	name = strings.NewReplacer(".", " ", "_", " ").Replace(name)
	fs := strings.Fields(name)
	if len(fs) == 0 {
		return by, "", 0
	}
	label = strings.ToUpper(fs[0][:1]) + fs[0][1:]
	initials = strings.ToUpper(fs[0][:1])
	if len(fs) > 1 {
		initials += strings.ToUpper(fs[1][:1])
	}
	h := 0
	for _, c := range by {
		h = (h*31 + int(c)) % 360
	}
	return label, initials, h
}

func ovRolloutName(r db.Rollout) (name, icon string) {
	switch r.Kind {
	case "ota":
		return "Firmware " + r.Name, "FW"
	case "app_update":
		switch r.Package {
		case "com.aioapp.mdm":
			return "Firmware MDM " + r.Name, "CL"
		case "aio.app.mdmclient.dpc":
			return "Standard client " + r.Name, "SC"
		}
		if r.Package != "" {
			return r.Package + " " + r.Name, "AP"
		}
		return "App update " + r.Name, "AP"
	case "set_kiosk", "kiosk_set":
		if r.Package != "" {
			return "Lock · " + r.Package, "LK"
		}
		return "Lock", "LK"
	}
	n := r.Name
	if strings.Contains(n, "/") {
		n = path.Base(n)
	}
	return n, "AP"
}

// buildOvRollouts lays the last seven days' rollouts on a Wed…Today track. Running and
// stalled ones are kept first when there are more than six.
func buildOvRollouts(rs []db.Rollout, now time.Time) ovRollouts {
	var out ovRollouts
	dayStart := time.Date(now.Year(), now.Month(), now.Day(), 0, 0, 0, 0, now.Location())
	start := dayStart.AddDate(0, 0, -6)
	end := dayStart.AddDate(0, 0, 1)
	span := end.Sub(start).Seconds()
	for i := 0; i < 7; i++ {
		d := start.AddDate(0, 0, i)
		l := d.Format("Mon 2")
		if i == 6 {
			l = "Today"
		}
		out.Days = append(out.Days, l)
	}
	pos := func(t time.Time) float64 {
		p := t.Sub(start).Seconds() / span * 100
		return math.Max(0, math.Min(100, p))
	}
	out.NowPct = fmt.Sprintf("%.2f", pos(now))

	type cand struct {
		bar  ovBar
		live bool
		at   time.Time
	}
	var cs []cand
	for _, r := range rs {
		// A deployment with no devices is an empty shell (three from 16 Sep still sat
		// "pending" with none), and a cancelled one that installed nothing did nothing.
		if r.Total == 0 || (r.Kind == "ota" && r.Status == "cancelled" && r.Done == 0) {
			continue
		}
		b := ovBar{}
		b.Name, b.Icon = ovRolloutName(r)
		who, ini, hue := ovWho(r.Who)
		b.Initials, b.Hue = ini, hue
		b.Sub = fmt.Sprintf("%s · %d device%s", who, r.Total, sIfN(r.Total))
		b.Count = fmt.Sprintf("%d / %d", r.Done, r.Total)
		b.Href = ovRolloutHref(r)
		from, to := r.Start, r.End
		live := r.InFlight > 0 // devices still on their way, not just a status never closed
		switch {
		case r.Kind == "ota" && r.Status == "pending" && r.Scheduled != nil && r.Scheduled.After(now):
			b.Tone, from, to = "q", now, *r.Scheduled
			b.Sub = fmt.Sprintf("%s · starts %s", who, r.Scheduled.Format("Mon 15:04"))
		case live:
			b.Tone, to = "run", now
			if r.Failed > 0 && r.Failed*10 >= r.Total {
				b.Tone = "warn"
			}
		case r.Done < r.Total && r.Failed > 0:
			b.Tone = "warn"
		default:
			b.Tone = "ok"
		}
		if to.Before(start) {
			continue
		}
		// A short bar still gets room for its icon and count: CSS gives it a minimum
		// width and keeps it inside the track (left: min(…, 100% − that width)).
		l, rr := pos(from), pos(to)
		b.Left, b.Width = fmt.Sprintf("%.2f", l), fmt.Sprintf("%.2f", rr-l)
		b.Title = fmt.Sprintf("%s · %s done", b.Name, b.Count)
		switch b.Tone {
		case "run":
			out.Running++
		case "warn":
			out.Stalled++
		}
		cs = append(cs, cand{b, b.Tone == "run" || b.Tone == "warn" || b.Tone == "q", r.Start})
		out.Devices += r.Total
	}
	// Keep the live ones, then the newest, then lay them out oldest first.
	sort.SliceStable(cs, func(i, j int) bool {
		if cs[i].live != cs[j].live {
			return cs[i].live
		}
		return cs[i].at.After(cs[j].at)
	})
	if len(cs) > ovRolloutRows {
		cs = cs[:ovRolloutRows]
	}
	sort.SliceStable(cs, func(i, j int) bool { return cs[i].at.Before(cs[j].at) })
	for _, c := range cs {
		out.Bars = append(out.Bars, c.bar)
	}
	return out
}

// ovRolloutHref opens the rollout itself: the deployment, or the job when it was one
// command. A rollout of many commands (the client policy queues one per device) has no
// page of its own, so it opens where it is run from: Clients, Lock, or the job history.
func ovRolloutHref(r db.Rollout) string {
	switch {
	case r.Kind == "ota" && r.ReleaseID > 0:
		return fmt.Sprintf("/releases/%d/deployments/%d", r.ReleaseID, r.DeploymentID)
	case r.Kind == "ota":
		return "/releases"
	case r.Commands == 1 && r.CommandID != uuid.Nil:
		return "/commands/" + r.CommandID.String()
	case r.Kind == "app_update" && r.Who == "auto-update":
		return "/clients"
	case r.Kind == "set_kiosk" || r.Kind == "kiosk_set":
		return "/lock"
	}
	return "/jobs/history"
}

func sIfN(n int) string {
	if n == 1 {
		return ""
	}
	return "s"
}

// ── Banner: a short to-do list (W3) ─────────────────────────────────────────

type ovTodo struct {
	Title, Sub   string
	Action, Href string
}

type ovBanner struct {
	Tone     string // bad | warn | calm
	Headline string
	Todos    []ovTodo // at most three
	More     int      // items past the three, in Needs attention
	Calm     string   // the line under "Everything is running"
}

const ovTodoRows = 3

// buildOvBanner orders what needs doing: critical alerts, the worst venue, a crash
// spike, warnings, then devices waiting for a venue. The headline counts every item,
// the rows show the first three, and "N more" says where the rest are.
func buildOvBanner(att []attentionRow, attTotal int, groups []db.GroupHealth, crashByVenue map[uuid.UUID]int,
	tr db.ScopedTrend, sc ovScope, online, total int) ovBanner {
	var crit, warn, info []ovTodo
	critVenues := map[string]bool{}
	alertTodo := func(a attentionRow) ovTodo {
		var bits []string
		if a.Serial != "" {
			bits = append(bits, a.Serial)
		} else if a.Devices > 0 {
			bits = append(bits, fmt.Sprintf("%d device%s", a.Devices, sIfN(a.Devices)))
		}
		if a.Restaurant != "" {
			bits = append(bits, a.Restaurant)
		}
		if !a.Since.IsZero() {
			bits = append(bits, "since "+agoShort(a.Since))
		}
		return ovTodo{Title: a.Issue, Sub: strings.Join(bits, " · "), Action: "Open", Href: a.Href}
	}
	for _, a := range att {
		switch a.Severity {
		case "critical":
			crit = append(crit, alertTodo(a))
			critVenues[a.Restaurant] = true
		case "warning":
			warn = append(warn, alertTodo(a))
		default:
			t := alertTodo(a)
			if a.Issue == "Waiting for a restaurant" {
				t = ovTodo{Title: fmt.Sprintf("%d new device%s waiting for a venue", a.Devices, sIfN(a.Devices)),
					Sub: "no lock rule, service hours or venue alerts until placed", Action: "Assign",
					Href: "/devices?onboarding=pending"}
			}
			info = append(info, t)
		}
	}
	// Rows past the eight Needs attention keeps are still items.
	hidden := attTotal - len(att)
	if hidden < 0 {
		hidden = 0
	}

	var venue []ovTodo
	for _, g := range groups { // worst first
		if g.DeviceCount == 0 || g.ScoreClass == "ok" || critVenues[g.Name] {
			continue
		}
		if sc.Venue != nil && g.GroupID != *sc.Venue {
			continue
		}
		var bits []string
		if g.OfflineCount > 0 {
			bits = append(bits, fmt.Sprintf("%d offline", g.OfflineCount))
		}
		if g.TempMax != nil && *g.TempMax >= 45 {
			bits = append(bits, "a unit running hot")
		}
		if c := crashByVenue[g.GroupID]; c > 0 {
			bits = append(bits, fmt.Sprintf("%d crash%s today", c, map[bool]string{true: "", false: "es"}[c == 1]))
		}
		if len(bits) == 0 {
			continue
		}
		word := "At risk"
		if g.ScoreClass == "warn" {
			word = "Watch"
		}
		venue = append(venue, ovTodo{Title: g.Name + ": " + strings.Join(bits, ", "),
			Sub:    fmt.Sprintf("%s · health %d · %d of %d online", word, g.Score, g.DeviceCount-g.OfflineCount, g.DeviceCount),
			Action: "Open venue", Href: "/restaurants/" + g.GroupID.String()})
		break
	}

	var spike []ovTodo
	if tr.Crashes24h >= 5 && tr.Crashes24h > tr.CrashesPrev {
		spike = append(spike, ovTodo{Title: fmt.Sprintf("%d crashes in the last 24 h", tr.Crashes24h),
			Sub: fmt.Sprintf("up from %d the day before", tr.CrashesPrev), Action: "See crashes", Href: "/fleet-health"})
	}

	all := append(append(append(append(crit, venue...), spike...), warn...), info...)
	b := ovBanner{Tone: "calm"}
	n := len(all) + hidden
	switch {
	case len(crit) > 0:
		b.Tone = "bad"
	case n > 0:
		b.Tone = "warn"
	}
	if n == 0 {
		b.Headline = "Everything is running"
		where := ""
		if sc.On {
			where = " for " + sc.Label
		}
		b.Calm = fmt.Sprintf("%d of %d devices online%s. Nothing needs you right now.", online, total, where)
		return b
	}
	thing := "things"
	if n == 1 {
		thing = "thing"
	}
	b.Headline = fmt.Sprintf("%d %s to look at today", n, thing)
	if len(all) > ovTodoRows {
		all = all[:ovTodoRows]
	}
	b.Todos = all
	b.More = n - len(all)
	return b
}

// ovScopeAlerts keeps the alerts inside the scope. An alert on a device the wall
// doesn't list (hidden, retired) is dropped when a scope is set.
func ovScopeAlerts(alerts []db.Alert, devs []ovDev, sc ovScope) []db.Alert {
	if !sc.On {
		return alerts
	}
	where := make(map[uuid.UUID]ovDev, len(devs))
	for _, d := range devs {
		where[d.ID] = d
	}
	var out []db.Alert
	for _, a := range alerts {
		if a.DeviceID == nil {
			if sc.Class == "" && sc.Venue != nil && a.RestaurantID == sc.Venue.String() {
				out = append(out, a)
			}
			continue
		}
		if d, ok := where[*a.DeviceID]; ok && sc.keeps(d.Class, d.Venue) {
			out = append(out, a)
		}
	}
	return out
}

// ovScopeInbox is how many devices wait for a venue inside the scope: none when a
// venue is chosen, those of the type when a type is.
func ovScopeInbox(inboxN int, devs []ovDev, sc ovScope) int {
	switch {
	case sc.Venue != nil:
		return 0
	case sc.Class != "":
		n := 0
		for _, d := range devs {
			if d.Unplaced && d.Class == sc.Class {
				n++
			}
		}
		if n > inboxN {
			n = inboxN
		}
		return n
	}
	return inboxN
}
