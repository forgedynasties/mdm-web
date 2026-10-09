package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Service uptime: were a restaurant's devices up while it was open? For each of the last
// days, the restaurant's opening hours (its service window, else the fleet default, in
// its local time) are cut into uptimeStep steps, and each placed device counts as up in a
// step when it sent at least one sample in it. Uptime is up device-steps over expected
// device-steps. The step is coarse on purpose: HTTP-only agents report every few minutes,
// and a per-minute count would call their quiet minutes an outage.

// uptimeStep is the resolution of the uptime count.
const uptimeStep = 15 * time.Minute

// uptimeDormant leaves out devices silent longer than this: a unit in a drawer for weeks
// is an inventory question (see the wall's dormant state), not a restaurant outage.
const uptimeDormant = 14 * 24 * time.Hour

// UptimeDay is one restaurant-day of service uptime.
type UptimeDay struct {
	Date       time.Time // local midnight
	Expected   int       // device-steps inside opening hours (so far, for today)
	Up         int
	DownDevice int // devices down for at least one step
}

// Pct is the day's uptime in percent; -1 when nothing was expected (closed, or not
// open yet today).
func (u UptimeDay) Pct() float64 {
	if u.Expected == 0 {
		return -1
	}
	return float64(u.Up) * 100 / float64(u.Expected)
}

// RestaurantUptime is one restaurant's uptime over the last days, oldest day first.
type RestaurantUptime struct {
	ID       uuid.UUID
	Name     string
	TZ       string
	OpenMin  int
	CloseMin int
	Devices  []uuid.UUID
	Days     []UptimeDay
}

// Pct is the whole period's uptime; -1 when nothing was expected.
func (r RestaurantUptime) Pct() float64 {
	var e, u int
	for _, d := range r.Days {
		e += d.Expected
		u += d.Up
	}
	if e == 0 {
		return -1
	}
	return float64(u) * 100 / float64(e)
}

// ServiceUptime computes each restaurant's service uptime for the last days (today
// included, up to now).
func (d *DB) ServiceUptime(ctx context.Context, days int, now time.Time) ([]RestaurantUptime, error) {
	fleet, err := d.GetFleetServiceWindow(ctx)
	if err != nil {
		return nil, err
	}
	own, err := d.ListRestaurantServiceWindows(ctx)
	if err != nil {
		return nil, err
	}
	// Placed devices, with the timezone they report (a restaurant's window has no zone
	// of its own unless one was set, so its devices' zone is the restaurant's).
	rows, err := d.pool.Query(ctx, `
		SELECT r.id, r.name, dv.id, dv.created_at, COALESCE(dv.latest_extra->>'timezone', '')
		FROM restaurants r
		JOIN devices dv ON dv.restaurant_id = r.id
		WHERE NOT dv.hidden AND dv.enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')
		  AND dv.custody_server = ''
		  AND dv.last_seen_at > $1
		ORDER BY r.name`, now.Add(-uptimeDormant))
	if err != nil {
		return nil, err
	}
	var out []RestaurantUptime
	idx := map[uuid.UUID]int{}
	created := map[uuid.UUID]time.Time{}
	tzVotes := map[uuid.UUID]map[string]int{}
	var all []uuid.UUID
	for rows.Next() {
		var rid, did uuid.UUID
		var name, tz string
		var c time.Time
		if err := rows.Scan(&rid, &name, &did, &c, &tz); err != nil {
			rows.Close()
			return nil, err
		}
		i, ok := idx[rid]
		if !ok {
			i = len(out)
			idx[rid] = i
			out = append(out, RestaurantUptime{ID: rid, Name: name})
			tzVotes[rid] = map[string]int{}
		}
		out[i].Devices = append(out[i].Devices, did)
		created[did] = c
		if tz != "" {
			tzVotes[rid][tz]++
		}
		all = append(all, did)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(out) == 0 {
		return nil, nil
	}
	// Steps each device reported in, over the period plus a day of slack for zones.
	from := now.Add(-time.Duration(days+1) * 24 * time.Hour)
	step := int64(uptimeStep / time.Second)
	srows, err := d.pool.Query(ctx, `
		SELECT device_id, (EXTRACT(EPOCH FROM at)::bigint / $3) AS b
		FROM device_samples
		WHERE device_id = ANY($1) AND at > $2
		GROUP BY 1, 2`, all, from, step)
	if err != nil {
		return nil, err
	}
	up := map[uuid.UUID]map[int64]bool{}
	for srows.Next() {
		var id uuid.UUID
		var b int64
		if err := srows.Scan(&id, &b); err != nil {
			srows.Close()
			return nil, err
		}
		if up[id] == nil {
			up[id] = map[int64]bool{}
		}
		up[id][b] = true
	}
	srows.Close()
	if err := srows.Err(); err != nil {
		return nil, err
	}
	for i := range out {
		r := &out[i]
		w := fleet
		if ow, ok := own[r.ID]; ok {
			w = ow
		}
		r.TZ = w.TZ
		if r.TZ == "" {
			best := 0
			for tz, n := range tzVotes[r.ID] {
				if n > best || (n == best && tz < r.TZ) {
					r.TZ, best = tz, n
				}
			}
		}
		r.OpenMin, r.CloseMin = w.OpenMin, w.CloseMin
		r.Days = uptimeDays(now, days, r.TZ, w.OpenMin, w.CloseMin, r.Devices, created, up, step)
	}
	return out, nil
}

// uptimeDays counts one restaurant's device-steps per local day. A window whose close
// is at or before its open runs past midnight and is counted on the day it opened.
func uptimeDays(now time.Time, days int, tz string, openMin, closeMin int, devs []uuid.UUID,
	created map[uuid.UUID]time.Time, up map[uuid.UUID]map[int64]bool, step int64) []UptimeDay {
	loc := time.UTC
	if tz != "" {
		if l, err := time.LoadLocation(tz); err == nil {
			loc = l
		}
	}
	span := closeMin - openMin
	if span <= 0 {
		span += 1440
	}
	local := now.In(loc)
	today := time.Date(local.Year(), local.Month(), local.Day(), 0, 0, 0, 0, loc)
	out := make([]UptimeDay, 0, days)
	for k := days - 1; k >= 0; k-- {
		day := today.AddDate(0, 0, -k)
		ud := UptimeDay{Date: day}
		open := day.Add(time.Duration(openMin) * time.Minute)
		end := open.Add(time.Duration(span) * time.Minute)
		if end.After(now) {
			end = now
		}
		down := map[uuid.UUID]bool{}
		// Whole steps only: a step counts once it has fully passed.
		for b := (open.Unix() + step - 1) / step; (b+1)*step <= end.Unix(); b++ {
			at := time.Unix(b*step, 0)
			for _, id := range devs {
				if created[id].After(at) {
					continue
				}
				ud.Expected++
				if up[id][b] {
					ud.Up++
				} else {
					down[id] = true
				}
			}
		}
		ud.DownDevice = len(down)
		out = append(out, ud)
	}
	return out
}
