package db

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	prod "mdm/internal/product"
)

// OverviewScope narrows the Overview's numbers to one device type, one venue, or
// both (C layout, 21 Oct 2026). The zero value is the whole fleet.
type OverviewScope struct {
	Class string
	Venue *uuid.UUID
}

// ScopedDay is one day of the scoped trend behind the Overview's delta cards.
type ScopedDay struct {
	Day        time.Time
	Active     int      // devices with a check-in that day
	LowBattery int      // battery devices whose daily minimum went under 20%
	Hot        int      // devices whose daily max temp reached 45 °C
	BatteryAvg *float32 // mean of battery devices' daily averages
	Crashes    int
}

// ScopedTrend is the last `days` days (oldest first, every day present) plus the
// rolling 24 h crash counts the 24 h window compares.
type ScopedTrend struct {
	Days        []ScopedDay
	Crashes24h  int
	CrashesPrev int // the 24 h before that
}

// crashKindsSQL is what counts as a crash on the Overview: every device event except
// the routine ones, same as GetFleetCrashStats.
const crashKindsSQL = "('reboot', 'kiosk_exit_offline', 'offline_dropped')"

// scopeCTE selects the scoped device set as "scope(id, has_batt)". $2 is the class
// ('' = all), $3 the venue (NULL = all).
func scopeCTE() string {
	return `WITH scope AS (
		SELECT id, (` + prod.BatteryPredicateSQL("devices") + `) AS has_batt
		FROM devices
		WHERE NOT hidden AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')
		  AND ($2::text = '' OR ` + derivedClassSQL() + ` = $2::text)
		  AND ($3::uuid IS NULL OR restaurant_id = $3::uuid)
	)`
}

// OverviewTrend returns the scoped daily trend for the last `days` days.
func (d *DB) OverviewTrend(ctx context.Context, sc OverviewScope, days int) (ScopedTrend, error) {
	var out ScopedTrend
	if days < 2 {
		days = 2
	}
	byDay := map[string]*ScopedDay{}
	today := time.Now().UTC().Truncate(24 * time.Hour)
	for i := days - 1; i >= 0; i-- {
		day := today.AddDate(0, 0, -i)
		out.Days = append(out.Days, ScopedDay{Day: day})
	}
	for i := range out.Days {
		byDay[out.Days[i].Day.Format("2006-01-02")] = &out.Days[i]
	}

	rows, err := d.pool.Query(ctx, scopeCTE()+`
		SELECT s.day,
		       COUNT(*) FILTER (WHERE s.checkin_count > 0),
		       COUNT(*) FILTER (WHERE s.battery_min < 20 AND sc.has_batt),
		       COUNT(*) FILTER (WHERE s.temp_max >= 45),
		       (AVG(s.battery_avg) FILTER (WHERE sc.has_batt))::real
		FROM device_daily_stats s
		JOIN scope sc ON sc.id = s.device_id
		WHERE s.day >= CURRENT_DATE - ($1::int - 1)
		GROUP BY s.day`, days, sc.Class, sc.Venue)
	if err != nil {
		return out, fmt.Errorf("overview trend: %w", err)
	}
	for rows.Next() {
		var day time.Time
		var r ScopedDay
		if err := rows.Scan(&day, &r.Active, &r.LowBattery, &r.Hot, &r.BatteryAvg); err != nil {
			rows.Close()
			return out, err
		}
		if p := byDay[day.Format("2006-01-02")]; p != nil {
			p.Active, p.LowBattery, p.Hot, p.BatteryAvg = r.Active, r.LowBattery, r.Hot, r.BatteryAvg
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	rows, err = d.pool.Query(ctx, scopeCTE()+`
		SELECT (e.occurred_at AT TIME ZONE 'UTC')::date, COUNT(*)
		FROM device_events e
		JOIN scope sc ON sc.id = e.device_id
		WHERE e.kind NOT IN `+crashKindsSQL+`
		  AND e.occurred_at >= CURRENT_DATE - ($1::int - 1)
		GROUP BY 1`, days, sc.Class, sc.Venue)
	if err != nil {
		return out, fmt.Errorf("overview crashes: %w", err)
	}
	for rows.Next() {
		var day time.Time
		var n int
		if err := rows.Scan(&day, &n); err != nil {
			rows.Close()
			return out, err
		}
		if p := byDay[day.Format("2006-01-02")]; p != nil {
			p.Crashes = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	err = d.pool.QueryRow(ctx, scopeCTE()+`
		SELECT COUNT(*) FILTER (WHERE e.occurred_at > NOW() - INTERVAL '24 hours'),
		       COUNT(*) FILTER (WHERE e.occurred_at <= NOW() - INTERVAL '24 hours')
		FROM device_events e
		JOIN scope sc ON sc.id = e.device_id
		WHERE e.kind NOT IN `+crashKindsSQL+`
		  AND e.occurred_at > NOW() - INTERVAL '48 hours' AND $1::int > 0`,
		days, sc.Class, sc.Venue).Scan(&out.Crashes24h, &out.CrashesPrev)
	if err != nil {
		return out, fmt.Errorf("overview crashes 24h: %w", err)
	}
	return out, nil
}

// Rollout is one bar on the Overview's Rollouts timeline: a firmware deployment, or
// the commands that installed one build (an app, a client auto-update) or set lock.
type Rollout struct {
	Kind         string // "ota" or the command type
	Name         string // version, app name or package
	Product      string // OTA only: the release's product key
	Package      string // app_update only
	Who          string // created_by ("auto-update" for the client policy)
	Start, End   time.Time
	Scheduled    *time.Time // OTA with a reboot window still ahead
	Total        int
	Done         int
	Failed       int // failed, plus in-flight past the install deadline
	InFlight     int
	Status       string // OTA: pending / active / complete / cancelled
	DeploymentID int
	ReleaseID    int
}

// RecentRollouts lists rollouts that started in the last `days` days or are still
// running, oldest start first. Command rollouts need two or more devices: a single
// install is a fix, not a rollout.
func (d *DB) RecentRollouts(ctx context.Context, days int) ([]Rollout, error) {
	var out []Rollout
	rows, err := d.pool.Query(ctx, `
		SELECT u.id, COALESCE(u.release_id, 0), COALESCE(rel.version, ''), COALESCE(rel.product, 't7'),
		       u.status, u.created_by, u.created_at, u.scheduled_time,
		       COUNT(ud.device_id),
		       COUNT(*) FILTER (WHERE ud.status = 'installed'),
		       COUNT(*) FILTER (WHERE ud.status = 'failed'),
		       COALESCE(MAX(COALESCE(ud.completed_at, ud.updated_at)), u.created_at)
		FROM updates u
		LEFT JOIN releases rel ON rel.id = u.release_id
		LEFT JOIN update_devices ud ON ud.update_id = u.id
		WHERE u.created_at >= NOW() - make_interval(days => $1::int)
		   OR u.status IN ('pending', 'active')
		GROUP BY u.id, rel.version, rel.product`, days)
	if err != nil {
		return nil, fmt.Errorf("rollouts ota: %w", err)
	}
	for rows.Next() {
		var r Rollout
		r.Kind = "ota"
		if err := rows.Scan(&r.DeploymentID, &r.ReleaseID, &r.Name, &r.Product, &r.Status, &r.Who,
			&r.Start, &r.Scheduled, &r.Total, &r.Done, &r.Failed, &r.End); err != nil {
			rows.Close()
			return nil, err
		}
		if r.Status == "pending" || r.Status == "active" {
			r.InFlight = r.Total - r.Done - r.Failed
		}
		out = append(out, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}

	// Commands, one rollout per (type, who, build). In-flight past the install deadline
	// (15 min) counts as failed, the same expiry the Jobs page applies.
	rows, err = d.pool.Query(ctx, `
		SELECT c.type, c.created_by,
		       COALESCE(NULLIF(c.payload->>'package', ''), '') AS pkg,
		       COALESCE(NULLIF(COALESCE(c.payload->>'version', ''), ''), NULLIF(MAX(a.name), ''), c.apk_url) AS name,
		       MIN(c.created_at), MAX(GREATEST(c.created_at, cs.updated_at)),
		       COUNT(*),
		       COUNT(*) FILTER (WHERE cs.status = 'completed'),
		       COUNT(*) FILTER (WHERE cs.status IN ('failed', 'expired', 'cancelled')
		                          OR (cs.status NOT IN ('completed', 'failed', 'expired', 'cancelled')
		                              AND c.created_at <= NOW() - INTERVAL '15 minutes')),
		       COUNT(*) FILTER (WHERE cs.status NOT IN ('completed', 'failed', 'expired', 'cancelled')
		                          AND c.created_at > NOW() - INTERVAL '15 minutes')
		FROM commands c
		JOIN command_status cs ON cs.command_id = c.id
		LEFT JOIN apps a ON a.apk_url = c.apk_url AND c.apk_url <> ''
		WHERE c.created_at >= NOW() - make_interval(days => $1::int)
		  AND (c.type IN `+prod.InstallShapedSQL()+` OR c.type IN ('set_kiosk', 'kiosk_set'))
		GROUP BY c.type, c.created_by, pkg, COALESCE(c.payload->>'version', ''), c.apk_url
		HAVING COUNT(*) >= 2`, days)
	if err != nil {
		return nil, fmt.Errorf("rollouts commands: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var r Rollout
		if err := rows.Scan(&r.Kind, &r.Who, &r.Package, &r.Name, &r.Start, &r.End,
			&r.Total, &r.Done, &r.Failed, &r.InFlight); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
