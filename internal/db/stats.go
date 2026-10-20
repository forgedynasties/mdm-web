package db

import (
	"context"
	"sort"
	"sync"
	"time"

	"github.com/google/uuid"
)

// HourlyOnline returns, for the last `hours` hours ending now (UTC hour
// buckets, oldest first), how many of the given devices checked in during
// each hour. Read from device_samples, which gets a row under exactly the condition
// a check-in history row used to be stored, so the counts are the same; the BRIN index
// on `at` bounds the scan.
func (d *DB) HourlyOnline(ctx context.Context, ids []uuid.UUID, hours int) ([]int, error) {
	out := make([]int, hours)
	if len(ids) == 0 {
		return out, nil
	}
	start := time.Now().UTC().Truncate(time.Hour).Add(-time.Duration(hours-1) * time.Hour)
	rows, err := d.pool.Query(ctx, `
		SELECT date_trunc('hour', at), COUNT(DISTINCT device_id)
		FROM device_samples WHERE device_id = ANY($1) AND at >= $2
		GROUP BY 1`, ids, start)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var t time.Time
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return nil, err
		}
		i := int(t.UTC().Sub(start) / time.Hour)
		if i >= 0 && i < hours {
			out[i] = n
		}
	}
	return out, rows.Err()
}

// IncidentCounts returns crash/ANR events per device in the last `days` days
// and the totals for that period and the period before it.
func (d *DB) IncidentCounts(ctx context.Context, ids []uuid.UUID, days int) (perDevice map[uuid.UUID]int, current, previous int, err error) {
	perDevice = map[uuid.UUID]int{}
	if len(ids) == 0 {
		return perDevice, 0, 0, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT device_id,
		       COUNT(*) FILTER (WHERE occurred_at >= NOW() - ($2 * INTERVAL '1 day')),
		       COUNT(*) FILTER (WHERE occurred_at <  NOW() - ($2 * INTERVAL '1 day'))
		FROM device_events
		WHERE device_id = ANY($1) AND kind NOT IN ('reboot', 'kiosk_exit_offline', 'offline_dropped')
		  AND occurred_at >= NOW() - (2 * $2 * INTERVAL '1 day')
		GROUP BY device_id`, ids, days)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var cur, prev int
		if err := rows.Scan(&id, &cur, &prev); err != nil {
			return nil, 0, 0, err
		}
		perDevice[id] = cur
		current += cur
		previous += prev
	}
	return perDevice, current, previous, rows.Err()
}


// DeviceDailyStat is one rolled-up day of telemetry for a device. Aggregate columns
// are pointers so a day with no data for a metric serializes as null rather than 0.
type DeviceDailyStat struct {
	Day           time.Time `json:"day"`
	CheckinCount  int       `json:"checkin_count"`
	BatteryMin    *int      `json:"battery_min"`
	BatteryMax    *int      `json:"battery_max"`
	BatteryAvg    *float32  `json:"battery_avg"`
	TempMax       *float32  `json:"temp_max"`
	RAMPctPeak    *int      `json:"ram_pct_peak"`
	ChargingFrac  *float32  `json:"charging_frac"`
	OnlineMinutes int       `json:"online_minutes"`
	BuildID       string    `json:"build_id"`
}

// GetDeviceDailyStats returns the last `days` days of rolled-up stats for a device,
// oldest first. Defaults to 30 days.
func (d *DB) GetDeviceDailyStats(ctx context.Context, deviceID uuid.UUID, days int) ([]DeviceDailyStat, error) {
	if days <= 0 {
		days = 30
	}
	rows, err := d.pool.Query(ctx, `
		SELECT day, checkin_count, battery_min, battery_max, battery_avg,
		       temp_max, ram_pct_peak, charging_frac, online_minutes, build_id
		FROM device_daily_stats
		WHERE device_id = $1 AND day >= CURRENT_DATE - ($2::int - 1)
		ORDER BY day`, deviceID, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []DeviceDailyStat
	for rows.Next() {
		var s DeviceDailyStat
		if err := rows.Scan(&s.Day, &s.CheckinCount, &s.BatteryMin, &s.BatteryMax,
			&s.BatteryAvg, &s.TempMax, &s.RAMPctPeak, &s.ChargingFrac,
			&s.OnlineMinutes, &s.BuildID); err != nil {
			return nil, err
		}
		stats = append(stats, s)
	}
	return stats, rows.Err()
}

// WrappedStat is one "superlative" in the Fleet Wrapped review: a device (or
// venue) that tops some metric, with the value and a bit of context.
type WrappedStat struct {
	Serial     string  `json:"serial"`
	Restaurant string  `json:"restaurant"`
	Value      float64 `json:"value"`
	Extra      string  `json:"extra"` // e.g. the date it happened
}

// FleetWrapped is the "Spotify Wrapped for devices" review — playful all-time
// superlatives and totals for the whole fleet, computed from the rolled-up daily
// stats plus lifetime device counters, commands, and alerts.
type FleetWrapped struct {
	HasData         bool
	FirstDay        time.Time
	LastDay         time.Time
	DaysCovered     int
	DeviceCount     int
	RestaurantCount int

	TotalCheckins int64
	OnlineMinutes int64
	TotalCycles   float64 // fleet-wide lifetime full battery cycles (discharge/100)
	PeakRAM       int     // highest RAM pressure ever seen, %

	TotalCommands  int64
	TopCommandType string
	TotalAlerts    int64
	TopAlertType   string

	HardestWorker WrappedStat // most online minutes, all-time
	Busiest       WrappedStat // most check-ins, all-time
	Hottest       WrappedStat // highest temperature ever recorded
	MostWorn      WrappedStat // most lifetime battery cycles
	LongestDay    WrappedStat // most online minutes in a single day
	PadLover      WrappedStat // highest average charging coverage
	Veteran       WrappedStat // earliest device in the fleet
	BusiestVenue  WrappedStat // restaurant with the most check-ins (Serial holds the name)
}

// GetFleetWrapped computes the all-time Fleet Wrapped review. Every query excludes
// hidden (retired) devices. Superlatives left at their zero value simply don't
// render — the page guards on HasData for the overall empty case.
func (d *DB) GetFleetWrapped(ctx context.Context) (FleetWrapped, error) {
	var w FleetWrapped

	// Fleet-wide totals + span from the daily rollups.
	var first, last *time.Time
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT s.day), MIN(s.day), MAX(s.day),
		       COALESCE(SUM(s.checkin_count),0), COALESCE(SUM(s.online_minutes),0),
		       COALESCE(MAX(s.ram_pct_peak),0)
		FROM device_daily_stats s JOIN devices d ON d.id = s.device_id
		WHERE NOT d.hidden`).
		Scan(&w.DaysCovered, &first, &last, &w.TotalCheckins, &w.OnlineMinutes, &w.PeakRAM)
	if err != nil {
		return w, err
	}
	if first != nil {
		w.FirstDay = *first
	}
	if last != nil {
		w.LastDay = *last
	}
	w.HasData = w.TotalCheckins > 0

	// The 15 queries below (2 counts, 1 sum, 2 group-bys, 10 superlative "scanStat"
	// lookups) are each a single independent aggregate over a different slice of
	// the fleet's history — none depends on another's result. Firing them
	// sequentially meant this page's latency was the sum of 15 round trips. Run
	// them concurrently and write into local vars; results are only copied into `w`
	// after every goroutine has finished, so there's no concurrent access to it.
	var totalDischarge, totalCommands, totalAlerts int64
	var deviceCount, restaurantCount int
	var topCommandType, topAlertType string
	var hardestWorker, busiest, hottest, longestDay, padLover, mostWorn, veteran, busiestVenue WrappedStat

	// scanStat runs a superlative query returning (serial, restaurant, value, extra)
	// and tolerates no rows (leaves the stat zero).
	scanStat := func(q string) WrappedStat {
		var s WrappedStat
		_ = d.pool.QueryRow(ctx, q).Scan(&s.Serial, &s.Restaurant, &s.Value, &s.Extra)
		return s
	}

	var wg sync.WaitGroup
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}

	run(func() { _ = d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM devices WHERE NOT hidden`).Scan(&deviceCount) })
	run(func() { _ = d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM restaurants`).Scan(&restaurantCount) })
	run(func() {
		_ = d.pool.QueryRow(ctx, `SELECT COALESCE(SUM(discharge_total_pct),0) FROM devices WHERE NOT hidden`).Scan(&totalDischarge)
	})
	run(func() { _ = d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM commands`).Scan(&totalCommands) })
	run(func() {
		_ = d.pool.QueryRow(ctx, `SELECT type FROM commands GROUP BY type ORDER BY COUNT(*) DESC, type LIMIT 1`).Scan(&topCommandType)
	})
	run(func() { _ = d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM alerts`).Scan(&totalAlerts) })
	run(func() {
		_ = d.pool.QueryRow(ctx, `SELECT type FROM alerts GROUP BY type ORDER BY COUNT(*) DESC, type LIMIT 1`).Scan(&topAlertType)
	})
	run(func() {
		hardestWorker = scanStat(`
			SELECT d.serial_number, COALESCE(r.name,''), SUM(s.online_minutes)::float8, ''
			FROM device_daily_stats s JOIN devices d ON d.id = s.device_id
			LEFT JOIN restaurants r ON r.id = d.restaurant_id
			WHERE NOT d.hidden GROUP BY d.id, d.serial_number, r.name
			ORDER BY 3 DESC LIMIT 1`)
	})
	run(func() {
		busiest = scanStat(`
			SELECT d.serial_number, COALESCE(r.name,''), SUM(s.checkin_count)::float8, ''
			FROM device_daily_stats s JOIN devices d ON d.id = s.device_id
			LEFT JOIN restaurants r ON r.id = d.restaurant_id
			WHERE NOT d.hidden GROUP BY d.id, d.serial_number, r.name
			ORDER BY 3 DESC LIMIT 1`)
	})
	run(func() {
		hottest = scanStat(`
			SELECT d.serial_number, COALESCE(r.name,''), s.temp_max::float8, to_char(s.day,'Mon DD')
			FROM device_daily_stats s JOIN devices d ON d.id = s.device_id
			LEFT JOIN restaurants r ON r.id = d.restaurant_id
			WHERE NOT d.hidden AND s.temp_max IS NOT NULL
			ORDER BY s.temp_max DESC LIMIT 1`)
	})
	run(func() {
		longestDay = scanStat(`
			SELECT d.serial_number, COALESCE(r.name,''), s.online_minutes::float8, to_char(s.day,'Mon DD')
			FROM device_daily_stats s JOIN devices d ON d.id = s.device_id
			LEFT JOIN restaurants r ON r.id = d.restaurant_id
			WHERE NOT d.hidden ORDER BY s.online_minutes DESC LIMIT 1`)
	})
	run(func() {
		padLover = scanStat(`
			SELECT d.serial_number, COALESCE(r.name,''), (AVG(s.charging_frac)*100)::float8, ''
			FROM device_daily_stats s JOIN devices d ON d.id = s.device_id
			LEFT JOIN restaurants r ON r.id = d.restaurant_id
			WHERE NOT d.hidden AND s.charging_frac IS NOT NULL
			GROUP BY d.id, d.serial_number, r.name HAVING COUNT(*) >= 3
			ORDER BY 3 DESC LIMIT 1`)
	})
	run(func() {
		mostWorn = scanStat(`
			SELECT d.serial_number, COALESCE(r.name,''), (d.discharge_total_pct::float8/100), ''
			FROM devices d LEFT JOIN restaurants r ON r.id = d.restaurant_id
			WHERE NOT d.hidden ORDER BY d.discharge_total_pct DESC LIMIT 1`)
	})
	run(func() {
		veteran = scanStat(`
			SELECT d.serial_number, COALESCE(r.name,''), 0::float8, to_char(d.created_at,'Mon DD, YYYY')
			FROM devices d LEFT JOIN restaurants r ON r.id = d.restaurant_id
			WHERE NOT d.hidden ORDER BY d.created_at ASC LIMIT 1`)
	})
	run(func() {
		// Busiest venue: name lands in Serial (WrappedStat has no venue field), Value = check-ins.
		busiestVenue = scanStat(`
			SELECT r.name, '', SUM(s.checkin_count)::float8, COUNT(DISTINCT d.id)::text
			FROM restaurants r JOIN devices d ON d.restaurant_id = r.id AND NOT d.hidden
			JOIN device_daily_stats s ON s.device_id = d.id
			GROUP BY r.id, r.name ORDER BY 3 DESC LIMIT 1`)
	})
	wg.Wait()

	w.DeviceCount = deviceCount
	w.RestaurantCount = restaurantCount
	w.TotalCycles = float64(totalDischarge) / 100
	w.TotalCommands = totalCommands
	w.TopCommandType = topCommandType
	w.TotalAlerts = totalAlerts
	w.TopAlertType = topAlertType
	w.HardestWorker = hardestWorker
	w.Busiest = busiest
	w.Hottest = hottest
	w.LongestDay = longestDay
	w.PadLover = padLover
	w.MostWorn = mostWorn
	w.Veteran = veteran
	w.BusiestVenue = busiestVenue

	return w, nil
}

// GroupDailyStat is one day of stats aggregated across all devices in a group.
// Aggregates are pointers so a day/metric with no data serializes as null, not 0.
type GroupDailyStat struct {
	Day            time.Time `json:"day"`
	DeviceCount    int       `json:"device_count"`    // devices with data that day
	BatteryMin     *int      `json:"battery_min"`     // lowest daily min across the group
	BatteryMax     *int      `json:"battery_max"`     // highest daily max across the group
	BatteryAvg     *float32  `json:"battery_avg"`     // mean of per-device daily averages
	TempMax        *float32  `json:"temp_max"`        // hottest device that day
	ChargingFrac   *float32  `json:"charging_frac"`   // mean charging coverage
	OnlineMinAvg   *float32  `json:"online_min_avg"`  // mean online minutes per device
	DistinctBuilds int       `json:"distinct_builds"` // build_id spread (fleet consistency)
}

// GetGroupDailyStats returns the last `days` days of stats rolled up across every
// device in the group, oldest first. Defaults to 30 days. Groups are treated as
// generic device buckets — no assumption about what a group represents.
func (d *DB) GetGroupDailyStats(ctx context.Context, groupID uuid.UUID, days int) ([]GroupDailyStat, error) {
	if days <= 0 {
		days = 30
	}
	rows, err := d.pool.Query(ctx, `
		SELECT
			s.day,
			COUNT(DISTINCT s.device_id),
			MIN(s.battery_min),
			MAX(s.battery_max),
			AVG(s.battery_avg)::real,
			MAX(s.temp_max)::real,
			AVG(s.charging_frac)::real,
			AVG(s.online_minutes)::real,
			COUNT(DISTINCT NULLIF(s.build_id, ''))
		FROM device_daily_stats s
		JOIN device_groups dg ON dg.device_id = s.device_id
		WHERE dg.group_id = $1 AND s.day >= CURRENT_DATE - ($2::int - 1)
		GROUP BY s.day
		ORDER BY s.day`, groupID, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []GroupDailyStat
	for rows.Next() {
		var s GroupDailyStat
		if err := rows.Scan(&s.Day, &s.DeviceCount, &s.BatteryMin, &s.BatteryMax,
			&s.BatteryAvg, &s.TempMax, &s.ChargingFrac, &s.OnlineMinAvg,
			&s.DistinctBuilds); err != nil {
			return nil, err
		}
		stats = append(stats, s)
	}
	return stats, rows.Err()
}

// GroupHealth is a per-group health scorecard (Tier 3). Pointer fields are null when
// the group has no rolled-up data in the window. Score/ScoreClass are computed in Go.
type GroupHealth struct {
	GroupID        uuid.UUID `json:"group_id"`
	Name           string    `json:"name"`
	DeviceCount    int       `json:"device_count"`
	OfflineCount   int       `json:"offline_count"`
	// DormantCount are devices that have not been heard from in over two weeks. They
	// are offline, but they are not today's outage: a device dark since last month is
	// an inventory question (where is it? was it returned? is it on a shelf?), and
	// scoring a venue as failing every day for the same absent hardware buries the
	// venue that actually lost a device this morning.
	DormantCount   int       `json:"dormant_count"`
	OpenCritical   int       `json:"open_critical"`
	OpenWarning    int       `json:"open_warning"`
	BatteryAvg     *float64  `json:"battery_avg"`     // recent avg daily peak battery (overnight fullness)
	BatteryDelta   *float64  `json:"battery_delta"`   // recent minus prior week (negative = declining)
	ChargingAvg    *float64  `json:"charging_avg"`    // recent avg charging coverage (0-1)
	TempMax        *float64  `json:"temp_max"`        // hottest device in the window
	TempMaxSerial  *string   `json:"temp_max_serial"` // serial of the device that hit TempMax
	DistinctBuilds int       `json:"distinct_builds"`
	Deployed       bool      `json:"deployed"`       // true for restaurants (a venue); false for tag-groups
	DeployedCount  int       `json:"deployed_count"` // devices that are deployed (assigned to a restaurant)
	Score          int       `json:"score"`          // 0-100, higher is healthier
	ScoreClass     string    `json:"score_class"`    // ok | warn | danger (for badge styling)
}

// healthScore derives a 0-100 score and class from a group's metrics. Heuristic and
// explainable: start at 100 and subtract penalties for offline devices, open alerts,
// poor charging, battery decline, overheating, and firmware fragmentation.
func (g *GroupHealth) computeScore() {
	score := 100
	if g.DeviceCount == 0 {
		g.Score, g.ScoreClass = 100, "ok"
		return
	}
	dev := float64(g.DeviceCount)
	// Every penalty is a SHARE of the venue, not a raw count, and every one is capped.
	// Flat per-alert penalties were the bug that made this useless: at -15 a critical
	// and -4 a warning, seven open alerts zeroed a venue whatever else was true, and a
	// score that is 0 for a fleet that is mostly fine tells nobody anything. A venue
	// where one device in nine is offline and a couple of alerts are open is a normal
	// Tuesday — it should read in the seventies, not fail.
	// Only devices that went quiet recently: see DormantCount.
	recentlyOffline := g.OfflineCount - g.DormantCount
	if recentlyOffline < 0 {
		recentlyOffline = 0
	}
	score -= capPenalty(float64(recentlyOffline)/dev*25, 25)
	// Dormant hardware is worth a nudge, not a failing grade.
	score -= capPenalty(float64(g.DormantCount)/dev*8, 4)
	score -= capPenalty(float64(g.OpenCritical)/dev*30, 18)
	score -= capPenalty(float64(g.OpenWarning)/dev*10, 6)
	// The environmental checks describe a venue that needs a habit changed, not one
	// that is failing to serve customers. They nudge; they do not decide the grade.
	if g.ChargingAvg != nil && *g.ChargingAvg < 0.3 {
		score -= 6
	}
	if g.BatteryDelta != nil && *g.BatteryDelta < -10 {
		score -= 6
	}
	if g.TempMax != nil && *g.TempMax >= 45 {
		score -= 6
	}
	if g.DistinctBuilds > 1 {
		// Fragmentation is worth noticing and never worth a failing grade on its own:
		// a fleet mid-rollout legitimately runs several builds for days.
		score -= capPenalty(float64(g.DistinctBuilds-1)*3, 6)
	}
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	g.Score = score
	switch {
	case score >= 70:
		g.ScoreClass = "ok"
	case score >= 40:
		g.ScoreClass = "warn"
	default:
		g.ScoreClass = "danger"
	}
}

// Rescore recomputes Score and ScoreClass from the metrics already set, for health
// built outside the database (the dashboard's sneak-peek sample).
func (g *GroupHealth) Rescore() { g.computeScore() }

// capPenalty rounds a penalty and holds it to a ceiling, so no single dimension can
// sink a score by itself. Kept here rather than inlined because health_explain.go
// mirrors these numbers exactly, and the two drifting apart is how a score stops
// matching the reasons shown underneath it.
func capPenalty(v, max float64) int {
	if v > max {
		v = max
	}
	if v < 0 {
		return 0
	}
	return int(v + 0.5)
}

// GetGroupHealth returns a health scorecard per group, worst score first. activeSecs is
// the offline threshold (a device quieter than this counts as offline). Recent window is
// the last 7 days; battery delta compares it to the prior 7 days.
// GetGroupHealth rolls up per-group health. offline_count is devices WITHOUT a live
// WebSocket (from the connected set), matching the dashboard's WS-based online status.
func (d *DB) GetGroupHealth(ctx context.Context, connected []uuid.UUID) ([]GroupHealth, error) {
	rows, err := d.pool.Query(ctx, `
		WITH recent AS (
			SELECT dg.group_id,
				AVG(s.battery_max)   AS battery_avg,
				AVG(s.charging_frac) AS charging_avg,
				MAX(s.temp_max)      AS temp_max,
				COUNT(DISTINCT NULLIF(s.build_id, '')) AS builds
			FROM device_daily_stats s
			JOIN device_groups dg ON dg.device_id = s.device_id
			WHERE s.day > CURRENT_DATE - 7
			GROUP BY dg.group_id
		),
		prior AS (
			SELECT dg.group_id, AVG(s.battery_max) AS battery_avg
			FROM device_daily_stats s
			JOIN device_groups dg ON dg.device_id = s.device_id
			WHERE s.day <= CURRENT_DATE - 7 AND s.day > CURRENT_DATE - 14
			GROUP BY dg.group_id
		),
		devs AS (
			SELECT dg.group_id,
				COUNT(*) AS device_count,
				-- Offline means "not connected and not accounted for". A device that is
				-- reporting to another MDM is neither: it is somewhere else, doing fine,
				-- and counting it as an outage penalises a venue for a move someone
				-- made on purpose. See internal/db/custody.go.
				COUNT(*) FILTER (WHERE d.id <> ALL($1::uuid[]) AND d.custody_server = '') AS offline_count,
				COUNT(*) FILTER (WHERE d.id <> ALL($1::uuid[]) AND d.custody_server = ''
				                   AND d.last_seen_at < NOW() - INTERVAL '14 days') AS dormant_count,
				COUNT(*) FILTER (WHERE d.restaurant_id IS NOT NULL) AS deployed_count
			FROM device_groups dg
			JOIN devices d ON d.id = dg.device_id AND NOT d.hidden
			GROUP BY dg.group_id
		),
		al AS (
			SELECT dg.group_id,
				COUNT(*) FILTER (WHERE a.severity = 'critical')  AS crit,
				COUNT(*) FILTER (WHERE a.severity <> 'critical') AS warn
			FROM alerts a
			JOIN device_groups dg ON dg.device_id = a.device_id
			JOIN devices d ON d.id = a.device_id AND NOT d.hidden
			WHERE a.status <> 'resolved'
			GROUP BY dg.group_id
		)
		SELECT g.id, g.name,
			COALESCE(devs.device_count, 0), COALESCE(devs.offline_count, 0), COALESCE(devs.dormant_count, 0),
			COALESCE(al.crit, 0), COALESCE(al.warn, 0),
			recent.battery_avg, (recent.battery_avg - prior.battery_avg),
			recent.charging_avg, recent.temp_max, COALESCE(recent.builds, 0),
			false, COALESCE(devs.deployed_count, 0)  -- a group is a tag, not a venue
		FROM groups g
		LEFT JOIN devs   ON devs.group_id   = g.id
		LEFT JOIN recent ON recent.group_id = g.id
		LEFT JOIN prior  ON prior.group_id  = g.id
		LEFT JOIN al     ON al.group_id     = g.id
		ORDER BY g.name`, connected)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []GroupHealth
	for rows.Next() {
		var g GroupHealth
		if err := rows.Scan(&g.GroupID, &g.Name, &g.DeviceCount, &g.OfflineCount, &g.DormantCount,
			&g.OpenCritical, &g.OpenWarning, &g.BatteryAvg, &g.BatteryDelta,
			&g.ChargingAvg, &g.TempMax, &g.DistinctBuilds, &g.Deployed, &g.DeployedCount); err != nil {
			return nil, err
		}
		g.computeScore()
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Worst (lowest score) first so the page surfaces venues needing attention.
	sort.Slice(out, func(i, j int) bool { return out[i].Score < out[j].Score })
	return out, nil
}

// FleetDailyStat is one day of stats aggregated across the whole (non-hidden)
// fleet, feeding the overview page sparklines. Aggregated from device_daily_stats,
// so "today" reflects the last rollup, not the live minute.
type FleetDailyStat struct {
	Day        time.Time `json:"day"`
	Active     int       `json:"active"`      // devices with at least one checkin that day
	LowBattery int       `json:"low_battery"` // devices whose daily minimum dipped under 20%
	Hot        int       `json:"hot"`         // devices whose daily max temp reached 45°C
	BatteryAvg *float32  `json:"battery_avg"` // mean of per-device daily averages
	Checkins   int64     `json:"checkins"`    // total check-ins across the fleet that day
}

// GetFleetDailyStats returns the last `days` days of fleet-wide rollups, oldest
// first. Days with no checkins at all simply have no row.
func (d *DB) GetFleetDailyStats(ctx context.Context, days int) ([]FleetDailyStat, error) {
	if days <= 0 {
		days = 7
	}
	rows, err := d.pool.Query(ctx, `
		SELECT s.day,
		       COUNT(*) FILTER (WHERE s.checkin_count > 0),
		       COUNT(*) FILTER (WHERE s.battery_min < 20),
		       COUNT(*) FILTER (WHERE s.temp_max >= 45),
		       AVG(s.battery_avg)::real,
		       COALESCE(SUM(s.checkin_count), 0)
		FROM device_daily_stats s
		JOIN devices dv ON dv.id = s.device_id AND NOT dv.hidden
		WHERE s.day >= CURRENT_DATE - ($1::int - 1)
		GROUP BY s.day
		ORDER BY s.day`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []FleetDailyStat
	for rows.Next() {
		var s FleetDailyStat
		if err := rows.Scan(&s.Day, &s.Active, &s.LowBattery, &s.Hot, &s.BatteryAvg, &s.Checkins); err != nil {
			return nil, err
		}
		stats = append(stats, s)
	}
	return stats, rows.Err()
}

// CountHotDevices returns how many non-hidden devices are currently reporting a
// device temperature at or above 45°C (the dashboard's warn threshold).
func (d *DB) CountHotDevices(ctx context.Context) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM devices
		WHERE NOT hidden AND COALESCE((latest_extra->>'battery_temp_c')::numeric, 0) >= 45`).Scan(&n)
	return n, err
}
