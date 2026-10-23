package dashboard

import (
	"context"
	"fmt"
	"net/http"
	"sort"
	"time"

	"mdm/internal/db"
	"mdm/internal/product"
)

// Resource use on the device page's Apps tab (A1, picked 22 Oct 2026): every app and
// system service ranked by what it cost over 24 h / 7 d / 30 d, sortable by screen time,
// memory, CPU, network or battery. Data from db.AppUsage (POST /api/v1/app-usage).
//
// T1 (picked 23 Oct 2026; user: "it displays all the apps, and we need to manually switch
// between memory and cpu and there is no visualization"): the card opens on the top five
// apps for memory and for CPU side by side, as bars (screen time and network on the Standard
// client, which can't measure the other two). The role app is always listed, in coral; an
// app other than the role's own over 300 MB or 20 % CPU is red. "All N apps" opens the
// full table in place.

type appUsageRowView struct {
	Name, Package        string
	System               bool
	Screen, Mem, MemPeak string
	CPU, Net, Battery    string
	Launches             int
	BarPct               int // the sorted column's share of the top row, for the bar
}

// auBar is one app in a T1 column.
type auBar struct {
	Name, Value string
	Pct         int    // of the column's top value
	Tone        string // role | bad | sys | ""
}

type auCol struct {
	Title, Total string
	Bars         []auBar
}

type appUsageView struct {
	Cols       []auCol // T1: memory | CPU (screen | network on the Standard client)
	All        bool    // the full table is open
	Count      int     // apps that reported in the window
	Serial     string
	Window     string // 24h | 7d | 30d
	Windows    []string
	Sort       string // screen | mem | cpu | net | battery
	Rows       []appUsageRowView
	More       int
	HasBattery bool
	Limited    bool // Standard client: screen time and network only
	Reported   bool
}

var appUsageWindows = map[string]time.Duration{"24h": 24 * time.Hour, "7d": 7 * 24 * time.Hour, "30d": 30 * 24 * time.Hour}

const appUsageShown = 25

func (h *Handler) appUsageView(ctx context.Context, d *db.Device, window, sortBy string, all bool) appUsageView {
	if _, ok := appUsageWindows[window]; !ok {
		window = "24h"
	}
	limited := d.AgentKind == product.KindDPC
	switch sortBy {
	case "screen", "mem", "cpu", "net", "battery":
	default:
		sortBy = "mem"
		if limited {
			sortBy = "screen"
		}
	}
	v := appUsageView{Serial: d.SerialNumber, Window: window, Windows: []string{"24h", "7d", "30d"}, Sort: sortBy, HasBattery: d.HasBattery() && !limited, Limited: limited}
	rows, err := h.db.AppUsage(ctx, d.ID, time.Now().Add(-appUsageWindows[window]))
	if err != nil || len(rows) == 0 {
		return v
	}
	v.Reported = true
	v.All, v.Count = all, len(rows)
	role := h.roleAppBase(ctx, d.ProductKey())
	if limited {
		v.Cols = []auCol{appUsageTop(rows, "screen", role), appUsageTop(rows, "net", role)}
	} else {
		v.Cols = []auCol{appUsageTop(rows, "mem", role), appUsageTop(rows, "cpu", role)}
	}
	key := func(r db.AppUsageRow) float64 {
		switch sortBy {
		case "screen":
			return float64(r.FgMs)
		case "cpu":
			if r.CPUAvg != nil {
				return *r.CPUAvg
			}
			return 0
		case "net":
			return float64(r.RxBytes + r.TxBytes)
		case "battery":
			return r.PowerMAh
		}
		if r.PssAvgMB != nil {
			return *r.PssAvgMB
		}
		return 0
	}
	sort.SliceStable(rows, func(i, j int) bool {
		if a, b := key(rows[i]), key(rows[j]); a != b {
			return a > b
		}
		return rows[i].Package < rows[j].Package
	})
	top := key(rows[0])
	for i, r := range rows {
		if i >= appUsageShown {
			v.More = len(rows) - appUsageShown
			break
		}
		name := r.Label
		if name == "" {
			name = r.Package
		}
		rv := appUsageRowView{Name: name, Package: r.Package, System: r.System, Launches: r.Launches,
			Screen: fmtMinutes(r.FgMs), Net: fmtBytes(r.RxBytes + r.TxBytes), CPU: "—", Mem: "—", Battery: "—"}
		if r.PssAvgMB != nil {
			rv.Mem = fmt.Sprintf("%.0f MB", *r.PssAvgMB)
			if r.PssMaxMB > 0 {
				rv.MemPeak = fmt.Sprintf("%d MB peak", r.PssMaxMB)
			}
		}
		if r.CPUAvg != nil {
			rv.CPU = fmt.Sprintf("%.1f %%", *r.CPUAvg)
		}
		if r.PowerMAh > 0 {
			rv.Battery = fmt.Sprintf("%.0f mAh", r.PowerMAh)
		}
		if top > 0 {
			rv.BarPct = int(key(r) * 100 / top)
		}
		v.Rows = append(v.Rows, rv)
	}
	return v
}

// fmtMinutes is screen time: "—" for none, "12 min", "3 h 05".
func fmtMinutes(ms int64) string {
	m := ms / 60000
	switch {
	case ms <= 0:
		return "—"
	case m < 1:
		return "<1 min"
	case m < 60:
		return fmt.Sprintf("%d min", m)
	}
	return fmt.Sprintf("%d h %02d", m/60, m%60)
}

// fmtBytes is a byte count in KB / MB / GB.
func fmtBytes(b int64) string {
	switch {
	case b <= 0:
		return "—"
	case b < 1<<20:
		return fmt.Sprintf("%d KB", b>>10)
	case b < 1<<30:
		return fmt.Sprintf("%.0f MB", float64(b)/(1<<20))
	}
	return fmt.Sprintf("%.1f GB", float64(b)/(1<<30))
}

// DeviceAppUsage is GET /devices/{serial}/app-usage?window=&sort=: the Resource use card,
// swapped in place when the window or sort changes.
func (h *Handler) DeviceAppUsage(w http.ResponseWriter, r *http.Request) {
	d, err := h.db.GetDevice(r.Context(), r.PathValue("serial"))
	if err != nil || d == nil {
		http.Error(w, "device not found", http.StatusNotFound)
		return
	}
	q := r.URL.Query()
	v := h.appUsageView(r.Context(), d, q.Get("window"), q.Get("sort"), q.Get("all") == "1")
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.tmpl.ExecuteTemplate(w, "dd-app-usage", v)
}

const auTopN = 5

// appUsageTop is one T1 column: the top five apps on [measure], with the role app put in
// the last place when it is not among them, so its cost is always visible.
func appUsageTop(rows []db.AppUsageRow, measure, roleBase string) auCol {
	val := func(r db.AppUsageRow) (float64, bool) {
		return muDeviceValue(measure, db.DeviceAppUsage{AppUsageRow: r})
	}
	title, _ := p1MeasureLabel(measure)
	col := auCol{Title: title}
	type kv struct {
		r db.AppUsageRow
		v float64
	}
	var list []kv
	total := 0.0
	for _, r := range rows {
		if v, ok := val(r); ok && v > 0 {
			list = append(list, kv{r, v})
			total += v
		}
	}
	sort.SliceStable(list, func(i, j int) bool {
		if list[i].v != list[j].v {
			return list[i].v > list[j].v
		}
		return list[i].r.Package < list[j].r.Package
	})
	if total > 0 {
		switch measure {
		case "mem":
			col.Total = muFmt("mem", total) + " in apps"
		case "cpu":
			col.Total = fmt.Sprintf("%.0f %% summed", total)
		default:
			col.Total = muFmt(measure, total) + " in all"
		}
	}
	top := list
	if len(top) > auTopN {
		top = append([]kv(nil), list[:auTopN]...)
		in := false
		for _, x := range top {
			in = in || isRoleApp(x.r.Package, roleBase)
		}
		if !in {
			for _, x := range list[auTopN:] {
				if isRoleApp(x.r.Package, roleBase) {
					top[auTopN-1] = x
					break
				}
			}
		}
	}
	for _, x := range top {
		name := x.r.Label
		if name == "" {
			name = x.r.Package
		}
		b := auBar{Name: name, Value: muFmt(measure, x.v)}
		if list[0].v > 0 {
			b.Pct = int(x.v*100/list[0].v + 0.5)
		}
		if b.Pct < 2 {
			b.Pct = 2
		}
		switch {
		case isRoleApp(x.r.Package, roleBase):
			b.Tone = "role"
		case !x.r.System && ((measure == "mem" && x.v >= muMemWarnMB) || (measure == "cpu" && x.v >= muCPUWarnPct)):
			b.Tone = "bad"
		case x.r.System:
			b.Tone = "sys"
		}
		col.Bars = append(col.Bars, b)
	}
	return col
}
