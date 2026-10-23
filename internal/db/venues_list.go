package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ServiceOpenAt reports whether a venue is inside its service hours at t. The window's
// own timezone wins, else the venue's (IANA or GMT±h), else UTC. Used by the Venues
// list (V1, 21 Oct 2026) for "open now" and venues open by hour.
func ServiceOpenAt(w ServiceWindow, venueTZ string, t time.Time) bool {
	tz := w.TZ
	if tz == "" {
		tz = venueTZ
	}
	lt := localTime(t, tz)
	w = w.On(lt)
	return inMinWindow(lt.Hour()*60+lt.Minute(), w.OpenMin, w.CloseMin)
}

// RestaurantStaffPINs returns every venue's staff PIN ("" = still the client default,
// DefaultStaffPIN), for the Venues list's "needs a look".
func (d *DB) RestaurantStaffPINs(ctx context.Context) (map[uuid.UUID]string, error) {
	rows, err := d.pool.Query(ctx, `SELECT id, COALESCE(staff_pin, '') FROM restaurants`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var pin string
		if err := rows.Scan(&id, &pin); err != nil {
			return nil, err
		}
		out[id] = pin
	}
	return out, rows.Err()
}
