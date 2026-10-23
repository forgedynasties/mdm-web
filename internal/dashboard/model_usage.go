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

// Resource use per model (/apps/usage). R1 (23 Oct 2026; user: "need resource use page
// per model too, like per android rbox") gave every app ranked by what it costs per device
// on one model; P1 (23 Oct, "take inspo from overview page instead") puts that in the
// Overview's layout: filter pills, a to-do banner, delta cards, top apps beside every
// model, the full table, and a rail. Fleet-wide viewers only. Builder in model_usage_p1.go.

type muDevice struct {
	Serial, Venue, Value, Extra string
}

type muRow struct {
	Name, Package                string
	System, Role                 bool // Role: the model's role app (set by P1)
	On                           int
	Screen, Mem, Worst, CPU, Net string
	Battery                      string
	BarPct                       int
	Heaviest                     []muDevice
	Value                        float64 // per device that runs it, by the sort
	Share                        int     // % of everything this model spends on the sort
	Background                   int     // devices where it holds memory but never came on screen
}

type muVenue struct {
	ID, Name string
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
	in := p1In{Now: time.Now(), Window: q.Get("window"), Venue: q.Get("venue"), Measure: q.Get("measure")}
	if in.Measure == "" {
		in.Measure = q.Get("sort") // R1/U1 links
	}
	if _, ok := appUsageWindows[in.Window]; !ok {
		in.Window = "24h"
	}
	in.Win = appUsageWindows[in.Window]
	in.Since = in.Now.Add(-in.Win)

	// Every device in the venue scope, then their usage once: it picks the default model
	// (the one with the most devices reporting) and fills the Models card.
	venues := map[string]string{}
	var ids []uuid.UUID
	byID := map[uuid.UUID]db.Device{}
	for _, d := range devs {
		if d.Hidden {
			continue
		}
		if d.RestaurantID != nil {
			venues[d.RestaurantID.String()] = d.RestaurantName
		}
		if in.Venue != "" && (d.RestaurantID == nil || d.RestaurantID.String() != in.Venue) {
			continue
		}
		byID[d.ID] = d
		ids = append(ids, d.ID)
	}
	if _, ok := venues[in.Venue]; !ok {
		in.Venue = ""
	}
	in.VenueName = venues[in.Venue]
	for id, name := range venues {
		in.Venues = append(in.Venues, muVenue{ID: id, Name: name})
	}
	sort.Slice(in.Venues, func(i, j int) bool { return in.Venues[i].Name < in.Venues[j].Name })

	all, _ := h.db.AppUsageForDevices(ctx, ids, in.Since, in.Now)
	in.Labels, _ = h.db.AppLabels(ctx, ids)
	in.All = all
	in.ByID = byID

	in.Model = product.Normalize(q.Get("model"))
	in.Model = p1DefaultModel(in.Model, byID, all)
	in.ModelLabel = productLabel(in.Model)

	var mids []uuid.UUID
	for _, d := range byID {
		if d.ProductKey() == in.Model {
			mids = append(mids, d.ID)
		}
	}
	prevSince := in.Since.Add(-in.Win)
	in.Prev, _ = h.db.AppUsageForDevices(ctx, mids, prevSince, in.Since)
	in.Series, _ = h.db.AppUsageSeries(ctx, mids, in.Since, in.Now)
	in.PrevSeries, _ = h.db.AppUsageSeries(ctx, mids, prevSince, in.Since)
	day := in.Now.Truncate(time.Hour)
	in.WeekSince = time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, day.Location()).AddDate(0, 0, -6)
	if !in.WeekSince.Before(in.Since) {
		in.Week = in.Series // the window already covers the rail's seven days
	} else {
		in.Week, _ = h.db.AppUsageSeries(ctx, mids, in.WeekSince, in.Now)
	}
	in.RoleBase = h.roleAppBase(ctx, in.Model)
	p := buildModelUsageP1(in)
	h.render(w, r, "apps_usage.html", map[string]any{"Title": "Resource use", "ActivePath": "/apps/usage", "AppsTab": "usage", "P": p})
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
		v, _ := muDeviceValue(sortBy, u)
		return v
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
		row.Value = key(a)
		if total > 0 {
			row.Share = int(key(a)*float64(len(a.devs))*100/total + 0.5)
		}
		for _, u := range a.devs {
			if u.PssAvgMB != nil && *u.PssAvgMB > 0 && u.FgMs == 0 {
				row.Background++
			}
		}
		// The heaviest devices for this app, by the sort metric.
		ds := append([]db.DeviceAppUsage(nil), a.devs...)
		sort.SliceStable(ds, func(i, j int) bool { return val(ds[i]) > val(ds[j]) })
		for k, u := range ds {
			if k >= 3 || val(u) <= 0 {
				break
			}
			d := byID[u.DeviceID]
			row.Heaviest = append(row.Heaviest, muDevice{Serial: d.SerialNumber, Venue: d.RestaurantName, Value: muFmt(sortBy, val(u))})
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
