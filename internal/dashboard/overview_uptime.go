package dashboard

import (
	"context"
	"fmt"
	"log"
	"sync"
	"time"

	"mdm/internal/db"
)

// Service uptime card on the Overview: per restaurant, the last week of opening hours,
// one cell a day, and the week's figure (see db.ServiceUptime for the counting).

const (
	uptimeDays     = 7
	uptimeCacheTTL = 10 * time.Minute
)

// uptimeCache holds the fleet's uptime between computations: it reads a week of
// samples, which is too much for every Overview load. A stale copy is served while a
// fresh one is computed in the background.
var uptimeCache struct {
	sync.Mutex
	at   time.Time
	rows []db.RestaurantUptime
	busy bool
}

func (h *Handler) serviceUptime(ctx context.Context) []db.RestaurantUptime {
	uptimeCache.Lock()
	rows, at, busy := uptimeCache.rows, uptimeCache.at, uptimeCache.busy
	stale := time.Since(at) > uptimeCacheTTL
	if stale && !busy && !at.IsZero() {
		uptimeCache.busy = true
	}
	uptimeCache.Unlock()
	if !stale {
		return rows
	}
	refresh := func(ctx context.Context) []db.RestaurantUptime {
		fresh, err := h.db.ServiceUptime(ctx, uptimeDays, time.Now())
		uptimeCache.Lock()
		defer uptimeCache.Unlock()
		uptimeCache.busy = false
		if err != nil {
			log.Printf("[uptime] %v", err)
			return uptimeCache.rows
		}
		uptimeCache.rows, uptimeCache.at = fresh, time.Now()
		return fresh
	}
	if at.IsZero() {
		return refresh(ctx) // first load: nothing to show yet, so wait for it
	}
	if !busy {
		go refresh(context.Background())
	}
	return rows
}

type uptimeRow struct {
	ID    string
	Name  string
	Pct   string
	Class string
	Days  []uptimeCell
}

type uptimeCell struct {
	Class string // ok | warn | bad | none
	Title string
}

// uptimeClass grades a percentage: 99 and up is fine, under 95 is an outage.
func uptimeClass(p float64) string {
	switch {
	case p < 0:
		return "none"
	case p >= 99:
		return "ok"
	case p >= 95:
		return "warn"
	}
	return "bad"
}

func uptimeRows(rs []db.RestaurantUptime, visible func(db.RestaurantUptime) bool) []uptimeRow {
	var out []uptimeRow
	for _, r := range rs {
		if visible != nil && !visible(r) {
			continue
		}
		row := uptimeRow{ID: r.ID.String(), Name: r.Name, Pct: "—", Class: uptimeClass(r.Pct())}
		if p := r.Pct(); p >= 0 {
			row.Pct = fmt.Sprintf("%.1f%%", p)
		}
		for _, d := range r.Days {
			c := uptimeCell{Class: uptimeClass(d.Pct())}
			day := d.Date.Format("Mon 2 Jan")
			switch {
			case d.Pct() < 0:
				c.Title = day + " · not open yet"
			case d.DownDevice == 0:
				c.Title = fmt.Sprintf("%s · %.1f%% up · every device reporting", day, d.Pct())
			default:
				c.Title = fmt.Sprintf("%s · %.1f%% up · %d device%s missed time", day, d.Pct(), d.DownDevice, plural(d.DownDevice))
			}
			row.Days = append(row.Days, c)
		}
		out = append(out, row)
	}
	return out
}
