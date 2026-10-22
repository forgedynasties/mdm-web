package dashboard

import (
	"encoding/json"
	"fmt"
	"net/http"
	"sort"
	"time"

	"github.com/google/uuid"

	"mdm/internal/db"
	"mdm/internal/product"
)

// Resource use per model (R1, picked 23 Oct 2026; user: "need resource use page per model
// too, like per android rbox"). /apps/usage: Lineup's models as chips; for the chosen one,
// every app and system service ranked by what it costs per device across that model's
// devices, how many of them run it, the worst device, and the heaviest devices behind
// each row. Window, venue and sort as pills. Fleet-wide viewers only.

type muChip struct {
	Key, Label string
	Devices    int
	On         bool
}

type muDevice struct {
	Serial, Venue, Value, Extra string
}

type muRow struct {
	Name, Package                string
	System                       bool
	On                           int
	Screen, Mem, Worst, CPU, Net string
	Battery                      string
	BarPct                       int
	Heaviest                     []muDevice
}

type muVenue struct {
	ID, Name string
	On       bool
}

type modelUsagePage struct {
	Chips      []muChip
	Model      string
	Label      string
	Window     string
	Windows    []string
	Sort       string
	Venue      string
	Venues     []muVenue
	Devices    int
	Reporting  int
	Rows       []muRow
	More       int
	HasBattery bool
	AnyFull    bool // some device here measures memory/CPU (firmware client)
	AnyLimited bool // some device here is on the Standard client
	TopShare   string
	MemInUse   string
}

const muRowsShown = 30

func (h *Handler) ModelUsagePage(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	q := r.URL.Query()
	devs, err := h.db.ListDevices(ctx, db.DeviceFilter{}, 0, 20000, "", "")
	if err != nil {
		http.Error(w, "Internal error", http.StatusInternalServerError)
		return
	}
	// Models, biggest first.
	count := map[string]int{}
	for _, d := range devs {
		if !d.Hidden {
			count[d.ProductKey()]++
		}
	}
	var chips []muChip
	for k, n := range count {
		chips = append(chips, muChip{Key: k, Label: productLabel(k), Devices: n})
	}
	sort.Slice(chips, func(i, j int) bool {
		if chips[i].Devices != chips[j].Devices {
			return chips[i].Devices > chips[j].Devices
		}
		return chips[i].Label < chips[j].Label
	})
	p := modelUsagePage{Window: q.Get("window"), Windows: []string{"24h", "7d", "30d"}, Sort: q.Get("sort"), Venue: q.Get("venue")}
	if _, ok := appUsageWindows[p.Window]; !ok {
		p.Window = "24h"
	}
	p.Model = product.Normalize(q.Get("model"))
	if _, ok := count[p.Model]; !ok && len(chips) > 0 {
		p.Model = chips[0].Key
	}
	for i := range chips {
		chips[i].On = chips[i].Key == p.Model
	}
	p.Chips = chips
	p.Label = productLabel(p.Model)

	// The model's devices (and its venues, for the pill).
	byID := map[uuid.UUID]db.Device{}
	venues := map[string]string{}
	var ids []uuid.UUID
	var ramUsed, ramN float64
	for _, d := range devs {
		if d.Hidden || d.ProductKey() != p.Model {
			continue
		}
		if d.RestaurantID != nil {
			venues[d.RestaurantID.String()] = d.RestaurantName
		}
		if p.Venue != "" && (d.RestaurantID == nil || d.RestaurantID.String() != p.Venue) {
			continue
		}
		p.Devices++
		byID[d.ID] = d
		ids = append(ids, d.ID)
		if d.AgentKind == product.KindDPC {
			p.AnyLimited = true
		} else {
			p.AnyFull = true
		}
		if d.HasBattery() && d.AgentKind != product.KindDPC {
			p.HasBattery = true
		}
		if used, total := ramOf(d.LatestExtra); total > 0 {
			ramUsed += float64(used) / float64(total)
			ramN++
		}
	}
	for id, name := range venues {
		p.Venues = append(p.Venues, muVenue{ID: id, Name: name, On: id == p.Venue})
	}
	sort.Slice(p.Venues, func(i, j int) bool { return p.Venues[i].Name < p.Venues[j].Name })
	if ramN > 0 {
		p.MemInUse = fmt.Sprintf("%.0f %% of its RAM", ramUsed*100/ramN)
	}
	switch p.Sort {
	case "screen", "mem", "cpu", "net", "battery":
	default:
		p.Sort = "mem"
		if !p.AnyFull {
			p.Sort = "screen"
		}
	}

	usage, _ := h.db.AppUsageForDevices(ctx, ids, time.Now().Add(-appUsageWindows[p.Window]))
	labels, _ := h.db.AppLabels(ctx, ids)
	p.Rows, p.More, p.Reporting, p.TopShare = buildModelUsageRows(usage, byID, labels, p.Sort)
	h.render(w, r, "apps_usage.html", map[string]any{"Title": "Resource use", "ActivePath": "/apps/usage", "P": p})
}

// buildModelUsageRows sums each app across the devices, averages per device that runs it,
// and keeps the three heaviest devices by the sort metric.
func buildModelUsageRows(usage []db.DeviceAppUsage, byID map[uuid.UUID]db.Device, labels map[string]string, sortBy string) (rows []muRow, more, reporting int, topShare string) {
	type agg struct {
		pkg            string
		system         bool
		devs           []db.DeviceAppUsage
		fg, net, power float64
		pssSum, cpuSum float64
		pssN, cpuN     int
		worst          int
		worstDev       uuid.UUID
	}
	val := func(u db.DeviceAppUsage) float64 {
		switch sortBy {
		case "screen":
			return float64(u.FgMs)
		case "cpu":
			if u.CPUAvg != nil {
				return *u.CPUAvg
			}
		case "net":
			return float64(u.RxBytes + u.TxBytes)
		case "battery":
			return u.PowerMAh
		case "mem":
			if u.PssAvgMB != nil {
				return *u.PssAvgMB
			}
		}
		return 0
	}
	apps := map[string]*agg{}
	seen := map[uuid.UUID]bool{}
	for _, u := range usage {
		seen[u.DeviceID] = true
		a := apps[u.Package]
		if a == nil {
			a = &agg{pkg: u.Package}
			apps[u.Package] = a
		}
		a.system = a.system || u.System
		a.devs = append(a.devs, u)
		a.fg += float64(u.FgMs)
		a.net += float64(u.RxBytes + u.TxBytes)
		a.power += u.PowerMAh
		if u.PssAvgMB != nil {
			a.pssSum += *u.PssAvgMB
			a.pssN++
		}
		if u.CPUAvg != nil {
			a.cpuSum += *u.CPUAvg
			a.cpuN++
		}
		if u.PssMaxMB > a.worst {
			a.worst, a.worstDev = u.PssMaxMB, u.DeviceID
		}
	}
	reporting = len(seen)
	key := func(a *agg) float64 {
		n := float64(len(a.devs))
		switch sortBy {
		case "screen":
			return a.fg / n
		case "cpu":
			if a.cpuN > 0 {
				return a.cpuSum / float64(a.cpuN)
			}
			return 0
		case "net":
			return a.net / n
		case "battery":
			return a.power / n
		}
		if a.pssN > 0 {
			return a.pssSum / float64(a.pssN)
		}
		return 0
	}
	list := make([]*agg, 0, len(apps))
	total := 0.0
	for _, a := range apps {
		list = append(list, a)
		total += key(a) * float64(len(a.devs))
	}
	sort.SliceStable(list, func(i, j int) bool {
		if a, b := key(list[i]), key(list[j]); a != b {
			return a > b
		}
		return list[i].pkg < list[j].pkg
	})
	if len(list) > 0 && total > 0 && key(list[0]) > 0 {
		topShare = fmt.Sprintf("%s · %.0f %%", nameOf(list[0].pkg, labels), key(list[0])*float64(len(list[0].devs))*100/total)
	}
	top := 0.0
	if len(list) > 0 {
		top = key(list[0])
	}
	for i, a := range list {
		if i >= muRowsShown {
			more = len(list) - muRowsShown
			break
		}
		n := float64(len(a.devs))
		row := muRow{Name: nameOf(a.pkg, labels), Package: a.pkg, System: a.system, On: len(a.devs),
			Screen: fmtMinutes(int64(a.fg / n)), Net: fmtBytes(int64(a.net / n)), Mem: "—", CPU: "—", Battery: "—"}
		if a.pssN > 0 {
			row.Mem = fmt.Sprintf("%.0f MB", a.pssSum/float64(a.pssN))
			if a.worst > 0 && a.pssN > 1 {
				row.Worst = fmt.Sprintf("worst %d MB · %s", a.worst, byID[a.worstDev].SerialNumber)
			}
		}
		if a.cpuN > 0 {
			row.CPU = fmt.Sprintf("%.1f %%", a.cpuSum/float64(a.cpuN))
		}
		if a.power > 0 {
			row.Battery = fmt.Sprintf("%.0f mAh", a.power/n)
		}
		if top > 0 {
			row.BarPct = int(key(a) * 100 / top)
		}
		// The heaviest devices for this app, by the sort metric.
		ds := append([]db.DeviceAppUsage(nil), a.devs...)
		sort.SliceStable(ds, func(i, j int) bool { return val(ds[i]) > val(ds[j]) })
		for k, u := range ds {
			if k >= 3 || val(u) <= 0 {
				break
			}
			d := byID[u.DeviceID]
			md := muDevice{Serial: d.SerialNumber, Venue: d.RestaurantName}
			switch sortBy {
			case "screen":
				md.Value = fmtMinutes(u.FgMs)
			case "cpu":
				md.Value = fmt.Sprintf("%.1f %% CPU", *u.CPUAvg)
			case "net":
				md.Value = fmtBytes(u.RxBytes + u.TxBytes)
			case "battery":
				md.Value = fmt.Sprintf("%.0f mAh", u.PowerMAh)
			default:
				md.Value = fmt.Sprintf("%.0f MB", *u.PssAvgMB)
			}
			row.Heaviest = append(row.Heaviest, md)
		}
		rows = append(rows, row)
	}
	return rows, more, reporting, topShare
}

// ramOf is extra.ram_usage_mb {used, total}.
func ramOf(extra json.RawMessage) (used, total int) {
	var m struct {
		RAM struct {
			Used  int `json:"used"`
			Total int `json:"total"`
		} `json:"ram_usage_mb"`
	}
	if json.Unmarshal(extra, &m) != nil {
		return 0, 0
	}
	return m.RAM.Used, m.RAM.Total
}

func nameOf(pkg string, labels map[string]string) string {
	if n := labels[pkg]; n != "" {
		return n
	}
	return pkg
}
