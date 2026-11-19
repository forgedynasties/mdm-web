package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Alerts pause per venue (user, 19 Nov 2026: the AIO Lab venue holds hundreds of dark lab
// devices whose offline/critical alerts would flood the channels, the Alerts page and the
// Overview). While it holds: no alert is raised for a device at the venue (upsertAlert),
// nothing is sent to channels or digests for its devices, and alerts already open stay in
// the database but are left out of every list and count (AlertHeldSQL). They come back by
// themselves when the pause ends or is resumed. alerts_paused_until NULL with
// alerts_paused = until resumed. Devices at no venue are never affected.

// AlertsPausedSQL is true for a venue row `r` whose alerts pause holds now.
const AlertsPausedSQL = `(r.alerts_paused AND (r.alerts_paused_until IS NULL OR r.alerts_paused_until > NOW()))`

// AlertHeldSQL is true for an alerts row aliased `a` whose device sits at a venue with the
// pause on. Use `NOT ` + AlertHeldSQL("a") wherever alerts are listed or counted.
func AlertHeldSQL(a string) string {
	return `EXISTS (SELECT 1 FROM devices hd JOIN restaurants r ON r.id = hd.restaurant_id
		WHERE hd.id = ` + a + `.device_id AND ` + AlertsPausedSQL + `)`
}

// DeviceAlertsHeldSQL is the same for a devices row aliased `dv`.
func DeviceAlertsHeldSQL(dv string) string {
	return `EXISTS (SELECT 1 FROM restaurants r WHERE r.id = ` + dv + `.restaurant_id AND ` + AlertsPausedSQL + `)`
}

// VenueAlertsPause is one venue's alerts pause.
type VenueAlertsPause struct {
	Paused      bool
	PausedUntil *time.Time
	By          string
	At          *time.Time
}

// PausedAt reports whether the pause holds at now.
func (p VenueAlertsPause) PausedAt(now time.Time) bool {
	return p.Paused && (p.PausedUntil == nil || p.PausedUntil.After(now))
}

// VenueAlertsPause reads one venue's alerts pause.
func (d *DB) VenueAlertsPause(ctx context.Context, id uuid.UUID) (VenueAlertsPause, error) {
	var p VenueAlertsPause
	err := d.pool.QueryRow(ctx, `SELECT alerts_paused, alerts_paused_until, alerts_set_by, alerts_set_at
		FROM restaurants WHERE id = $1`, id).Scan(&p.Paused, &p.PausedUntil, &p.By, &p.At)
	return p, err
}

// SetVenueAlertsPause stores one venue's alerts pause.
func (d *DB) SetVenueAlertsPause(ctx context.Context, id uuid.UUID, p VenueAlertsPause) error {
	_, err := d.pool.Exec(ctx, `UPDATE restaurants SET alerts_paused = $2, alerts_paused_until = $3,
		alerts_set_by = $4, alerts_set_at = NOW() WHERE id = $1`, id, p.Paused, p.PausedUntil, p.By)
	return err
}

// VenueHeldAlertCount is how many open alerts of the venue's devices the pause is holding
// back (counted whether or not the pause still holds; the caller only asks while it does).
func (d *DB) VenueHeldAlertCount(ctx context.Context, id uuid.UUID) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM alerts a JOIN devices dv ON dv.id = a.device_id
		WHERE dv.restaurant_id = $1 AND a.status <> 'resolved' AND NOT dv.hidden`, id).Scan(&n)
	return n, err
}

// DevicesAlertsHeld returns which of ids sit at a venue whose alerts pause holds.
func (d *DB) DevicesAlertsHeld(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `SELECT dv.id FROM devices dv WHERE dv.id = ANY($1) AND `+DeviceAlertsHeldSQL("dv"), ids)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return out, err
		}
		out[id] = true
	}
	return out, rows.Err()
}
