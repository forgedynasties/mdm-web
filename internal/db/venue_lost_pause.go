package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Lost-device pause per venue (L1, picked 22 Oct 2026; user: "add option to pause device
// lost per venue too"). While it holds, a tablet leaving the venue raises no left_venue
// alert and gets no self-lock (its config says self_lock enabled=false); the venue's
// self-lock time is kept and comes back when the pause ends or is resumed. Marking a
// device lost by hand, Bluetooth listening and the lost-device-seen/heard alerts are not
// paused.

// LostPausedSQL is true for a venue row `r` whose lost-device pause holds now.
const LostPausedSQL = `(r.lost_paused AND (r.lost_paused_until IS NULL OR r.lost_paused_until > NOW()))`

// VenueLostWatch is one venue's lost-device setting.
type VenueLostWatch struct {
	SelfLockMinutes int // 0 = alerts only
	Paused          bool
	PausedUntil     *time.Time // nil with Paused = until someone resumes
	By              string
	At              *time.Time
}

// PausedAt reports whether the pause holds at now (an ended timed pause does not).
func (w VenueLostWatch) PausedAt(now time.Time) bool {
	return w.Paused && (w.PausedUntil == nil || now.Before(*w.PausedUntil))
}

// VenueLostWatch reads one venue's lost-device setting.
func (d *DB) VenueLostWatch(ctx context.Context, id uuid.UUID) (VenueLostWatch, error) {
	var w VenueLostWatch
	err := d.pool.QueryRow(ctx, `SELECT self_lock_minutes, lost_paused, lost_paused_until, lost_set_by, lost_set_at
		FROM restaurants WHERE id = $1`, id).Scan(&w.SelfLockMinutes, &w.Paused, &w.PausedUntil, &w.By, &w.At)
	return w, err
}

// SetVenueLostWatch stores one venue's lost-device setting and moves the config generation
// so its devices' next answer carries the self-lock change.
func (d *DB) SetVenueLostWatch(ctx context.Context, id uuid.UUID, w VenueLostWatch) error {
	_, err := d.pool.Exec(ctx, `UPDATE restaurants SET self_lock_minutes = $2, lost_paused = $3,
		lost_paused_until = $4, lost_set_by = $5, lost_set_at = NOW() WHERE id = $1`,
		id, w.SelfLockMinutes, w.Paused, w.PausedUntil, w.By)
	if err == nil {
		lostConfigGen.Add(1)
	}
	return err
}
