package db

import (
	"context"
	"strconv"
)

// Fleet hygiene: the Overview's clean-up checklist. Each job is a count and a Devices
// list filter (DeviceFilter.Hygiene) that shows exactly those devices, where the
// existing bulk actions (retire, assign a restaurant) do the work.

// hygieneWhere is the SQL for one clean-up job over devices aliased d, or "" for none.
func hygieneWhere(job string) string {
	switch job {
	case "silent":
		return "d.last_seen_at < NOW() - INTERVAL '14 days'"
	case "placed-silent":
		return "d.restaurant_id IS NOT NULL AND d.custody_server = '' AND d.last_seen_at < NOW() - INTERVAL '1 day'"
	case "pad":
		return "EXISTS (SELECT 1 FROM alerts ha WHERE ha.device_id = d.id AND ha.type = 'wlc_dead' AND ha.fired_at > NOW() - INTERVAL '14 days')"
	}
	return ""
}

// HygieneJobLabel names a job for the Devices list's filter chip.
func HygieneJobLabel(job string) string {
	switch job {
	case "silent":
		return "Silent 14+ days"
	case "placed-silent":
		return "In a restaurant, not reporting"
	case "pad":
		return "Dead wireless pad (14 days)"
	}
	return ""
}

// FleetHygiene is the clean-up checklist's counts.
type FleetHygiene struct {
	Fleet          int // devices in the fleet (not retired, not hidden)
	Silent         int // not seen for 14+ days
	SilentPlaced   int // …of which in a restaurant
	PlacedSilent   int // in a restaurant, not seen for a day or more
	PlacedSilentAt []string
	DeadPad        int      // devices with a dead wireless pad alert in 14 days
	Builds         int      // distinct builds on devices seen in 14 days
	OddBuilds      []string // …that are not a release, most devices first
}

// GetFleetHygiene counts the clean-up jobs.
func (d *DB) GetFleetHygiene(ctx context.Context) (FleetHygiene, error) {
	var h FleetHygiene
	err := d.pool.QueryRow(ctx, `
		WITH f AS (SELECT * FROM devices d WHERE NOT d.hidden AND d.enrollment_status NOT IN ('retired', 'wiped'))
		SELECT (SELECT COUNT(*) FROM f),
		       (SELECT COUNT(*) FROM f d WHERE `+hygieneWhere("silent")+`),
		       (SELECT COUNT(*) FROM f d WHERE `+hygieneWhere("silent")+` AND d.restaurant_id IS NOT NULL),
		       (SELECT COUNT(*) FROM f d WHERE `+hygieneWhere("pad")+`),
		       (SELECT COUNT(DISTINCT d.build_id) FROM f d WHERE d.last_seen_at > NOW() - INTERVAL '14 days' AND d.build_id <> '')`).
		Scan(&h.Fleet, &h.Silent, &h.SilentPlaced, &h.DeadPad, &h.Builds)
	if err != nil {
		return h, err
	}
	rows, err := d.pool.Query(ctx, `
		SELECT r.name, COUNT(*) FROM devices d JOIN restaurants r ON r.id = d.restaurant_id
		WHERE NOT d.hidden AND d.enrollment_status NOT IN ('retired', 'wiped') AND `+hygieneWhere("placed-silent")+`
		GROUP BY r.name ORDER BY COUNT(*) DESC, r.name`)
	if err != nil {
		return h, err
	}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			rows.Close()
			return h, err
		}
		h.PlacedSilent += n
		h.PlacedSilentAt = append(h.PlacedSilentAt, name+" "+strconv.Itoa(n))
	}
	rows.Close()
	rows, err = d.pool.Query(ctx, `
		SELECT d.build_id FROM devices d
		WHERE NOT d.hidden AND d.enrollment_status NOT IN ('retired', 'wiped') AND d.build_id <> ''
		  AND d.last_seen_at > NOW() - INTERVAL '14 days'
		  AND NOT EXISTS (SELECT 1 FROM releases rl WHERE rl.version = d.build_id)
		GROUP BY d.build_id ORDER BY COUNT(*) DESC, d.build_id`)
	if err != nil {
		return h, err
	}
	defer rows.Close()
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return h, err
		}
		h.OddBuilds = append(h.OddBuilds, b)
	}
	return h, rows.Err()
}
