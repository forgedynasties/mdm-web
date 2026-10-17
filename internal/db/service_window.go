package db

import (
	"context"
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
	switch aw {
	case "service":
		return inMinWindow(localMin, w.OpenMin, w.CloseMin)
	case "overnight":
		return inMinWindow(localMin, w.NightOpenMin, w.NightCloseMin)
	}
	return true
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
		SELECT restaurant_id, open_min, close_min, night_open_min, night_close_min, timezone
		FROM service_windows WHERE restaurant_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]ServiceWindow)
	for rows.Next() {
		var id uuid.UUID
		var w ServiceWindow
		if err := rows.Scan(&id, &w.OpenMin, &w.CloseMin, &w.NightOpenMin, &w.NightCloseMin, &w.TZ); err != nil {
			return nil, err
		}
		rid := id
		w.RestaurantID = &rid
		out[id] = w
	}
	return out, rows.Err()
}

// GetRestaurantServiceWindow returns a restaurant's own service window and whether it has
// one. When the restaurant has no row, ok is false and the fleet default is returned.
func (d *DB) GetRestaurantServiceWindow(ctx context.Context, restaurantID uuid.UUID) (w ServiceWindow, ok bool, err error) {
	err = d.pool.QueryRow(ctx, `
		SELECT open_min, close_min, night_open_min, night_close_min, timezone
		FROM service_windows WHERE restaurant_id = $1`, restaurantID).
		Scan(&w.OpenMin, &w.CloseMin, &w.NightOpenMin, &w.NightCloseMin, &w.TZ)
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
	_, err := d.pool.Exec(ctx, `
		INSERT INTO service_windows (restaurant_id, open_min, close_min, night_open_min, night_close_min, timezone, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (restaurant_id) WHERE restaurant_id IS NOT NULL DO UPDATE SET
			open_min = EXCLUDED.open_min, close_min = EXCLUDED.close_min,
			night_open_min = EXCLUDED.night_open_min, night_close_min = EXCLUDED.night_close_min,
			timezone = EXCLUDED.timezone, updated_at = NOW()
	`, w.RestaurantID, w.OpenMin, w.CloseMin, w.NightOpenMin, w.NightCloseMin, w.TZ)
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
		       sw.open_min, sw.close_min, sw.night_open_min, sw.night_close_min, sw.timezone
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
		if err := rows.Scan(&id, &rid, &devTZ, &open, &closeM, &nOpen, &nClose, &winTZ); err != nil {
			return nil, err
		}
		w := fleet // copy fleet defaults, override with the group's window when present
		w.RestaurantID = rid
		if open != nil {
			w.OpenMin, w.CloseMin, w.NightOpenMin, w.NightCloseMin = *open, *closeM, *nOpen, *nClose
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
