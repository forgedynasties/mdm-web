package db

import (
	"context"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
)

// ── Restaurants ─────────────────────────────────────────────────────────────────
//
// A restaurant is the venue a device physically lives in (1 device → at most 1
// restaurant). Restaurants own the venue semantics (deployed flag, service window,
// health, AI bucketing); groups remain free-form tags. See
// docs/release-versioning-and-restaurants.md.

func (d *DB) CreateRestaurant(ctx context.Context, r Restaurant) (*Restaurant, error) {
	var out Restaurant
	err := d.pool.QueryRow(ctx, `
		INSERT INTO restaurants (name, address, latitude, longitude, timezone, notes, contact_email)
		VALUES ($1, $2, $3, $4, $5, $6, $7)
		RETURNING id, name, address, latitude, longitude, timezone, notes, created_at, contact_email
	`, r.Name, r.Address, r.Latitude, r.Longitude, r.Timezone, r.Notes, r.ContactEmail).
		Scan(&out.ID, &out.Name, &out.Address, &out.Latitude, &out.Longitude, &out.Timezone, &out.Notes, &out.CreatedAt, &out.ContactEmail)
	return &out, err
}

func (d *DB) UpdateRestaurant(ctx context.Context, r Restaurant) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE restaurants SET name = $2, address = $3, latitude = $4, longitude = $5,
			timezone = $6, notes = $7
		WHERE id = $1
	`, r.ID, r.Name, r.Address, r.Latitude, r.Longitude, r.Timezone, r.Notes)
	venueChanged(err)
	return err
}

// SetRestaurantContactEmail sets only the venue's contact email. Separate from
// UpdateRestaurant, which writes every field, so a form that does not carry the field
// cannot blank it.
func (d *DB) SetRestaurantContactEmail(ctx context.Context, id uuid.UUID, email string) error {
	_, err := d.pool.Exec(ctx, `UPDATE restaurants SET contact_email = $2 WHERE id = $1`, id, email)
	return err
}

// RenameRestaurant changes only a restaurant's display name — used by the fleet
// toolbar's inline rename, which must not clobber address/timezone/notes the way
// posting the full edit form with a blank rest would.
func (d *DB) RenameRestaurant(ctx context.Context, id uuid.UUID, name string) error {
	_, err := d.pool.Exec(ctx, `UPDATE restaurants SET name = $2 WHERE id = $1`, id, name)
	venueChanged(err)
	return err
}

func (d *DB) DeleteRestaurant(ctx context.Context, id uuid.UUID) error {
	// devices.restaurant_id ON DELETE SET NULL unassigns members automatically.
	_, err := d.pool.Exec(ctx, `DELETE FROM restaurants WHERE id = $1`, id)
	return err
}

func (d *DB) ListRestaurants(ctx context.Context) ([]Restaurant, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT r.id, r.name, r.address, r.latitude, r.longitude, r.timezone,
		       r.notes, r.created_at, COUNT(d.id) AS device_count, r.contact_email
		FROM restaurants r
		LEFT JOIN devices d ON d.restaurant_id = r.id AND NOT d.hidden
		GROUP BY r.id
		ORDER BY r.name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Restaurant
	for rows.Next() {
		var r Restaurant
		if err := rows.Scan(&r.ID, &r.Name, &r.Address, &r.Latitude, &r.Longitude, &r.Timezone,
			&r.Notes, &r.CreatedAt, &r.DeviceCount, &r.ContactEmail); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) GetRestaurant(ctx context.Context, id uuid.UUID) (*Restaurant, error) {
	var r Restaurant
	err := d.pool.QueryRow(ctx, `
		SELECT r.id, r.name, r.address, r.latitude, r.longitude, r.timezone,
		       r.notes, r.created_at, COUNT(d.id) AS device_count, r.contact_email
		FROM restaurants r
		LEFT JOIN devices d ON d.restaurant_id = r.id AND NOT d.hidden
		WHERE r.id = $1
		GROUP BY r.id
	`, id).Scan(&r.ID, &r.Name, &r.Address, &r.Latitude, &r.Longitude, &r.Timezone,
		&r.Notes, &r.CreatedAt, &r.DeviceCount, &r.ContactEmail)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// AssignDeviceToRestaurant sets (or with restaurantID=nil clears) a device's venue.
func (d *DB) AssignDeviceToRestaurant(ctx context.Context, serial string, restaurantID *uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `UPDATE devices SET restaurant_id = $2 WHERE serial_number = $1`, serial, restaurantID)
	venueChanged(err)
	return err
}

// AssignDevicesToRestaurant assigns many devices (by serial) to a restaurant at once.
func (d *DB) AssignDevicesToRestaurant(ctx context.Context, serials []string, restaurantID uuid.UUID) error {
	if len(serials) == 0 {
		return nil
	}
	_, err := d.pool.Exec(ctx, `UPDATE devices SET restaurant_id = $2 WHERE serial_number = ANY($1)`, serials, restaurantID)
	venueChanged(err)
	return err
}

func (d *DB) ListRestaurantDevices(ctx context.Context, restaurantID uuid.UUID) ([]Device, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT
			d.id, d.serial_number, d.build_id, d.last_seen_at, d.created_at,
			d.latest_battery_pct AS battery_pct,
			d.poll_interval_ms,
			COALESCE(dc.kiosk_enabled, false),
			COALESCE(dc.kiosk_package, '')
		FROM devices d
		LEFT JOIN device_config dc ON dc.device_id = d.id
		WHERE d.restaurant_id = $1 AND NOT d.hidden
		ORDER BY d.serial_number
	`, restaurantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var devices []Device
	for rows.Next() {
		var dev Device
		if err := rows.Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct, &dev.PollIntervalMs, &dev.KioskEnabled, &dev.KioskPackage); err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	return devices, rows.Err()
}

// ListAssignableDevices returns candidate devices to assign to a restaurant: every
// non-hidden device NOT already in this restaurant, optionally filtered by serial, with
// its current restaurant name. Unassigned (lab) devices sort first, then most-recently
// seen. Powers the restaurant device picker.
func (d *DB) ListAssignableDevices(ctx context.Context, restaurantID uuid.UUID, query, status, battery string, limit int, connected []uuid.UUID) ([]Device, error) {
	if limit <= 0 {
		limit = 20
	}
	args := []any{restaurantID, query}
	q := `
		SELECT d.serial_number, d.build_id, d.latest_battery_pct, d.last_seen_at, COALESCE(r.name, '')
		FROM devices d
		LEFT JOIN restaurants r ON r.id = d.restaurant_id
		WHERE NOT d.hidden
		  AND d.restaurant_id IS DISTINCT FROM $1
		  AND ($2 = '' OR d.serial_number ILIKE '%' || $2 || '%')`
	if status == "online" || status == "offline" {
		// Online/offline is live WebSocket presence, not check-in recency.
		args = append(args, connected)
		op := "= ANY"
		if status == "offline" {
			op = "<> ALL"
		}
		q += fmt.Sprintf(" AND d.id %s($%d::uuid[])", op, len(args))
	}
	switch battery {
	case "low":
		q += " AND "+hasBatteryD+" AND d.latest_battery_pct < 20"
	case "mid":
		q += " AND "+hasBatteryD+" AND d.latest_battery_pct BETWEEN 20 AND 49"
	case "ok":
		q += " AND "+hasBatteryD+" AND d.latest_battery_pct >= 50"
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY (d.restaurant_id IS NULL) DESC, d.last_seen_at DESC LIMIT $%d", len(args))
	rows, err := d.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var dev Device
		if err := rows.Scan(&dev.SerialNumber, &dev.BuildID, &dev.BatteryPct, &dev.LastSeenAt, &dev.RestaurantName); err != nil {
			return nil, err
		}
		out = append(out, dev)
	}
	return out, rows.Err()
}

// GetRestaurantDailyStats returns the last `days` days of stats rolled up across every
// device in the restaurant, oldest first. Mirrors GetGroupDailyStats but keyed by venue.
func (d *DB) GetRestaurantDailyStats(ctx context.Context, restaurantID uuid.UUID, days int) ([]GroupDailyStat, error) {
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
		JOIN devices d ON d.id = s.device_id
		WHERE d.restaurant_id = $1 AND s.day >= CURRENT_DATE - ($2::int - 1)
		GROUP BY s.day
		ORDER BY s.day`, restaurantID, days)
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

// SiteMetrics is a venue's power and usage picture over a window, aggregated from
// device_daily_stats. Restaurants are managed per site but almost everything else in the
// MDM reports per device, so this answers "how is this venue's hardware doing this week".
//
// Every figure carries what it was measured over, because a thin window must read as
// "not enough data" rather than a confident zero — a venue whose devices checked in for
// an hour is not a venue with 0% uptime.
type SiteMetrics struct {
	Days        int
	DeviceCount int // devices with at least one rolled-up day in the window

	PoweredMinutes     float64 // total, across devices
	PoweredOpenMinutes float64 // ...restricted to opening hours
	HasOpenHours       bool    // false when the site has no service window configured
	OpenWindowMinutes  float64 // opening hours over the device-days that have the data
	FullWindowMinutes  float64 // 24h × device-days present
	// Device-days actually rolled up. Both denominators are built from these rather than
	// from days × devices: a device enrolled two days ago, or a day whose rollup predates
	// a column, must not drag the percentage down as though it were silent all week.
	DeviceDays     int
	OpenDeviceDays int // ...of which have opening-hours data

	PadMinutes      float64 // a guest phone was on the tablet's pad this long
	PadDrainPct     float64 // battery points that cost us, off mains only
	PadDrainMinutes float64 // measured over

	// Standby: powered, nobody using it. Both figures cover only the device-days that
	// actually reported screen state, so a fleet mid-firmware-rollout is not reported as
	// overwhelmingly idle just because most devices cannot answer yet.
	ScreenOnMinutes      float64
	ScreenPoweredMinutes float64 // powered minutes on those same device-days
	ScreenDeviceDays     int

	ConnectAvgPct    float64 // battery % when someone plugs the tablet in
	ConnectCount     int
	DisconnectAvgPct float64 // ...and when they unplug it
	DisconnectCount  int

	// Time on mains, summed the same way the weekly report's per-device rows do it
	// (charging fraction × powered minutes), so the venue card and the report's
	// "Plugged in" tile are the same measurement under the same name.
	PluggedMinutes float64
}

// PluggedPct is time on mains as a share of the device-days present.
func (m SiteMetrics) PluggedPct() int {
	if m.FullWindowMinutes <= 0 {
		return 0
	}
	p := int(m.PluggedMinutes * 100 / m.FullWindowMinutes)
	if p > 100 {
		return 100
	}
	return p
}


// StandbyMinutes is powered time with the screen off, over the days that measured it.
func (m SiteMetrics) StandbyMinutes() float64 {
	v := m.ScreenPoweredMinutes - m.ScreenOnMinutes
	if v < 0 {
		return 0 // clock skew or a partial day; never report negative idle time
	}
	return v
}

// HasStandby reports whether any device-day in the window carried screen state. Until the
// firmware that reports it reaches the fleet this is false, and the tile says so.
func (m SiteMetrics) HasStandby() bool { return m.ScreenDeviceDays > 0 && m.ScreenPoweredMinutes > 0 }

// StandbyPct is the share of powered time nobody was using the device.
func (m SiteMetrics) StandbyPct() int {
	if !m.HasStandby() {
		return 0
	}
	p := int(m.StandbyMinutes() * 100 / m.ScreenPoweredMinutes)
	if p > 100 {
		return 100
	}
	return p
}

// PadDrainPctPerMin is the headline "1% per minute" figure, or 0 when the window holds
// too little pad time to say anything honest.
func (m SiteMetrics) PadDrainPctPerMin() float64 {
	if !m.HasPadDrain() {
		return 0
	}
	return m.PadDrainPct / m.PadDrainMinutes
}

// PadDrainMinMinutes is the least wireless-charging time, off the tablet's own charger,
// a drain rate is shown for. Battery is reported in whole percent, so below this one
// step either way swings the rate wildly. It was 30, which left most tablets at a
// venue with a dash; ten minutes still means at least a percent or so of real drain.
const PadDrainMinMinutes = 10

// PadUnusedMinutes is how little pad time in a week reads as "not used" rather than
// "too little to measure".
const PadUnusedMinutes = 5

// HasPadDrain reports whether the drain rate rests on enough measured time to show.
func (m SiteMetrics) HasPadDrain() bool { return m.PadDrainMinutes >= PadDrainMinMinutes }

// UptimeOpenPct is powered time as a share of opening hours, capped at 100: a device
// left on overnight would otherwise read as 140% of a site that opens for 10 hours.
func (m SiteMetrics) UptimeOpenPct() int {
	if !m.HasOpenHours || m.OpenWindowMinutes <= 0 {
		return 0
	}
	p := int(m.PoweredOpenMinutes * 100 / m.OpenWindowMinutes)
	if p > 100 {
		return 100
	}
	return p
}

// UptimeFullPct is powered time as a share of the whole window (every hour, open or not).
func (m SiteMetrics) UptimeFullPct() int {
	if m.FullWindowMinutes <= 0 {
		return 0
	}
	p := int(m.PoweredMinutes * 100 / m.FullWindowMinutes)
	if p > 100 {
		return 100
	}
	return p
}

// DeviceWeek is one device's row in a venue's weekly report: the same figures as
// SiteMetrics, measured for a single serial. Denominators are built from the
// device-days actually rolled up, so a device enrolled midweek reads as a short
// window rather than a bad one.
type DeviceWeek struct {
	DeviceID uuid.UUID
	Serial   string
	Nickname string

	Days              int
	DeviceDays        int
	PoweredMinutes    float64
	FullWindowMinutes float64 // 24h × device-days present
	PluggedMinutes    float64

	PadMinutes      float64
	PadDrainPct     float64
	PadDrainMinutes float64

	ScreenOnMinutes      float64
	ScreenPoweredMinutes float64
	ScreenDeviceDays     int
}

// UptimeFullPct is powered time as a share of the device-days present, capped at 100.
func (w DeviceWeek) UptimeFullPct() int {
	if w.FullWindowMinutes <= 0 {
		return 0
	}
	p := int(w.PoweredMinutes * 100 / w.FullWindowMinutes)
	if p > 100 {
		return 100
	}
	return p
}

// PluggedPct is time on mains as a share of the device-days present.
func (w DeviceWeek) PluggedPct() int {
	if w.FullWindowMinutes <= 0 {
		return 0
	}
	p := int(w.PluggedMinutes * 100 / w.FullWindowMinutes)
	if p > 100 {
		return 100
	}
	return p
}

// HasPadDrain reports whether the drain rate rests on enough measured time to show.
// Same floor as the site figure, so a row and the header agree.
func (w DeviceWeek) HasPadDrain() bool { return w.PadDrainMinutes >= PadDrainMinMinutes }

// PadUnused reports a device whose pad was barely touched all week — the honest
// reason its drain is blank.
func (w DeviceWeek) PadUnused() bool { return w.PadMinutes < PadUnusedMinutes }

// PadDrainPctPerMin is the per-device drain while a guest phone is on the pad and
// the tablet is off mains, or 0 when too little pad time was measured to say.
func (w DeviceWeek) PadDrainPctPerMin() float64 {
	if !w.HasPadDrain() {
		return 0
	}
	return w.PadDrainPct / w.PadDrainMinutes
}

// HasStandby reports whether this device reported screen state at all in the window.
func (w DeviceWeek) HasStandby() bool { return w.ScreenDeviceDays > 0 && w.ScreenPoweredMinutes > 0 }

// StandbyMinutes is powered time with the screen off, over the days that measured it.
func (w DeviceWeek) StandbyMinutes() float64 {
	v := w.ScreenPoweredMinutes - w.ScreenOnMinutes
	if v < 0 {
		return 0 // clock skew or a partial day; never report negative idle time
	}
	return v
}

// RestaurantDeviceWeeks returns one row per device at a venue for the same window
// SiteMetricsFor aggregates, so the report's table sums to its header. Reads only
// device_daily_stats — never raw check-ins.
// endDayArg renders an explicit window end for the metrics queries. The zero time
// means "up to today", which is what every rolling caller wants; the weekly report
// passes the last completed Sunday instead so it covers a finished week rather than a
// part-week that changes under you as the day goes on.
func endDayArg(endDay time.Time) any {
	if endDay.IsZero() {
		return nil
	}
	return endDay
}

// DailyStatsStamp is the newest computed_at and the row count of a venue's rollups in
// a window: whatever changes the figures a report quotes changes one of the two, so
// together they key a cached render of it.
func (d *DB) DailyStatsStamp(ctx context.Context, restaurantID uuid.UUID, days int, endDay time.Time) (time.Time, int, error) {
	var at *time.Time
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT MAX(s.computed_at), COUNT(*)::int
		FROM device_daily_stats s
		JOIN devices dev ON dev.id = s.device_id AND NOT dev.hidden
		WHERE dev.restaurant_id = $2
		  AND s.day >  COALESCE($3::date, CURRENT_DATE) - $1::int
		  AND s.day <= COALESCE($3::date, CURRENT_DATE)`, days, restaurantID, endDayArg(endDay)).Scan(&at, &n)
	if err != nil || at == nil {
		return time.Time{}, n, err
	}
	return *at, n, nil
}

func (d *DB) RestaurantDeviceWeeks(ctx context.Context, restaurantID uuid.UUID, days int, endDay time.Time) ([]DeviceWeek, error) {
	if days <= 0 {
		days = 7
	}
	rows, err := d.pool.Query(ctx, `
		SELECT dev.id, dev.serial_number, COALESCE(n.name, ''),
			COUNT(*)::int,
			COALESCE(SUM(s.online_minutes), 0)::float8,
			COALESCE(SUM(s.charging_frac * s.online_minutes), 0)::float8,
			COALESCE(SUM(s.wlc_minutes), 0)::float8,
			COALESCE(SUM(s.wlc_drain_pct), 0)::float8,
			COALESCE(SUM(s.wlc_drain_minutes), 0)::float8,
			COALESCE(SUM(s.screen_on_minutes), 0)::float8,
			COALESCE(SUM(s.online_minutes) FILTER (WHERE s.screen_on_minutes IS NOT NULL), 0)::float8,
			COUNT(s.screen_on_minutes)::int
		FROM device_daily_stats s
		JOIN devices dev ON dev.id = s.device_id AND NOT dev.hidden
		LEFT JOIN device_nicknames n ON n.device_id = dev.id
		WHERE dev.restaurant_id = $2
		  AND s.day >  COALESCE($3::date, CURRENT_DATE) - $1::int
		  AND s.day <= COALESCE($3::date, CURRENT_DATE)
		GROUP BY dev.id, dev.serial_number, n.name
		ORDER BY SUM(s.online_minutes) DESC NULLS LAST, dev.serial_number`, days, restaurantID, endDayArg(endDay))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceWeek
	for rows.Next() {
		w := DeviceWeek{Days: days}
		if err := rows.Scan(&w.DeviceID, &w.Serial, &w.Nickname, &w.DeviceDays,
			&w.PoweredMinutes, &w.PluggedMinutes, &w.PadMinutes, &w.PadDrainPct,
			&w.PadDrainMinutes, &w.ScreenOnMinutes, &w.ScreenPoweredMinutes,
			&w.ScreenDeviceDays); err != nil {
			return nil, err
		}
		w.FullWindowMinutes = float64(w.DeviceDays) * 24 * 60
		out = append(out, w)
	}
	return out, rows.Err()
}

// SiteMetricsDay is one day of a venue's power picture, for the evidence strip under the
// headline figures: a percentage nobody can check is a percentage nobody trusts.
type SiteMetricsDay struct {
	Day            time.Time
	Devices        int
	PoweredMinutes float64
	OpenMinutes    float64 // powered inside opening hours; -1 when that day predates the data
	PadMinutes     float64
}

// HasOpen reports whether this day carries opening-hours data.
func (d SiteMetricsDay) HasOpen() bool { return d.OpenMinutes >= 0 }

// SiteMetricsDaily returns the window day by day, oldest first, so the card can show what
// the headline is made of and which days are missing.
func (d *DB) SiteMetricsDaily(ctx context.Context, restaurantID uuid.UUID, days int, endDay time.Time) ([]SiteMetricsDay, error) {
	if days <= 0 {
		days = 7
	}
	// Fixed argument positions ($1 days, $2 end, $3 venue) so the window predicate is
	// one string regardless of whether a venue was named.
	scope := "d.restaurant_id IS NOT NULL AND ($3::uuid IS NULL OR d.restaurant_id = $3)"
	var venue any
	if restaurantID != uuid.Nil {
		venue = restaurantID
	}
	args := []any{days, endDayArg(endDay), venue}
	rows, err := d.pool.Query(ctx, `
		SELECT s.day,
		       COUNT(DISTINCT s.device_id),
		       COALESCE(SUM(s.online_minutes), 0)::float8,
		       CASE WHEN COUNT(s.online_minutes_open) = 0 THEN -1
		            ELSE COALESCE(SUM(s.online_minutes_open), 0)::float8 END,
		       COALESCE(SUM(s.wlc_minutes), 0)::float8
		FROM device_daily_stats s
		JOIN devices d ON d.id = s.device_id AND NOT d.hidden
		WHERE `+scope+`
		  AND s.day >  COALESCE($2::date, CURRENT_DATE) - $1::int
		  AND s.day <= COALESCE($2::date, CURRENT_DATE)
		GROUP BY s.day
		ORDER BY s.day`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SiteMetricsDay
	for rows.Next() {
		var x SiteMetricsDay
		if err := rows.Scan(&x.Day, &x.Devices, &x.PoweredMinutes, &x.OpenMinutes, &x.PadMinutes); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// SiteMetricsFor aggregates the window for one restaurant, or for the whole fleet when
// restaurantID is uuid.Nil (which backs the Overview widget).
func (d *DB) SiteMetricsFor(ctx context.Context, restaurantID uuid.UUID, days int, endDay time.Time) (SiteMetrics, error) {
	if days <= 0 {
		days = 7
	}
	m := SiteMetrics{Days: days}
	scope := "d.restaurant_id IS NOT NULL AND ($3::uuid IS NULL OR d.restaurant_id = $3)"
	var venue any
	if restaurantID != uuid.Nil {
		venue = restaurantID
	}
	args := []any{days, endDayArg(endDay), venue}
	var openMin, closeMin *int
	err := d.pool.QueryRow(ctx, `
		SELECT
			COUNT(DISTINCT s.device_id),
			COUNT(*),
			COUNT(s.online_minutes_open),
			COALESCE(SUM(s.online_minutes), 0)::float8,
			COALESCE(SUM(s.online_minutes_open), 0)::float8,
			bool_or(s.online_minutes_open IS NOT NULL),
			COALESCE(SUM(s.wlc_minutes), 0)::float8,
			COALESCE(SUM(s.wlc_drain_pct), 0)::float8,
			COALESCE(SUM(s.wlc_drain_minutes), 0)::float8,
			COALESCE(SUM(s.charge_connect_sum), 0)::float8,
			COALESCE(SUM(s.charge_connect_n), 0)::int,
			COALESCE(SUM(s.charge_disconnect_sum), 0)::float8,
			COALESCE(SUM(s.charge_disconnect_n), 0)::int,
			COALESCE(SUM(s.screen_on_minutes), 0)::float8,
			COALESCE(SUM(s.online_minutes) FILTER (WHERE s.screen_on_minutes IS NOT NULL), 0)::float8,
			COUNT(s.screen_on_minutes),
			COALESCE(SUM(s.charging_frac * s.online_minutes), 0)::float8
		FROM device_daily_stats s
		JOIN devices d ON d.id = s.device_id AND NOT d.hidden
		WHERE `+scope+`
		  AND s.day >  COALESCE($2::date, CURRENT_DATE) - $1::int
		  AND s.day <= COALESCE($2::date, CURRENT_DATE)`, args...).
		Scan(&m.DeviceCount, &m.DeviceDays, &m.OpenDeviceDays, &m.PoweredMinutes, &m.PoweredOpenMinutes, &m.HasOpenHours,
			&m.PadMinutes, &m.PadDrainPct, &m.PadDrainMinutes,
			&m.ConnectAvgPct, &m.ConnectCount, &m.DisconnectAvgPct, &m.DisconnectCount,
			&m.ScreenOnMinutes, &m.ScreenPoweredMinutes, &m.ScreenDeviceDays, &m.PluggedMinutes)
	if err != nil {
		return m, err
	}
	// The scans above collected SUMS of the battery levels; turn them into averages now
	// that the whole window is added up (an average of daily averages would weight a day
	// with one connect the same as a day with twenty).
	if m.ConnectCount > 0 {
		m.ConnectAvgPct /= float64(m.ConnectCount)
	}
	if m.DisconnectCount > 0 {
		m.DisconnectAvgPct /= float64(m.DisconnectCount)
	}
	m.FullWindowMinutes = 24 * 60 * float64(m.DeviceDays)

	// Opening hours for the denominator: the venue's window, else the fleet default.
	if restaurantID != uuid.Nil {
		if w, ok, e := d.GetRestaurantServiceWindow(ctx, restaurantID); e == nil && ok {
			openMin, closeMin = &w.OpenMin, &w.CloseMin
		}
	}
	if openMin == nil {
		if w, e := d.GetFleetServiceWindow(ctx); e == nil {
			openMin, closeMin = &w.OpenMin, &w.CloseMin
		}
	}
	if openMin != nil {
		per := float64(*closeMin - *openMin)
		if per < 0 {
			per += 24 * 60 // a window that wraps past midnight
		}
		m.OpenWindowMinutes = per * float64(m.OpenDeviceDays)
	}
	return m, nil
}

// GetRestaurantHealth returns a health scorecard per restaurant, worst score first.
// Reuses the GroupHealth struct (GroupID carries the restaurant id, Name the restaurant
// name). activeSecs is the offline threshold. windowDays sizes the recent aggregation
// window and the equal-length prior window used for the delta: pass 7 for the dashboard's
// week-over-week view, or 1 for the Daily Report (today vs yesterday).
// GetRestaurantHealth rolls up per-restaurant health. offline_count is devices WITHOUT
// a live WebSocket (from the connected set), matching the WS-based online status.
func (d *DB) GetRestaurantHealth(ctx context.Context, connected []uuid.UUID, windowDays int) ([]GroupHealth, error) {
	if windowDays <= 0 {
		windowDays = 7
	}
	rows, err := d.pool.Query(ctx, `
		WITH recent AS (
			SELECT d.restaurant_id,
				AVG(s.battery_max)   AS battery_avg,
				AVG(s.charging_frac) AS charging_avg,
				MAX(s.temp_max)      AS temp_max,
				COUNT(DISTINCT NULLIF(s.build_id, '')) AS builds
			FROM device_daily_stats s
			JOIN devices d ON d.id = s.device_id
			WHERE s.day > CURRENT_DATE - $2::int AND d.restaurant_id IS NOT NULL AND NOT d.hidden
			GROUP BY d.restaurant_id
		),
		prior AS (
			SELECT d.restaurant_id, AVG(s.battery_max) AS battery_avg
			FROM device_daily_stats s
			JOIN devices d ON d.id = s.device_id
			WHERE s.day <= CURRENT_DATE - $2::int AND s.day > CURRENT_DATE - ($2::int * 2) AND d.restaurant_id IS NOT NULL AND NOT d.hidden
			GROUP BY d.restaurant_id
		),
		hottest AS (
			-- the single device that drove each restaurant's MAX(temp_max) in the window,
			-- so the report can name the unit instead of just citing the peak number.
			SELECT DISTINCT ON (d.restaurant_id) d.restaurant_id, d.serial_number AS hot_serial
			FROM device_daily_stats s
			JOIN devices d ON d.id = s.device_id
			WHERE s.day > CURRENT_DATE - $2::int AND d.restaurant_id IS NOT NULL AND s.temp_max IS NOT NULL AND NOT d.hidden
			ORDER BY d.restaurant_id, s.temp_max DESC
		),
		devs AS (
			SELECT d.restaurant_id,
				COUNT(*) AS device_count,
				-- Offline means "not connected and not accounted for". A device that is
				-- reporting to another MDM is neither: it is somewhere else, doing fine,
				-- and counting it as an outage penalises a venue for a move someone
				-- made on purpose. See internal/db/custody.go.
				COUNT(*) FILTER (WHERE d.id <> ALL($1::uuid[]) AND d.custody_server = '') AS offline_count,
				COUNT(*) FILTER (WHERE d.id <> ALL($1::uuid[]) AND d.custody_server = ''
				                   AND d.last_seen_at < NOW() - INTERVAL '14 days') AS dormant_count,
				COUNT(*) AS deployed_count  -- every device in a restaurant is deployed
			FROM devices d
			WHERE NOT d.hidden AND d.restaurant_id IS NOT NULL
			GROUP BY d.restaurant_id
		),
		al AS (
			SELECT d.restaurant_id,
				COUNT(*) FILTER (WHERE a.severity = 'critical')  AS crit,
				COUNT(*) FILTER (WHERE a.severity <> 'critical') AS warn
			FROM alerts a
			JOIN devices d ON d.id = a.device_id
			WHERE a.status <> 'resolved' AND d.restaurant_id IS NOT NULL AND NOT d.hidden
			GROUP BY d.restaurant_id
		)
		SELECT r.id, r.name,
			COALESCE(devs.device_count, 0), COALESCE(devs.offline_count, 0), COALESCE(devs.dormant_count, 0),
			COALESCE(al.crit, 0), COALESCE(al.warn, 0),
			recent.battery_avg, (recent.battery_avg - prior.battery_avg),
			recent.charging_avg, recent.temp_max, hottest.hot_serial, COALESCE(recent.builds, 0),
			true, COALESCE(devs.deployed_count, 0)  -- a restaurant is a live venue by definition
		FROM restaurants r
		LEFT JOIN devs    ON devs.restaurant_id    = r.id
		LEFT JOIN recent  ON recent.restaurant_id  = r.id
		LEFT JOIN prior   ON prior.restaurant_id   = r.id
		LEFT JOIN hottest ON hottest.restaurant_id = r.id
		LEFT JOIN al      ON al.restaurant_id      = r.id
		ORDER BY r.name`, connected, windowDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupHealth
	for rows.Next() {
		var g GroupHealth
		if err := rows.Scan(&g.GroupID, &g.Name, &g.DeviceCount, &g.OfflineCount, &g.DormantCount,
			&g.OpenCritical, &g.OpenWarning, &g.BatteryAvg, &g.BatteryDelta,
			&g.ChargingAvg, &g.TempMax, &g.TempMaxSerial, &g.DistinctBuilds, &g.Deployed, &g.DeployedCount); err != nil {
			return nil, err
		}
		g.computeScore()
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score < out[j].Score })
	return out, nil
}

// UpdateRestaurantProfile sets the venue's name and notes, and nothing else (the
// venue page's Profile card).
func (d *DB) UpdateRestaurantProfile(ctx context.Context, id uuid.UUID, name, notes string) error {
	_, err := d.pool.Exec(ctx, `UPDATE restaurants SET name = $2, notes = $3 WHERE id = $1`, id, name, notes)
	venueChanged(err)
	return err
}

// UpdateRestaurantLocation sets the venue's address, point and timezone, and nothing
// else (the venue page's Location card).
func (d *DB) UpdateRestaurantLocation(ctx context.Context, id uuid.UUID, address string, lat, lon *float64, tz string) error {
	_, err := d.pool.Exec(ctx, `UPDATE restaurants SET address = $2, latitude = $3, longitude = $4, timezone = $5 WHERE id = $1`,
		id, address, lat, lon, tz)
	return err
}
