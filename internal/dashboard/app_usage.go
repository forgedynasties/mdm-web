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

type appUsageRowView struct {
	Name, Package        string
	System               bool
	Screen, Mem, MemPeak string
	CPU, Net, Battery    string
	Launches             int
	BarPct               int // the sorted column's share of the top row, for the bar
}

type appUsageView struct {
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

func (h *Handler) appUsageView(ctx context.Context, d *db.Device, window, sortBy string) appUsageView {
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
	v := h.appUsageView(r.Context(), d, r.URL.Query().Get("window"), r.URL.Query().Get("sort"))
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	_ = h.tmpl.ExecuteTemplate(w, "dd-app-usage", v)
}
