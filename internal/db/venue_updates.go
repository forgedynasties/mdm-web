package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// When devices move, per venue (V2, picked 22 Oct 2026). The fleet window on Lineup
// (config RoleRollouts) says when deployed devices take a new prod firmware and MDM client
// updates by themselves; a venue can follow it, move only while it is closed (outside its
// service hours), use its own window, or be paused, for a while or until resumed.

// Venue update modes.
const (
	VenueUpdateFleet  = ""
	VenueUpdateClosed = "closed"
	VenueUpdateOwn    = "own"
)

// VenueUpdatePolicy is one venue's setting. Paused sits over Mode, so Resume brings the
// mode back.
type VenueUpdatePolicy struct {
	Mode        string
	Start, End  string // "HH:MM", Mode own
	Paused      bool
	PausedUntil *time.Time // nil with Paused = until someone resumes
	By          string
	At          *time.Time
}

// PausedAt reports whether the pause holds at now (an ended timed pause does not).
func (p VenueUpdatePolicy) PausedAt(now time.Time) bool {
	return p.Paused && (p.PausedUntil == nil || now.Before(*p.PausedUntil))
}

// IsDefault is a venue that follows the fleet and is not paused.
func (p VenueUpdatePolicy) IsDefault(now time.Time) bool {
	return p.Mode == VenueUpdateFleet && !p.PausedAt(now)
}

func IsVenueUpdateMode(m string) bool {
	return m == VenueUpdateFleet || m == VenueUpdateClosed || m == VenueUpdateOwn
}

const venueUpdateCols = `update_mode, update_window_start, update_window_end, update_paused,
	update_paused_until, update_set_by, update_set_at`

func scanVenueUpdate(sc interface{ Scan(...any) error }, extra ...any) (VenueUpdatePolicy, error) {
	var p VenueUpdatePolicy
	args := append(extra, &p.Mode, &p.Start, &p.End, &p.Paused, &p.PausedUntil, &p.By, &p.At)
	err := sc.Scan(args...)
	return p, err
}

// VenueUpdatePolicy returns one venue's setting (the zero value = follow the fleet).
func (d *DB) VenueUpdatePolicy(ctx context.Context, id uuid.UUID) (VenueUpdatePolicy, error) {
	return scanVenueUpdate(d.pool.QueryRow(ctx, `SELECT `+venueUpdateCols+` FROM restaurants WHERE id = $1`, id))
}

// VenueUpdatePolicies returns every venue that does not simply follow the fleet, for the
// update jobs (one query per tick).
func (d *DB) VenueUpdatePolicies(ctx context.Context) (map[uuid.UUID]VenueUpdatePolicy, error) {
	rows, err := d.pool.Query(ctx, `SELECT id, `+venueUpdateCols+` FROM restaurants
		WHERE update_mode <> '' OR update_paused`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]VenueUpdatePolicy{}
	for rows.Next() {
		var id uuid.UUID
		p, err := scanVenueUpdate(rows, &id)
		if err != nil {
			return nil, err
		}
		out[id] = p
	}
	return out, rows.Err()
}

// SetVenueUpdatePolicy stores one venue's setting.
func (d *DB) SetVenueUpdatePolicy(ctx context.Context, id uuid.UUID, p VenueUpdatePolicy) error {
	_, err := d.pool.Exec(ctx, `UPDATE restaurants SET update_mode = $2, update_window_start = $3,
		update_window_end = $4, update_paused = $5, update_paused_until = $6, update_set_by = $7,
		update_set_at = NOW() WHERE id = $1`, id, p.Mode, p.Start, p.End, p.Paused, p.PausedUntil, p.By)
	return err
}
