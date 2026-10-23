package db

import (
	"context"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// gmtOffset parses a device-reported timezone like "GMT+5"/"GMT-3"/"GMT+0" to an
// hour offset. Anything unrecognized is treated as UTC (0).
func gmtOffset(tz string) int {
	if !strings.HasPrefix(tz, "GMT") {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(tz, "GMT"))
	if err != nil {
		return 0
	}
	return n
}

// localTime converts UTC now to the venue's/device's local time. It accepts an IANA
// zone name ("America/New_York", DST-aware) or a legacy "GMT±h" offset; unknown or
// empty falls back to UTC. (tzdata is embedded via the time/tzdata import in main.)
func localTime(now time.Time, tz string) time.Time {
	if tz != "" && !strings.HasPrefix(tz, "GMT") {
		if loc, err := time.LoadLocation(tz); err == nil {
			return now.In(loc)
		}
	}
	return now.UTC().Add(time.Duration(gmtOffset(tz)) * time.Hour)
}

// inQuiet reports whether local hour h falls in the [start,end) quiet window,
// supporting windows that wrap past midnight (e.g. 22→6). start==end means "never".
func inQuiet(h, start, end int) bool {
	if start == end {
		return false
	}
	if start < end {
		return h >= start && h < end
	}
	return h >= start || h < end
}

// ── Service windows (Tier 5 §9) ─────────────────────────────────────────────────

// ServiceWindow is a venue's open/close + overnight hours, in local minutes past
// midnight. TZ is the GMT±offset string used to convert UTC "now" to local time;
// empty TZ means fall back to the device's own reported extra.timezone.
type ServiceWindow struct {
	RestaurantID  *uuid.UUID `json:"restaurant_id"`
	OpenMin       int        `json:"open_min"`
	CloseMin      int        `json:"close_min"`
	NightOpenMin  int        `json:"night_open_min"`
	NightCloseMin int        `json:"night_close_min"`
	TZ            string     `json:"timezone"`
	// Week, when set, is the venue's hours per weekday (H1, 23 Oct 2026: the AIO backend
	// keeps hours per day; a venue may set its own per day too). OpenMin/CloseMin then
	// hold the widest window of the week, for anything that reads one window. Overnight
	// stays one window. On resolves a day.
	Week *Week `json:"week,omitempty"`
}

// Week is Monday … Sunday.
type Week [7]DayHours

// DayHours is one weekday's service hours in local minutes past midnight.
type DayHours struct {
	Closed bool `json:"closed,omitempty"`
	Open   int  `json:"open"`
	Close  int  `json:"close"`
}

// WeekIndex is t's weekday as a Week index (Monday = 0).
func WeekIndex(t time.Time) int { return (int(t.Weekday()) + 6) % 7 }

// Span is the widest window across the open days (earliest open, latest close), and
// whether any day is open.
func (wk Week) Span() (open, close int, any bool) {
	for _, d := range wk {
		if d.Closed || d.Open == d.Close {
			continue
		}
		if !any || d.Open < open {
			open = d.Open
		}
		c := d.Close
		if c < d.Open {
			c += 1440 // past midnight
		}
		if !any || c > close {
			close = c
		}
		any = true
	}
	return open, close % 1440, any
}

// AvgOpenMinutes is the week's open minutes per day, on average.
func (wk Week) AvgOpenMinutes() float64 {
	total := 0
	for _, d := range wk {
		if d.Closed || d.Open == d.Close {
			continue
		}
		n := d.Close - d.Open
		if n < 0 {
			n += 1440
		}
		total += n
	}
	return float64(total) / 7
}

// LocalNow is now in the window's timezone, else fallbackTZ.
func (w ServiceWindow) LocalNow(now time.Time, fallbackTZ string) time.Time {
	tz := w.TZ
	if tz == "" {
		tz = fallbackTZ
	}
	return localTime(now, tz)
}

// On returns the window for the local day of lt: with a Week, that day's hours (a closed
// day comes back with OpenMin == CloseMin, which every reader treats as never open) and
// Week cleared; without one, w unchanged.
func (w ServiceWindow) On(lt time.Time) ServiceWindow {
	if w.Week == nil {
		return w
	}
	d := w.Week[WeekIndex(lt)]
	w.Week = nil
	if d.Closed {
		w.OpenMin, w.CloseMin = 0, 0
	} else {
		w.OpenMin, w.CloseMin = d.Open, d.Close
	}
	return w
}

// ClosedOn reports whether lt's day is a closed day of the week.
func (w ServiceWindow) ClosedOn(lt time.Time) bool {
	return w.Week != nil && (w.Week[WeekIndex(lt)].Closed || w.Week[WeekIndex(lt)].Open == w.Week[WeekIndex(lt)].Close)
}

// decodeWeek reads a service_windows.week value (NULL = none).
func decodeWeek(b []byte) *Week {
	if len(b) == 0 || string(b) == "null" {
		return nil
	}
	var wk Week
	if json.Unmarshal(b, &wk) != nil {
		return nil
	}
	return &wk
}

func encodeWeek(wk *Week) any {
	if wk == nil {
		return nil
	}
	b, _ := json.Marshal(wk)
	return string(b)
}

// inMinWindow reports whether local minute m falls in [start,end), supporting windows
// that wrap past midnight (e.g. 1410→360). start==end means "never".
func inMinWindow(m, start, end int) bool {
	if start == end {
		return false
	}
	if start < end {
		return m >= start && m < end
	}
	return m >= start || m < end
}

// inActiveWindow reports whether a device with reported timezone devTZ is currently
// inside the rule's active window. aw is "" / "always" (always true), "service", or
// "overnight". w.TZ overrides devTZ when set (per-venue timezone).
func inActiveWindow(now time.Time, devTZ string, w ServiceWindow, aw string) bool {
	if aw == "" || aw == "always" {
		return true
	}
	tz := w.TZ
	if tz == "" {
		tz = devTZ
	}
	lt := localTime(now, tz)
	localMin := lt.Hour()*60 + lt.Minute()
	w = w.On(lt)
	switch aw {
	case "service":
		return inMinWindow(localMin, w.OpenMin, w.CloseMin)
	case "overnight":
		return inMinWindow(localMin, w.NightOpenMin, w.NightCloseMin)
	}
	return true
}

// InService reports whether now falls inside the window's service hours, in the
// window's own timezone (else fallbackTZ, e.g. the venue's).
func (w ServiceWindow) InService(now time.Time, fallbackTZ string) bool {
	return inActiveWindow(now, fallbackTZ, w, "service")
}

// GetFleetServiceWindow returns the fleet-default service window (the row with both
// restaurant_id and group_id NULL, seeded by the migration).
func (d *DB) GetFleetServiceWindow(ctx context.Context) (ServiceWindow, error) {
	var w ServiceWindow
	err := d.pool.QueryRow(ctx, `
		SELECT open_min, close_min, night_open_min, night_close_min, timezone
		FROM service_windows WHERE restaurant_id IS NULL AND group_id IS NULL`).
		Scan(&w.OpenMin, &w.CloseMin, &w.NightOpenMin, &w.NightCloseMin, &w.TZ)
	if errors.Is(err, pgx.ErrNoRows) {
		// Defensive fallback matching the migration defaults (07:00–23:00 / 23:30–06:00).
		return ServiceWindow{OpenMin: 420, CloseMin: 1380, NightOpenMin: 1410, NightCloseMin: 360}, nil
	}
	return w, err
}

// FleetWindowActive reports whether the fleet-default service window says we are
// currently inside aw ("" / always | service | overnight). Used to gate channel
// delivery (e.g. an on-call channel that should only page during service hours).
func (d *DB) FleetWindowActive(ctx context.Context, aw string) bool {
	if aw == "" || aw == "always" {
		return true
	}
	w, err := d.GetFleetServiceWindow(ctx)
	if err != nil {
		return true // fail open: better a stray notification than a silently dropped alert
	}
	return inActiveWindow(time.Now().UTC(), w.TZ, w, aw)
}

// ListRestaurantServiceWindows returns every restaurant's own service window in one
// query (restaurants without a row are absent; the caller falls back to the fleet
// default). Replaces the per-restaurant N+1 on the Settings page.
func (d *DB) ListRestaurantServiceWindows(ctx context.Context) (map[uuid.UUID]ServiceWindow, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT restaurant_id, open_min, close_min, night_open_min, night_close_min, timezone, week
		FROM service_windows WHERE restaurant_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]ServiceWindow)
	for rows.Next() {
		var id uuid.UUID
		var w ServiceWindow
		var wk []byte
		if err := rows.Scan(&id, &w.OpenMin, &w.CloseMin, &w.NightOpenMin, &w.NightCloseMin, &w.TZ, &wk); err != nil {
			return nil, err
		}
		w.Week = decodeWeek(wk)
		rid := id
		w.RestaurantID = &rid
		out[id] = w
	}
	return out, rows.Err()
}

// GetRestaurantServiceWindow returns a restaurant's own service window and whether it has
// one. When the restaurant has no row, ok is false and the fleet default is returned.
func (d *DB) GetRestaurantServiceWindow(ctx context.Context, restaurantID uuid.UUID) (w ServiceWindow, ok bool, err error) {
	var wk []byte
	err = d.pool.QueryRow(ctx, `
		SELECT open_min, close_min, night_open_min, night_close_min, timezone, week
		FROM service_windows WHERE restaurant_id = $1`, restaurantID).
		Scan(&w.OpenMin, &w.CloseMin, &w.NightOpenMin, &w.NightCloseMin, &w.TZ, &wk)
	w.Week = decodeWeek(wk)
	if errors.Is(err, pgx.ErrNoRows) {
		fleet, ferr := d.GetFleetServiceWindow(ctx)
		return fleet, false, ferr
	}
	if err != nil {
		return ServiceWindow{}, false, err
	}
	id := restaurantID
	w.RestaurantID = &id
	return w, true, nil
}

// DeleteServiceWindow removes a restaurant's window so it falls back to the fleet default.
func (d *DB) DeleteServiceWindow(ctx context.Context, restaurantID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM service_windows WHERE restaurant_id = $1`, restaurantID)
	return err
}

// SetServiceWindow upserts a restaurant's service window (pass a nil RestaurantID to set
// the fleet default).
func (d *DB) SetServiceWindow(ctx context.Context, w ServiceWindow) error {
	if w.RestaurantID == nil {
		// Fleet default is the single both-NULL row seeded by the migration: update it
		// (ON CONFLICT can't target NULL, which never conflicts).
		_, err := d.pool.Exec(ctx, `
			UPDATE service_windows SET
				open_min = $1, close_min = $2, night_open_min = $3, night_close_min = $4,
				timezone = $5, updated_at = NOW()
			WHERE restaurant_id IS NULL AND group_id IS NULL`,
			w.OpenMin, w.CloseMin, w.NightOpenMin, w.NightCloseMin, w.TZ)
		return err
	}
	if w.Week != nil {
		if o, c, ok := w.Week.Span(); ok {
			w.OpenMin, w.CloseMin = o, c
		} else {
			w.OpenMin, w.CloseMin = 0, 0
		}
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO service_windows (restaurant_id, open_min, close_min, night_open_min, night_close_min, timezone, week, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7::jsonb, NOW())
		ON CONFLICT (restaurant_id) WHERE restaurant_id IS NOT NULL DO UPDATE SET
			open_min = EXCLUDED.open_min, close_min = EXCLUDED.close_min,
			night_open_min = EXCLUDED.night_open_min, night_close_min = EXCLUDED.night_close_min,
			timezone = EXCLUDED.timezone, week = EXCLUDED.week, updated_at = NOW()
	`, w.RestaurantID, w.OpenMin, w.CloseMin, w.NightOpenMin, w.NightCloseMin, w.TZ, encodeWeek(w.Week))
	return err
}

// effectiveWindows returns the resolved service window for every non-hidden device:
// its restaurant's window if it has one, else the fleet default. Loaded once per
// evaluation pass so window-gated rules don't re-query per device.
func (d *DB) effectiveWindows(ctx context.Context) (map[uuid.UUID]ServiceWindow, error) {
	fleet, err := d.GetFleetServiceWindow(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := d.pool.Query(ctx, `
		SELECT d.id, d.restaurant_id, COALESCE(d.latest_extra->>'timezone', ''),
		       sw.open_min, sw.close_min, sw.night_open_min, sw.night_close_min, sw.timezone, sw.week
		FROM devices d
		LEFT JOIN service_windows sw ON sw.restaurant_id = d.restaurant_id
		WHERE NOT d.hidden`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]ServiceWindow)
	for rows.Next() {
		var id uuid.UUID
		var rid *uuid.UUID
		var devTZ string
		var open, closeM, nOpen, nClose *int
		var winTZ *string
		var wk []byte
		if err := rows.Scan(&id, &rid, &devTZ, &open, &closeM, &nOpen, &nClose, &winTZ, &wk); err != nil {
			return nil, err
		}
		w := fleet // copy fleet defaults, override with the group's window when present
		w.RestaurantID = rid
		if open != nil {
			w.OpenMin, w.CloseMin, w.NightOpenMin, w.NightCloseMin = *open, *closeM, *nOpen, *nClose
			w.Week = decodeWeek(wk)
			if winTZ != nil {
				w.TZ = *winTZ
			}
		}
		// Bake the effective tz: the venue window's tz wins, else the device's own.
		if w.TZ == "" {
			w.TZ = devTZ
		}
		out[id] = w
	}
	return out, rows.Err()
}

// deployedDeviceSet returns the IDs of devices that are deployed — i.e. assigned to a
// restaurant. Operational service-window rules from the T7 matrix only apply to these: a
// bench/lab unit (no restaurant) idle or unplugged during the default day window is
// expected, not an alert. Hardware rules (storage_low, temp_elevated, overheating) still
// fire fleet-wide so genuine lab faults surface. Restaurant assignment IS the deploy signal.
func (d *DB) deployedDeviceSet(ctx context.Context) (map[uuid.UUID]bool, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.id, (d.restaurant_id IS NOT NULL)
		FROM devices d
		WHERE NOT d.hidden`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]bool)
	for rows.Next() {
		var id uuid.UUID
		var dep bool
		if err := rows.Scan(&id, &dep); err != nil {
			return nil, err
		}
		if dep {
			out[id] = true
		}
	}
	return out, rows.Err()
}

// windowFor returns the resolved window for a device, falling back to fleet defaults
// (already baked into the map) or a hard default if the device is absent.
func windowFor(m map[uuid.UUID]ServiceWindow, id uuid.UUID) ServiceWindow {
	if w, ok := m[id]; ok {
		return w
	}
	return ServiceWindow{OpenMin: 420, CloseMin: 1380, NightOpenMin: 1410, NightCloseMin: 360}
}

// ── Peak windows (discrete busy periods, active_window='peak') ───────────────────

// PeakWindow is one busy period for a venue (or the fleet default when RestaurantID
// is nil), in local minutes past midnight. A range may wrap past midnight (End < Start).
type PeakWindow struct {
	ID           uuid.UUID  `json:"id"`
	RestaurantID *uuid.UUID `json:"restaurant_id"`
	StartMin     int        `json:"start_min"`
	EndMin       int        `json:"end_min"`
}

// inAnyPeak reports whether local time "now" (converted with tz) falls in any of the
// ranges. Empty ranges → never in peak.
func inAnyPeak(now time.Time, tz string, ranges []PeakWindow) bool {
	if len(ranges) == 0 {
		return false
	}
	lt := localTime(now, tz)
	m := lt.Hour()*60 + lt.Minute()
	for _, r := range ranges {
		if inMinWindow(m, r.StartMin, r.EndMin) {
			return true
		}
	}
	return false
}

// ListPeakWindows returns the fleet-default peak set (restaurant_id NULL) plus every
// per-restaurant override, for the settings UI.
func (d *DB) ListPeakWindows(ctx context.Context) ([]PeakWindow, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, restaurant_id, start_min, end_min FROM peak_windows
		ORDER BY restaurant_id NULLS FIRST, start_min`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PeakWindow
	for rows.Next() {
		var w PeakWindow
		if err := rows.Scan(&w.ID, &w.RestaurantID, &w.StartMin, &w.EndMin); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// GetPeakWindows returns the peak ranges stored for one scope (a restaurant, or the
// fleet default when restaurantID is nil) — the raw rows, without inheritance.
func (d *DB) GetPeakWindows(ctx context.Context, restaurantID *uuid.UUID) ([]PeakWindow, error) {
	var rows pgx.Rows
	var err error
	if restaurantID == nil {
		rows, err = d.pool.Query(ctx, `SELECT id, restaurant_id, start_min, end_min FROM peak_windows WHERE restaurant_id IS NULL ORDER BY start_min`)
	} else {
		rows, err = d.pool.Query(ctx, `SELECT id, restaurant_id, start_min, end_min FROM peak_windows WHERE restaurant_id = $1 ORDER BY start_min`, restaurantID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PeakWindow
	for rows.Next() {
		var w PeakWindow
		if err := rows.Scan(&w.ID, &w.RestaurantID, &w.StartMin, &w.EndMin); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SetPeakWindows replaces the full peak set for one scope (a restaurant, or the fleet
// default when restaurantID is nil) in a single transaction — delete-then-insert, so
// the caller passes the complete desired list of ranges.
func (d *DB) SetPeakWindows(ctx context.Context, restaurantID *uuid.UUID, ranges []PeakWindow) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if restaurantID == nil {
		_, err = tx.Exec(ctx, `DELETE FROM peak_windows WHERE restaurant_id IS NULL`)
	} else {
		_, err = tx.Exec(ctx, `DELETE FROM peak_windows WHERE restaurant_id = $1`, restaurantID)
	}
	if err != nil {
		return err
	}
	for _, r := range ranges {
		if _, err = tx.Exec(ctx, `
			INSERT INTO peak_windows (restaurant_id, start_min, end_min) VALUES ($1, $2, $3)`,
			restaurantID, r.StartMin, r.EndMin); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// effectivePeakWindows resolves the peak ranges for every non-hidden device: its
// restaurant's own set if it has any, otherwise the fleet-default set. Loaded once per
// evaluation pass (mirrors effectiveWindows).
func (d *DB) effectivePeakWindows(ctx context.Context) (map[uuid.UUID][]PeakWindow, error) {
	rows, err := d.pool.Query(ctx, `SELECT restaurant_id, start_min, end_min FROM peak_windows`)
	if err != nil {
		return nil, err
	}
	var fleet []PeakWindow
	byRest := make(map[uuid.UUID][]PeakWindow)
	for rows.Next() {
		var rid *uuid.UUID
		var w PeakWindow
		if err := rows.Scan(&rid, &w.StartMin, &w.EndMin); err != nil {
			rows.Close()
			return nil, err
		}
		if rid == nil {
			fleet = append(fleet, w)
		} else {
			byRest[*rid] = append(byRest[*rid], w)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	drows, err := d.pool.Query(ctx, `SELECT id, restaurant_id FROM devices WHERE NOT hidden`)
	if err != nil {
		return nil, err
	}
	defer drows.Close()
	out := make(map[uuid.UUID][]PeakWindow)
	for drows.Next() {
		var id uuid.UUID
		var rid *uuid.UUID
		if err := drows.Scan(&id, &rid); err != nil {
			return nil, err
		}
		if rid != nil {
			if r, ok := byRest[*rid]; ok {
				out[id] = r
				continue
			}
		}
		out[id] = fleet
	}
	return out, drows.Err()
}
