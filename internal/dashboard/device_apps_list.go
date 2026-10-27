package dashboard

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"mdm/internal/db"
	"mdm/internal/product"
)

// The Apps tab as one list (device tabs plan A1, picked 27 Oct 2026; user: "no need to
// show host app again in the apps list. and we can also add small icons in this list").
// Every installed app is a row with its icon, version and what it used in the window
// (memory, CPU, screen time), user apps ranked by use, system apps folded under them. The
// host app has its own line above the list, so its packages are left out of it.

type deviceAppRow struct {
	db.DevicePackage
	Mem, CPU, Screen string
	rank             float64
}

type deviceAppsView struct {
	User, System []deviceAppRow
	Window       string // 24h | 7d | 30d
	Windows      []string
	Limited      bool // Standard client: screen time only
	Hidden       int  // host app packages on its own line
}

func (h *Handler) deviceAppsOf(ctx context.Context, d *db.Device, pkgs []db.DevicePackage, host *HostAppView, window string) deviceAppsView {
	if _, ok := appUsageWindows[window]; !ok {
		window = "24h"
	}
	v := deviceAppsView{Window: window, Windows: []string{"24h", "7d", "30d"}, Limited: d.AgentKind == product.KindDPC}
	skip := map[string]bool{}
	if host != nil && host.Count > 0 {
		skip[host.Active.Package] = true
		for _, o := range host.Others {
			skip[o.Package] = true
		}
	}
	use := map[string]db.AppUsageRow{}
	if rows, err := h.db.AppUsage(ctx, d.ID, time.Now().Add(-appUsageWindows[window])); err == nil {
		for _, r := range rows {
			use[r.Package] = r
		}
	}
	for _, p := range launchableOnly(pkgs) {
		if skip[p.PackageName] {
			v.Hidden++
			continue
		}
		row := deviceAppRow{DevicePackage: p, Mem: "—", CPU: "—", Screen: "—"}
		if u, ok := use[p.PackageName]; ok {
			if u.PssAvgMB != nil {
				row.Mem = fmt.Sprintf("%.0f MB", *u.PssAvgMB)
			}
			if u.CPUAvg != nil {
				row.CPU = fmt.Sprintf("%.1f %%", *u.CPUAvg)
			}
			row.Screen = fmtMinutes(u.FgMs)
			switch {
			case v.Limited:
				row.rank = float64(u.FgMs)
			case u.PssAvgMB != nil:
				row.rank = *u.PssAvgMB
			}
		}
		if p.EffectiveSystem {
			v.System = append(v.System, row)
		} else {
			v.User = append(v.User, row)
		}
	}
	rank := func(rows []deviceAppRow) {
		sort.SliceStable(rows, func(i, j int) bool {
			if rows[i].rank != rows[j].rank {
				return rows[i].rank > rows[j].rank
			}
			return strings.ToLower(appRowName(rows[i])) < strings.ToLower(appRowName(rows[j]))
		})
	}
	rank(v.User)
	rank(v.System)
	return v
}

func appRowName(r deviceAppRow) string {
	if r.AppName != "" {
		return r.AppName
	}
	return r.PackageName
}
