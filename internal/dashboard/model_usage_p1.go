package dashboard

import (
	"fmt"
	"math"
	"net/url"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// P1 (picked 23 Oct 2026): /apps/usage in the Overview's language. Pills scope the page
// (Model · Venue · Window · Measure); the banner is a short to-do (role app stopping,
// apps over the limit, a device far above its model, apps holding memory in the
// background); three delta cards against the window before; the top apps beside every
// model on the same measure; the full table; a rail with a ring, seven days and the
// heaviest devices.

type p1In struct {
	Now                      time.Time
	Win                      time.Duration
	Since, WeekSince         time.Time
	Model, ModelLabel        string
	Venue, VenueName         string
	Window, Measure          string
	Venues                   []muVenue
	ByID                     map[uuid.UUID]db.Device // every device in the venue scope
	All                      []db.DeviceAppUsage     // their usage in the window
	Prev                     []db.DeviceAppUsage     // the model's, the window before
	Series, PrevSeries, Week []db.UsagePoint         // the model's
	Labels                   map[string]string
	RoleBase                 string
}

type p1Top struct {
	Name, Href, Value string
	Share, Pct        int
	Tone              string // role | bad | ""
}

type p1Model struct {
	Label, Href        string
	Devices, Reporting int
	Value, Biggest     string
	On                 bool
}

type p1Heavy struct {
	Serial, Venue, Value, Note, Href string
	Tone                             string // danger | warn | ""
}

type p1Ring struct {
	Pct, Offset, Label, Tone string
	Title                    string
}

type modelUsageP1 struct {
	Model, ModelLabel                    string
	VenueName, WindowLabel, MeasureLabel string
	Models, Venues, Windows, Measures    []ovOpt
	Lbl                                  string
	Devices, Reporting                   int
	Banner                               a1Banner
	Cards                                []ovStat
	Top                                  []p1Top
	TopTitle                             string
	ModelRows                            []p1Model
	Rows                                 []muRow
	More                                 int
	Full                                 bool // memory and CPU are measured here
	Battery                              bool
	RoleBase                             string
	Ring                                 p1Ring
	HiTitle, HiSub, HiDelta, HiTone      string
	Bars                                 []ovRailBar
	BarsCap                              string
	Heavy                                []p1Heavy
	Empty                                bool
}

var p1Measures = []struct{ Key, Label, Noun string }{
	{"mem", "Memory", "memory"},
	{"cpu", "CPU", "CPU"},
	{"screen", "Screen time", "screen time"},
	{"net", "Network", "network"},
	{"battery", "Battery", "battery"},
}

func p1MeasureLabel(k string) (string, string) {
	for _, m := range p1Measures {
		if m.Key == k {
			return m.Label, m.Noun
		}
	}
	return k, k
}

var p1WindowLabels = map[string]string{"24h": "24 hours", "7d": "7 days", "30d": "30 days"}

// p1DefaultModel keeps a model that has devices in scope; otherwise it picks the one with
// the most devices reporting, then the most devices.
func p1DefaultModel(want string, byID map[uuid.UUID]db.Device, usage []db.DeviceAppUsage) string {
	count, rep := map[string]int{}, map[string]int{}
	for _, d := range byID {
		count[d.ProductKey()]++
	}
	if _, ok := count[want]; ok {
		return want
	}
	seen := map[uuid.UUID]bool{}
	for _, u := range usage {
		if d, ok := byID[u.DeviceID]; ok && !seen[u.DeviceID] {
			seen[u.DeviceID] = true
			rep[d.ProductKey()]++
		}
	}
	best := ""
	for k := range count {
		if best == "" || rep[k] > rep[best] || (rep[k] == rep[best] && (count[k] > count[best] || (count[k] == count[best] && k < best))) {
			best = k
		}
	}
	return best
}

// p1PerDevice is each device's total on the measure (summed over its apps); ok=false for a
// device that reported nothing measurable on it.
func p1PerDevice(measure string, usage []db.DeviceAppUsage) map[uuid.UUID]float64 {
	out := map[uuid.UUID]float64{}
	for _, u := range usage {
		if v, ok := muDeviceValue(measure, u); ok {
			out[u.DeviceID] += v
		}
	}
	return out
}

func p1Mean(m map[uuid.UUID]float64) (float64, bool) {
	if len(m) == 0 {
		return 0, false
	}
	t := 0.0
	for _, v := range m {
		t += v
	}
	return t / float64(len(m)), true
}

// p1Slots buckets a series into n equal slots from `since`, as the per-device measure.
// Memory and CPU average the device-buckets in a slot (and carry the last value over an
// empty one); the rest add up and divide by the devices reporting.
func p1Slots(measure string, pts []db.UsagePoint, since time.Time, win time.Duration, n int) []float64 {
	v, _ := p1SlotsHas(measure, pts, since, win, n)
	return v
}

// p1SlotsHas is p1Slots plus which slots had any reading.
func p1SlotsHas(measure string, pts []db.UsagePoint, since time.Time, win time.Duration, n int) ([]float64, []bool) {
	sum := make([]float64, n)
	cnt := make([]int, n)
	devs := map[uuid.UUID]bool{}
	step := win / time.Duration(n)
	for _, p := range pts {
		i := int(p.Bucket.Sub(since) / step)
		if i < 0 {
			i = 0
		}
		if i >= n {
			i = n - 1
		}
		var v float64
		switch measure {
		case "mem":
			if !p.HasPss {
				continue
			}
			v = p.PssMB
		case "cpu":
			if !p.HasCPU {
				continue
			}
			v = p.CPUPct
		case "screen":
			v = float64(p.FgMs)
		case "net":
			v = float64(p.NetBytes)
		case "battery":
			v = p.PowerMAh
		}
		devs[p.DeviceID] = true
		sum[i] += v
		cnt[i]++
	}
	out := make([]float64, n)
	last := math.NaN()
	for i := range out {
		switch measure {
		case "mem", "cpu":
			if cnt[i] > 0 {
				last = sum[i] / float64(cnt[i])
			}
			if math.IsNaN(last) {
				continue
			}
			out[i] = last
		default:
			if len(devs) > 0 {
				out[i] = sum[i] / float64(len(devs))
			}
		}
	}
	has := make([]bool, n)
	for i := range has {
		has[i] = cnt[i] > 0
	}
	return out, has
}

// p1Trim drops the slots before either window has its first reading, so a model that
// started reporting this morning draws from this morning, not a flat zero before it. A
// window before with no readings at all draws no line.
func p1Trim(a, b []float64, ha, hb []bool) ([]float64, []float64) {
	first, anyB := len(a), false
	for i := range a {
		if ha[i] || hb[i] {
			if i < first {
				first = i
			}
		}
		anyB = anyB || hb[i]
	}
	if first >= len(a) {
		return nil, nil
	}
	if !anyB {
		return a[first:], nil
	}
	return a[first:], b[first:]
}

func p1Slotn(window string) int {
	switch window {
	case "7d":
		return 7
	case "30d":
		return 30
	}
	return 24
}

// p1Change is the per-device change against the window before, as a percentage.
func p1Change(cur, prev float64, okPrev bool, badWhenUp bool) (string, string) {
	if !okPrev || prev <= 0 {
		return "", "flat"
	}
	d, tone := ovDelta((cur-prev)*100/prev, "%", badWhenUp, 0)
	return d, tone
}

func buildModelUsageP1(in p1In) modelUsageP1 {
	p := modelUsageP1{Model: in.Model, ModelLabel: in.ModelLabel, VenueName: in.VenueName,
		WindowLabel: p1WindowLabels[in.Window], RoleBase: in.RoleBase}

	// The model's devices and their usage.
	var mine []db.DeviceAppUsage
	byID := map[uuid.UUID]db.Device{}
	for id, d := range in.ByID {
		if d.ProductKey() == in.Model {
			byID[id] = d
		}
	}
	p.Devices = len(byID)
	for _, u := range in.All {
		if _, ok := byID[u.DeviceID]; ok {
			u.Label = nameOf(u.Package, in.Labels)
			mine = append(mine, u)
			if u.PssAvgMB != nil {
				p.Full = true
			}
		}
	}
	for _, d := range byID {
		if d.HasBattery() && d.AgentKind != product.KindDPC {
			p.Battery = true
		}
	}

	// Measures this model has.
	avail := map[string]bool{"screen": true, "net": true}
	if p.Full {
		avail["mem"], avail["cpu"] = true, true
	}
	if p.Battery && p.Full {
		avail["battery"] = true
	}
	m := in.Measure
	if !avail[m] {
		m = "screen"
		if p.Full {
			m = "mem"
		}
	}
	p.MeasureLabel, _ = p1MeasureLabel(m)
	_, noun := p1MeasureLabel(m)

	href := func(model, venue, window, measure string) string {
		v := url.Values{}
		v.Set("model", model)
		if venue != "" {
			v.Set("venue", venue)
		}
		v.Set("window", window)
		v.Set("measure", measure)
		return "/apps/usage?" + v.Encode()
	}

	// Pills.
	count := map[string]int{}
	for _, d := range in.ByID {
		count[d.ProductKey()]++
	}
	var models []string
	for k := range count {
		models = append(models, k)
	}
	sort.Slice(models, func(i, j int) bool {
		if count[models[i]] != count[models[j]] {
			return count[models[i]] > count[models[j]]
		}
		return productLabel(models[i]) < productLabel(models[j])
	})
	for _, k := range models {
		p.Models = append(p.Models, ovOpt{Label: fmt.Sprintf("%s · %d", productLabel(k), count[k]), Href: href(k, in.Venue, in.Window, in.Measure), Active: k == in.Model})
	}
	p.Venues = append(p.Venues, ovOpt{Label: "All venues", Href: href(in.Model, "", in.Window, m), Active: in.Venue == ""})
	for _, v := range in.Venues {
		p.Venues = append(p.Venues, ovOpt{Label: v.Name, Href: href(in.Model, v.ID, in.Window, m), Active: v.ID == in.Venue})
	}
	for _, w := range []string{"24h", "7d", "30d"} {
		p.Windows = append(p.Windows, ovOpt{Label: p1WindowLabels[w], Href: href(in.Model, in.Venue, w, m), Active: w == in.Window})
	}
	for _, ms := range p1Measures {
		if avail[ms.Key] {
			p.Measures = append(p.Measures, ovOpt{Label: ms.Label, Href: href(in.Model, in.Venue, in.Window, ms.Key), Active: ms.Key == m})
		}
	}

	// The table and the per-device numbers.
	var topShare string
	p.Rows, p.More, p.Reporting, topShare = buildModelUsageRows(mine, byID, in.Labels, m)
	_ = topShare
	for i := range p.Rows {
		p.Rows[i].Role = isRoleApp(p.Rows[i].Package, in.RoleBase)
	}
	if p.Reporting == 0 {
		p.Empty = true
	}

	per := p1PerDevice(m, mine)
	cur, okCur := p1Mean(per)
	prevPer := p1PerDevice(m, in.Prev)
	prev, okPrev := p1Mean(prevPer)
	heroTxt := "—"
	if okCur {
		heroTxt = muFmt(m, cur)
	}

	// Banner.
	p.Banner = p1Banner(in, p, mine, byID, per, cur, m, noun, heroTxt)

	// Cards.
	n := p1Slotn(in.Window)
	slots := func(measure string) ([]float64, []float64) {
		a, ha := p1SlotsHas(measure, in.Series, in.Since, in.Win, n)
		b, hb := p1SlotsHas(measure, in.PrevSeries, in.Since.Add(-in.Win), in.Win, n)
		return p1Trim(a, b, ha, hb)
	}
	card := func(title, measure string, badUp bool) ovStat {
		c := ovStat{Title: title, Href: href(in.Model, in.Venue, in.Window, measure)}
		pd := p1PerDevice(measure, mine)
		cv, ok := p1Mean(pd)
		if !ok {
			c.Empty = "Not reported here yet"
			return c
		}
		pv, okp := p1Mean(p1PerDevice(measure, in.Prev))
		c.A = ovMetric{Value: muFmt(measure, cv), Label: "per device"}
		c.A.Delta, c.A.Tone = p1Change(cv, pv, okp, badUp)
		if !badUp {
			c.A.Tone = "flat"
		}
		// B: the biggest app's share of it.
		rows, _, _, _ := buildModelUsageRows(mine, byID, in.Labels, measure)
		if len(rows) > 0 && rows[0].Share > 0 {
			c.B = ovMetric{Value: fmt.Sprintf("%d %%", rows[0].Share), Label: rows[0].Name}
		}
		a, b := slots(measure)
		c.LineA, c.LineB, c.Fill = ovLines(a, b)
		return c
	}
	if p.Full {
		mem := card("Memory", "mem", true)
		if ramPct, ok := p1RAMInUse(byID); ok {
			mem.B = ovMetric{Value: fmt.Sprintf("%d %%", ramPct), Label: "of RAM in use now"}
		}
		p.Cards = []ovStat{mem, card("CPU", "cpu", true), card("Network", "net", true)}
	} else {
		rep := ovStat{Title: "Reporting", Href: href(in.Model, in.Venue, in.Window, m)}
		rep.A = ovMetric{Value: fmt.Sprintf("%d", p.Reporting), Label: fmt.Sprintf("of %d devices", p.Devices), Tone: "flat"}
		apps := map[string]bool{}
		for _, u := range mine {
			apps[u.Package] = true
		}
		rep.B = ovMetric{Value: fmt.Sprintf("%d", len(apps)), Label: "apps seen", Tone: "flat"}
		p.Cards = []ovStat{card("Screen time", "screen", false), card("Network", "net", true), rep}
	}

	// Top apps on the measure.
	p.TopTitle = noun
	for i, r := range p.Rows {
		if i >= 8 || r.Share <= 0 {
			break
		}
		t := p1Top{Name: r.Name, Href: "/apps/" + url.PathEscape(r.Package), Value: muFmt(m, r.Value), Share: r.Share, Pct: r.BarPct}
		if t.Pct < 2 {
			t.Pct = 2
		}
		switch {
		case isRoleApp(r.Package, in.RoleBase):
			t.Tone = "role"
		case !r.System && p1OverLimit(m, r):
			t.Tone = "bad"
		}
		p.Top = append(p.Top, t)
	}

	// Every model on the same measure (within the venue scope).
	modelRows := map[string]*p1Model{}
	modelUsage := map[string][]db.DeviceAppUsage{}
	for _, u := range in.All {
		if d, ok := in.ByID[u.DeviceID]; ok {
			modelUsage[d.ProductKey()] = append(modelUsage[d.ProductKey()], u)
		}
	}
	for _, k := range models {
		r := &p1Model{Label: productLabel(k), Href: href(k, in.Venue, in.Window, m), Devices: count[k], On: k == in.Model, Value: "—"}
		us := modelUsage[k]
		seen := map[uuid.UUID]bool{}
		for _, u := range us {
			seen[u.DeviceID] = true
		}
		r.Reporting = len(seen)
		if v, ok := p1Mean(p1PerDevice(m, us)); ok {
			r.Value = muFmt(m, v)
		}
		if rows, _, _, _ := buildModelUsageRows(us, in.ByID, in.Labels, m); len(rows) > 0 && rows[0].Share > 0 {
			r.Biggest = fmt.Sprintf("%s · %d %%", rows[0].Name, rows[0].Share)
		}
		modelRows[k] = r
	}
	for _, k := range models {
		p.ModelRows = append(p.ModelRows, *modelRows[k])
	}
	sort.SliceStable(p.ModelRows, func(i, j int) bool { return p.ModelRows[i].Reporting > p.ModelRows[j].Reporting })

	// Rail: ring, summary, seven days, heaviest devices.
	if p.Full {
		within, known := 0, 0
		for _, d := range byID {
			if used, total := ramOf(d.LatestExtra); total > 0 {
				known++
				if float64(used)/float64(total) < 0.85 {
					within++
				}
			}
		}
		pct := 0
		if known > 0 {
			pct = within * 100 / known
		}
		p.Ring = p1Ring{Pct: fmt.Sprintf("%d", pct), Offset: fmt.Sprintf("%.1f", 326.7*float64(100-pct)/100), Label: "under 85 % RAM",
			Title: fmt.Sprintf("%d of %d devices under 85 %% of their RAM", within, known), Tone: p1RingTone(pct)}
	} else {
		pct := 0
		if p.Devices > 0 {
			pct = p.Reporting * 100 / p.Devices
		}
		p.Ring = p1Ring{Pct: fmt.Sprintf("%d", pct), Offset: fmt.Sprintf("%.1f", 326.7*float64(100-pct)/100), Label: "reporting",
			Title: fmt.Sprintf("%d of %d devices report app use", p.Reporting, p.Devices), Tone: p1RingTone(pct)}
	}
	p.HiTitle = heroTxt + " " + noun + " per device"
	if p.Empty {
		p.HiTitle = "No app use reported"
	}
	p.HiSub = fmt.Sprintf("%s · last %s", in.ModelLabel, p.WindowLabel)
	if d, tone := p1Change(cur, prev, okPrev && okCur, m != "screen"); d != "" {
		p.HiDelta = d + " on the " + p.WindowLabel + " before"
		switch tone {
		case "good":
			p.HiTone = "up"
		case "bad":
			p.HiTone = "down"
		}
	}
	week := p1Slots(m, in.Week, in.WeekSince, 7*24*time.Hour, 7)
	peak := 0.0
	for _, v := range week {
		peak = math.Max(peak, v)
	}
	if peak > 0 {
		for i, v := range week {
			day := in.WeekSince.AddDate(0, 0, i)
			pct := int(v * 100 / peak)
			if pct < 4 {
				pct = 4
			}
			p.Bars = append(p.Bars, ovRailBar{Pct: pct, Label: day.Format("Mon")[:2], Title: day.Format("Mon 2 Jan") + ": " + muFmt(m, v) + " per device", Cur: i == 6})
		}
		p.BarsCap = noun + " per device, each day"
	}
	type dv struct {
		id uuid.UUID
		v  float64
	}
	var ds []dv
	for id, v := range per {
		ds = append(ds, dv{id, v})
	}
	sort.Slice(ds, func(i, j int) bool {
		if ds[i].v != ds[j].v {
			return ds[i].v > ds[j].v
		}
		return byID[ds[i].id].SerialNumber < byID[ds[j].id].SerialNumber
	})
	for i, x := range ds {
		if i >= 5 || x.v <= 0 {
			break
		}
		d := byID[x.id]
		h := p1Heavy{Serial: d.SerialNumber, Venue: d.RestaurantName, Value: muFmt(m, x.v), Href: "/devices/" + url.PathEscape(d.SerialNumber) + "?tab=apps"}
		if cur > 0 && len(ds) >= 3 {
			above := (x.v - cur) * 100 / cur
			switch {
			case above >= 30:
				h.Tone = "danger"
			case above >= 15:
				h.Tone = "warn"
			}
			if above >= 1 {
				h.Note = fmt.Sprintf("+%.0f %% on the model", above)
			}
		}
		p.Heavy = append(p.Heavy, h)
	}
	return p
}

// p1ExitProblem are the exit reasons that mean the app failed; a user or the system
// stopping it on purpose (user_requested, exit_self, package_updated…) is not counted.
var p1ExitProblem = map[string]bool{"crash": true, "native_crash": true, "anr": true, "low_memory": true, "excessive_resources": true, "init_failure": true}

func p1RingTone(pct int) string {
	switch {
	case pct >= 90:
		return "good"
	case pct >= 60:
		return "warn"
	}
	return "danger"
}

func p1RAMInUse(byID map[uuid.UUID]db.Device) (int, bool) {
	t, n := 0.0, 0
	for _, d := range byID {
		if used, total := ramOf(d.LatestExtra); total > 0 {
			t += float64(used) / float64(total)
			n++
		}
	}
	if n == 0 {
		return 0, false
	}
	return int(t*100/float64(n) + 0.5), true
}

func p1OverLimit(measure string, r muRow) bool {
	switch measure {
	case "mem":
		return r.Value >= muMemWarnMB
	case "cpu":
		return r.Value >= muCPUWarnPct
	}
	return false
}

// p1Banner lists what to look at, most urgent first: the role app stopping, apps over the
// memory or CPU limit, a device far above the rest of its model, apps that hold memory on
// devices where they never came on screen.
func p1Banner(in p1In, p modelUsageP1, usage []db.DeviceAppUsage, byID map[uuid.UUID]db.Device, per map[uuid.UUID]float64, cur float64, m, noun, hero string) a1Banner {
	b := a1Banner{Kicker: fmt.Sprintf("Resource use · %s · %s %s per device", in.ModelLabel, hero, noun)}
	if p.Empty {
		b.Kicker = "Resource use · " + in.ModelLabel
		b.Tone = "calm"
		b.Headline = "Nothing reported yet"
		if p.Devices == 0 {
			b.Calm = "No devices of this model in scope."
		} else {
			b.Calm = fmt.Sprintf("None of the %d %s here has sent app use in the last %s. Devices report it from firmware client 1.17.1 and Standard client 0.6.0.", p.Devices, pluralClass(in.ModelLabel), p.WindowLabel)
		}
		return b
	}
	var todos []a1Todo
	appHref := func(pkg string) string { return "/apps/" + url.PathEscape(pkg) }

	// The role app stopping (process exits in the last 24 h, from the host app report).
	if in.RoleBase != "" {
		exits, devs := 0, 0
		why := map[string]int{}
		for _, d := range byID {
			_, w := hostAppExits(d.LatestExtra)
			n := 0
			for k, c := range w {
				if p1ExitProblem[k] {
					n += c
					why[k] += c
				}
			}
			if n > 0 {
				exits += n
				devs++
			}
		}
		if exits >= 3 {
			top, tc := "", 0
			for k, c := range why {
				if c > tc || (c == tc && k < top) {
					top, tc = k, c
				}
			}
			name := nameOf(in.RoleBase, in.Labels)
			todos = append(todos, a1Todo{Title: fmt.Sprintf("%s stopped %d times in 24 h", name, exits),
				Sub: fmt.Sprintf("on %d device%s · most%s", devs, plural(devs), exitReason(top)), Action: "Open app", Href: appHref(in.RoleBase)})
		}
	}

	// Apps over the limit (not the role app: it is there to use the device).
	if m == "mem" || m == "cpu" {
		for _, r := range p.Rows {
			if r.System || isRoleApp(r.Package, in.RoleBase) || !p1OverLimit(m, r) {
				continue
			}
			lim := fmt.Sprintf("%.0f MB", muMemWarnMB)
			if m == "cpu" {
				lim = fmt.Sprintf("%.0f %%", muCPUWarnPct)
			}
			todos = append(todos, a1Todo{Title: fmt.Sprintf("%s averages %s", r.Name, muFmt(m, r.Value)),
				Sub: fmt.Sprintf("on %d device%s · above %s", r.On, plural(r.On), lim), Action: "Open app", Href: appHref(r.Package)})
		}
	}

	// A device far above the rest of its model.
	if len(per) >= 3 && cur > 0 {
		var worst uuid.UUID
		wv := 0.0
		for id, v := range per {
			if v > wv || (v == wv && byID[id].SerialNumber < byID[worst].SerialNumber) {
				worst, wv = id, v
			}
		}
		if above := (wv - cur) * 100 / cur; above >= 30 {
			d := byID[worst]
			sub := muFmt(m, wv) + " against " + muFmt(m, cur)
			if d.RestaurantName != "" {
				sub = d.RestaurantName + " · " + sub
			}
			todos = append(todos, a1Todo{Title: fmt.Sprintf("%s uses %.0f %% more %s than the model", d.SerialNumber, above, noun),
				Sub: sub, Action: "Open device", Href: "/devices/" + url.PathEscape(d.SerialNumber) + "?tab=apps"})
		}
	}

	// Apps holding memory in the background.
	if p.Full {
		for _, r := range p.Rows {
			if r.System || isRoleApp(r.Package, in.RoleBase) || r.Background == 0 || r.Mem == "—" {
				continue
			}
			mb := 0.0
			fmt.Sscanf(strings.TrimSuffix(r.Mem, " MB"), "%f", &mb)
			if mb < muMemWarnMB/3 {
				continue
			}
			todos = append(todos, a1Todo{Title: fmt.Sprintf("%s holds %s without coming on screen", r.Name, r.Mem),
				Sub: fmt.Sprintf("on %d device%s", r.Background, plural(r.Background)), Action: "Open app", Href: appHref(r.Package)})
		}
	}

	if len(todos) == 0 {
		b.Tone = "calm"
		b.Headline = "Nothing stands out"
		b.Calm = fmt.Sprintf("No app is above the limits and every device is within 30 %% of the model's %s.", noun)
		if len(p.Rows) > 0 && p.Rows[0].Share > 0 {
			b.Calm = fmt.Sprintf("%s is the biggest share of %s at %d %%. ", p.Rows[0].Name, noun, p.Rows[0].Share) + b.Calm
		}
		return b
	}
	b.Headline = fmt.Sprintf("%d thing%s to look at", len(todos), plural(len(todos)))
	if len(todos) > ovTodoRows {
		b.More = len(todos) - ovTodoRows
		todos = todos[:ovTodoRows]
	}
	b.Todos = todos
	return b
}
