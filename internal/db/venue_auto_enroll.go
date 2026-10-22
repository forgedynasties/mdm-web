package db

import (
	"context"

	"github.com/google/uuid"
)

// Auto-enrol per venue (22 Oct 2026; user: "add option to auto enroll yes or no per
// venue"). restaurants.auto_enroll, off by default; the scout acts on it (scout.go).

// RestaurantAutoEnroll reports whether the venue enrols found devices by itself.
func (d *DB) RestaurantAutoEnroll(ctx context.Context, id uuid.UUID) (bool, error) {
	var on bool
	err := d.pool.QueryRow(ctx, `SELECT auto_enroll FROM restaurants WHERE id = $1`, id).Scan(&on)
	return on, err
}

// SetRestaurantAutoEnroll switches it.
func (d *DB) SetRestaurantAutoEnroll(ctx context.Context, id uuid.UUID, on bool) error {
	_, err := d.pool.Exec(ctx, `UPDATE restaurants SET auto_enroll = $2 WHERE id = $1`, id, on)
	return err
}

// SightingAt returns the venue's sighting of a serial and its state.
func (d *DB) SightingAt(ctx context.Context, restaurantID uuid.UUID, serial string) (uuid.UUID, string, error) {
	var id uuid.UUID
	var state string
	err := d.pool.QueryRow(ctx, `SELECT id, state FROM sightings WHERE restaurant_id = $1 AND serial = $2`,
		restaurantID, serial).Scan(&id, &state)
	return id, state, err
}
