package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// The venue report's plain-language views ask questions the daily rollup does not
// answer: which hours of service a tablet was ready in, how many times a guest actually
// charged a phone, when a tablet went flat. VenueInsights reads them from the shaped
// history for one venue and one week, in the venue's local time. It is only run when
// one of those views is asked for, so the standard report never pays for it.

// VenueHour is one device's local clock hour: it reported at least once, and the lowest
// battery it reported in that hour.
type VenueHour struct {
	DeviceID   uuid.UUID
	Hour       time.Time // local, truncated to the hour
	MinBattery int
}

// PadSession is one guest phone on a tablet's charging pad. The pad's status flickers —
// a week on live had 10,250 switches to "phone on the pad" with a median length of about
// a second — so switches less than two minutes apart are one session, and a session
// shorter than two minutes is a phone put down and picked up, not a charge.
type PadSession struct {
	DeviceID uuid.UUID
	Start    time.Time // local
	Minutes  float64
}

// VenueAlert is an alert that fired on one of the venue's devices in the window.
type VenueAlert struct {
	DeviceID uuid.UUID
	Type     string
	At       time.Time // local
}

// VenueInsights is the raw material; the dashboard turns it into sentences.
type VenueInsights struct {
	Hours    []VenueHour
	Sessions []PadSession
	Restarts map[uuid.UUID]int // restarts the device did on its own (ours subtracted)
	Alerts   []VenueAlert
}

// venueReportAlertTypes are the alerts the plain-language views can explain to a venue:
// a tablet offline during opening hours, a failing charging cable, and running hot.
// The raw battery temperature is too noisy on the T7 to use directly (most tablets touch
// 45°C in a week); the overheating rule debounces it.
var venueReportAlertTypes = []string{"offline_peak", "charger_flapping", "overheating"}

// VenueInsightsFor reads [from, to) for the given devices, bucketing by loc. Hourly
// presence alone runs on to hoursTo: opening hours that pass midnight put the last
// night's service into the next morning, while sessions, restarts and alerts must stop
// at the week's end or they are counted in two weeks.
func (d *DB) VenueInsightsFor(ctx context.Context, ids []uuid.UUID, from, to, hoursTo time.Time, loc *time.Location) (VenueInsights, error) {
	out := VenueInsights{Restarts: map[uuid.UUID]int{}}
	if len(ids) == 0 {
		return out, nil
	}
	tz := loc.String()

	rows, err := d.pool.Query(ctx, `
		SELECT device_id, date_trunc('hour', at AT TIME ZONE $4), MIN(COALESCE(battery_pct, 100))
		FROM device_samples
		WHERE device_id = ANY($1) AND at >= $2 AND at < $3
		GROUP BY 1, 2`, ids, from, hoursTo, tz)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var h VenueHour
		var local time.Time
		if err := rows.Scan(&h.DeviceID, &local, &h.MinBattery); err != nil {
			rows.Close()
			return out, err
		}
		h.Hour = asLocal(local, loc)
		out.Hours = append(out.Hours, h)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	// Runs of wlc_status = 1, merged across gaps of two minutes or less. Each run ends at
	// the next event for that device, capped at three hours so a device that went silent
	// with a phone on it does not claim the night. Read from a day early so a session
	// under way at the window's start is measured whole; only sessions starting inside
	// the window are kept.
	rows, err = d.pool.Query(ctx, `
		WITH ev AS (
			SELECT device_id, at, to_val,
			       LEAD(at) OVER (PARTITION BY device_id ORDER BY at) AS nxt
			FROM device_state_events
			WHERE key = 'wlc_status' AND device_id = ANY($1)
			  AND at >= $2::timestamptz - INTERVAL '1 day' AND at < $3
		), runs AS (
			SELECT device_id, at AS s,
			       LEAST(COALESCE(nxt, at), at + INTERVAL '3 hours', $3::timestamptz) AS e
			FROM ev WHERE to_val = '1'
		), marked AS (
			SELECT *, CASE WHEN s - LAG(e) OVER (PARTITION BY device_id ORDER BY s) <= INTERVAL '2 minutes'
			               THEN 0 ELSE 1 END AS brk
			FROM runs
		), grouped AS (
			SELECT *, SUM(brk) OVER (PARTITION BY device_id ORDER BY s) AS sid FROM marked
		)
		SELECT device_id, MIN(s) AT TIME ZONE $4, SUM(EXTRACT(EPOCH FROM e - s)) / 60
		FROM grouped
		GROUP BY device_id, sid
		HAVING SUM(EXTRACT(EPOCH FROM e - s)) >= 120 AND MIN(s) >= $2`, ids, from, to, tz)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var s PadSession
		var local time.Time
		if err := rows.Scan(&s.DeviceID, &local, &s.Minutes); err != nil {
			rows.Close()
			return out, err
		}
		s.Start = asLocal(local, loc)
		out.Sessions = append(out.Sessions, s)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	// Restarts the tablet did by itself: reboot events, less the reboot commands we
	// sent it in the same window. A venue should not be told its tablet restarted
	// unexpectedly because an operator restarted it.
	rows, err = d.pool.Query(ctx, `
		SELECT e.device_id, COUNT(*) - COALESCE((
			SELECT COUNT(*) FROM command_status cs JOIN commands c ON c.id = cs.command_id
			WHERE cs.device_id = e.device_id AND c.type = 'reboot'
			  AND c.created_at >= $2 AND c.created_at < $3), 0)
		FROM device_events e
		WHERE e.device_id = ANY($1) AND e.kind = 'reboot'
		  AND e.occurred_at >= $2 AND e.occurred_at < $3
		GROUP BY e.device_id`, ids, from, to)
	if err != nil {
		return out, err
	}
	for rows.Next() {
		var id uuid.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			rows.Close()
			return out, err
		}
		if n > 0 {
			out.Restarts[id] = n
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return out, err
	}

	rows, err = d.pool.Query(ctx, `
		SELECT device_id, type, fired_at AT TIME ZONE $5
		FROM alerts
		WHERE device_id = ANY($1) AND type = ANY($2) AND fired_at >= $3 AND fired_at < $4
		ORDER BY fired_at`, ids, venueReportAlertTypes, from, to, tz)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var a VenueAlert
		var local time.Time
		if err := rows.Scan(&a.DeviceID, &a.Type, &local); err != nil {
			return out, err
		}
		a.At = asLocal(local, loc)
		out.Alerts = append(out.Alerts, a)
	}
	return out, rows.Err()
}

// asLocal re-reads a timestamp-without-time-zone (which pgx hands back as UTC wall
// clock) as the same wall clock in loc.
func asLocal(t time.Time, loc *time.Location) time.Time {
	return time.Date(t.Year(), t.Month(), t.Day(), t.Hour(), t.Minute(), t.Second(), t.Nanosecond(), loc)
}

// VenueDeviceTimezone is the timezone most of these devices report. A venue with no
// timezone of its own is read in its tablets' time rather than UTC, which put an
// American dinner service at 2am.
func (d *DB) VenueDeviceTimezone(ctx context.Context, ids []uuid.UUID) (string, error) {
	var tz string
	err := d.pool.QueryRow(ctx, `
		SELECT COALESCE((
			SELECT latest_extra->>'timezone' FROM devices
			WHERE id = ANY($1) AND COALESCE(latest_extra->>'timezone', '') <> ''
			GROUP BY 1 ORDER BY COUNT(*) DESC, 1 LIMIT 1), '')`, ids).Scan(&tz)
	return tz, err
}
