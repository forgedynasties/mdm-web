package dashboard

import (
	"context"
	"encoding/json"
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// U1 (picked 23 Oct 2026, after the user's reference shot): the Resource use page as a
// hero number with the hourly (or daily) shape beside it, the top apps as "tanks" filled
// by their share — red with a warning when an app other than the role's own is over its
// limit — and a rail with the heaviest device and a "needs a look" list.

type muBar struct {
	Pct   int
	Hi    bool
	Title string
}

type muTank struct {
	Name, Value, Detail string
	Fill, Share         int
	Tone                string // a | b | c | d | r
	Warn                string
	Initial             string
}

type muLook struct {
	Icon, Title, Value, Chip string
	Pct                      int
	Tone                     string // bad | warn | ok
}

type muHeavy struct {
	Serial, Num, Unit, Apps string
}

type modelUsageU1 struct {
	HeroLabel, HeroNum, HeroUnit string
	BarsLabel, Peak              string
	Bars                         []muBar
	RAMText                      string
	RAMPct                       int
	Tanks                        []muTank
	Heavy                        *muHeavy
	Looks                        []muLook
}

// Limits above which an app (other than the role's own) is drawn red.
const (
	muMemWarnMB  = 300.0
	muCPUWarnPct = 20.0
)

var muTones = []string{"a", "b", "c", "d"}

// muSplit is a sort metric as a big number and its unit: 1.42 / GB.
func muSplit(sortBy string, v float64) (string, string) {
	switch sortBy {
	case "screen":
		h := v / 3600000
		if h >= 1 {
			return fmt.Sprintf("%.1f", h), "h"
		}
		return fmt.Sprintf("%.0f", v/60000), "min"
	case "net":
		switch {
		case v >= 1<<30:
			return fmt.Sprintf("%.2f", v/(1<<30)), "GB"
		case v >= 1<<20:
			return fmt.Sprintf("%.0f", v/(1<<20)), "MB"
		}
		return fmt.Sprintf("%.0f", v/1024), "KB"
	case "cpu":
		return fmt.Sprintf("%.1f", v), "%"
	case "battery":
		return fmt.Sprintf("%.0f", v), "mAh"
	}
	if v >= 1024 {
		return fmt.Sprintf("%.2f", v/1024), "GB"
	}
	return fmt.Sprintf("%.0f", v), "MB"
}

func muFmt(sortBy string, v float64) string {
	n, u := muSplit(sortBy, v)
	if u == "%" {
		return n + " %"
	}
	return n + " " + u
}

func muDeviceValue(sortBy string, u db.DeviceAppUsage) (float64, bool) {
	switch sortBy {
	case "screen":
		return float64(u.FgMs), true
	case "net":
		return float64(u.RxBytes + u.TxBytes), true
	case "battery":
		return u.PowerMAh, true
	case "cpu":
		if u.CPUAvg != nil {
			return *u.CPUAvg, true
		}
		return 0, false
	}
	if u.PssAvgMB != nil {
		return *u.PssAvgMB, true
	}
	return 0, false
}

// roleAppBase is the base package of the model's role app ("" when none).
func (h *Handler) roleAppBase(ctx context.Context, model string) string {
	role := ""
	if p, ok := product.Resolve(model); ok && p.Class != "" {
		role = p.Class
	}
	if role == "" {
		role, _ = h.db.ProductRole(ctx, model)
	}
	if role == "" {
		return ""
	}
	return h.db.RoleAppBase(ctx, role)
}

func isRoleApp(pkg, base string) bool {
	return base != "" && (pkg == base || strings.HasPrefix(pkg, base+"."))
}

func buildModelUsageU1(p *modelUsagePage, usage []db.DeviceAppUsage, series []db.UsagePoint, byID map[uuid.UUID]db.Device,
	roleBase string, now time.Time) modelUsageU1 {
	var v modelUsageU1
	sortBy := p.Sort
	v.HeroLabel = map[string]string{"screen": "On screen, per device", "net": "Network, per device", "cpu": "CPU, per device",
		"battery": "Battery, per device", "mem": "Used by apps, per device"}[sortBy]

	// Hero: each device's total over its apps, averaged over the devices that measured it.
	perDev := map[uuid.UUID]float64{}
	topApps := map[uuid.UUID][]db.DeviceAppUsage{}
	for _, u := range usage {
		if val, ok := muDeviceValue(sortBy, u); ok {
			perDev[u.DeviceID] += val
			topApps[u.DeviceID] = append(topApps[u.DeviceID], u)
		}
	}
	if len(perDev) > 0 {
		sum, heavy, heavyVal := 0.0, uuid.Nil, -1.0
		for id, val := range perDev {
			sum += val
			if val > heavyVal {
				heavy, heavyVal = id, val
			}
		}
		v.HeroNum, v.HeroUnit = muSplit(sortBy, sum/float64(len(perDev)))
		if heavyVal > 0 && len(perDev) > 1 {
			apps := topApps[heavy]
			sort.Slice(apps, func(i, j int) bool {
				a, _ := muDeviceValue(sortBy, apps[i])
				b, _ := muDeviceValue(sortBy, apps[j])
				return a > b
			})
			var names []string
			for i, a := range apps {
				if i >= 2 {
					break
				}
				val, _ := muDeviceValue(sortBy, a)
				names = append(names, a.Label+" "+muFmt(sortBy, val))
			}
			n, u := muSplit(sortBy, heavyVal)
			v.Heavy = &muHeavy{Serial: byID[heavy].SerialNumber, Num: n, Unit: u, Apps: strings.Join(names, " · ")}
		}
	}

	// Bars: hours for 24 h, days otherwise; each the average device's total.
	hourly := p.Window == "24h"
	step := 24 * time.Hour
	n := 7
	if p.Window == "30d" {
		n = 30
	}
	start := now.Truncate(24 * time.Hour).Add(-time.Duration(n-1) * step)
	if hourly {
		step, n = time.Hour, 24
		start = now.Truncate(time.Hour).Add(-23 * time.Hour)
	}
	type acc struct {
		devs map[uuid.UUID]float64
		hrs  map[uuid.UUID]int
	}
	buckets := make([]acc, n)
	for i := range buckets {
		buckets[i] = acc{devs: map[uuid.UUID]float64{}, hrs: map[uuid.UUID]int{}}
	}
	averaged := sortBy == "mem" || sortBy == "cpu"
	for _, pt := range series {
		i := int(pt.Bucket.Sub(start) / step)
		if i < 0 || i >= n {
			continue
		}
		var val float64
		switch sortBy {
		case "screen":
			val = float64(pt.FgMs)
		case "net":
			val = float64(pt.NetBytes)
		case "battery":
			val = pt.PowerMAh
		case "cpu":
			if !pt.HasCPU {
				continue
			}
			val = pt.CPUPct
		default:
			if !pt.HasPss {
				continue
			}
			val = pt.PssMB
		}
		buckets[i].devs[pt.DeviceID] += val
		buckets[i].hrs[pt.DeviceID]++
	}
	vals := make([]float64, n)
	max, maxAt := 0.0, -1
	for i, b := range buckets {
		if len(b.devs) == 0 {
			continue
		}
		sum := 0.0
		for id, val := range b.devs {
			if averaged && b.hrs[id] > 0 {
				val /= float64(b.hrs[id]) // a day of hourly memory readings is their average
			}
			sum += val
		}
		vals[i] = sum / float64(len(b.devs))
		if vals[i] > max {
			max, maxAt = vals[i], i
		}
	}
	for i, val := range vals {
		at := start.Add(time.Duration(i) * step)
		title := at.Format("Mon 2 Jan")
		if hourly {
			title = at.Format("15:04")
		}
		b := muBar{Title: title + " · " + muFmt(sortBy, val)}
		if max > 0 {
			b.Pct = int(val * 100 / max)
			b.Hi = val >= max*0.75
		}
		if b.Pct < 3 {
			b.Pct = 3
		}
		v.Bars = append(v.Bars, b)
	}
	v.BarsLabel = "Average per day"
	if hourly {
		v.BarsLabel = "Average per hour"
	}
	if maxAt >= 0 {
		at := start.Add(time.Duration(maxAt) * step)
		when := "on " + at.Format("Mon 2 Jan")
		if hourly {
			when = "at " + at.Format("15:04")
		}
		v.Peak = "peak " + muFmt(sortBy, max) + " " + when
	}

	// Tanks: the top four apps, filled by their share.
	for i, r := range p.Rows {
		if i >= 4 || r.Share <= 0 {
			break
		}
		t := muTank{Name: r.Name, Value: muFmt(sortBy, r.Value), Share: r.Share, Fill: r.Share, Tone: muTones[i%len(muTones)]}
		if t.Fill < 8 {
			t.Fill = 8
		}
		if r.On < p.Reporting {
			t.Detail = fmt.Sprintf("%d device%s", r.On, sIfN(r.On))
		}
		if !isRoleApp(r.Package, roleBase) {
			if sortBy == "mem" && r.Value >= muMemWarnMB {
				t.Tone, t.Warn = "r", fmt.Sprintf("over %.0f MB", muMemWarnMB)
			}
			if sortBy == "cpu" && r.Value >= muCPUWarnPct {
				t.Tone, t.Warn = "r", fmt.Sprintf("over %.0f %% CPU", muCPUWarnPct)
			}
		}
		if rs := []rune(r.Name); len(rs) > 0 {
			t.Initial = strings.ToUpper(string(rs[0]))
		}
		v.Tanks = append(v.Tanks, t)
	}

	// RAM in use across the model's devices, and the needs-a-look list.
	var used, total float64
	inBudget, measured := 0, 0
	exits, exitWhy := 0, map[string]int{}
	for _, d := range byID {
		if u, t := ramOf(d.LatestExtra); t > 0 {
			used += float64(u)
			total += float64(t)
			measured++
			if float64(u)/float64(t) < 0.85 {
				inBudget++
			}
		}
		n, why := hostAppExits(d.LatestExtra)
		exits += n
		for k, c := range why {
			exitWhy[k] += c
		}
	}
	if measured > 0 && total > 0 {
		v.RAMText = fmt.Sprintf("%.1f of %.0f GB", used/float64(measured)/1024, total/float64(measured)/1024)
		v.RAMPct = int(used * 100 / total)
	}
	if p.AnyFull {
		var bg *muRow
		for i := range p.Rows {
			r := &p.Rows[i]
			if r.System || isRoleApp(r.Package, roleBase) || r.Background == 0 {
				continue
			}
			if bg == nil || r.Background > bg.Background {
				bg = r
			}
		}
		if bg != nil && p.Reporting > 0 {
			pct := bg.Background * 100 / p.Reporting
			v.Looks = append(v.Looks, muLook{Icon: "⚠", Title: bg.Name + " open in the background",
				Value: fmt.Sprintf("%d of %d devices", bg.Background, p.Reporting), Chip: fmt.Sprintf("%d%%", pct), Pct: pct, Tone: "bad"})
		}
	}
	if exits > 0 {
		top, topN := "", 0
		for k, c := range exitWhy {
			if c > topN {
				top, topN = k, c
			}
		}
		pct := exits * 5
		if pct > 100 {
			pct = 100
		}
		v.Looks = append(v.Looks, muLook{Icon: "↻", Title: "Host app stopped" + exitReason(top),
			Value: fmt.Sprintf("%d time%s in 24 h", exits, sIfN(exits)), Chip: fmt.Sprintf("%d", exits), Pct: pct, Tone: "warn"})
	}
	if measured > 0 {
		pct := inBudget * 100 / measured
		tone := "ok"
		if pct < 80 {
			tone = "warn"
		}
		v.Looks = append(v.Looks, muLook{Icon: "✓", Title: "Within the RAM budget (under 85 %)",
			Value: fmt.Sprintf("%d of %d devices", inBudget, measured), Chip: fmt.Sprintf("%d%%", pct), Pct: pct, Tone: tone})
	}
	return v
}

// hostAppExits sums the role app's process exits in the last 24 h, by reason.
func hostAppExits(extra json.RawMessage) (int, map[string]int) {
	var m struct {
		HostApp struct {
			Variants []struct {
				Exits map[string]int `json:"exits_24h"`
			} `json:"variants"`
		} `json:"host_app"`
	}
	why := map[string]int{}
	if json.Unmarshal(extra, &m) != nil {
		return 0, why
	}
	n := 0
	for _, v := range m.HostApp.Variants {
		for k, c := range v.Exits {
			n += c
			why[k] += c
		}
	}
	return n, why
}

func exitReason(k string) string {
	switch strings.ToLower(k) {
	case "low_memory", "lmk":
		return " (low memory)"
	case "crash", "crash_native":
		return " (crashed)"
	case "anr":
		return " (not responding)"
	case "":
		return ""
	}
	return " (" + strings.ReplaceAll(strings.ToLower(k), "_", " ") + ")"
}
