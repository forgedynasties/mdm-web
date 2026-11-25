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

// VenueAutoEnrollRow is one venue in the all-venues auto-enrol switch.
type VenueAutoEnrollRow struct {
	ID         uuid.UUID `json:"id"`
	Name       string    `json:"name"`
	AutoEnroll bool      `json:"auto_enroll"`
	Scouts     int       `json:"scouts"` // firmware devices that can sweep its Wi-Fi
}

// ListVenueAutoEnroll lists every venue with its auto-enrol setting and scout count.
func (d *DB) ListVenueAutoEnroll(ctx context.Context) ([]VenueAutoEnrollRow, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT r.id, r.name, r.auto_enroll,
		       (SELECT count(*) FROM devices dv
		         WHERE dv.restaurant_id = r.id AND dv.agent_kind = 'firmware'
		           AND dv.enrollment_status NOT IN ('retired','wiped','unenrolled'))::int
		  FROM restaurants r ORDER BY r.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VenueAutoEnrollRow
	for rows.Next() {
		var v VenueAutoEnrollRow
		if err := rows.Scan(&v.ID, &v.Name, &v.AutoEnroll, &v.Scouts); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// SetAllVenuesAutoEnroll switches auto-enrol on every venue; returns how many changed.
func (d *DB) SetAllVenuesAutoEnroll(ctx context.Context, on bool) (int64, error) {
	tag, err := d.pool.Exec(ctx, `UPDATE restaurants SET auto_enroll = $1 WHERE auto_enroll <> $1`, on)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// SightingAt returns the venue's sighting of a serial and its state.
func (d *DB) SightingAt(ctx context.Context, restaurantID uuid.UUID, serial string) (uuid.UUID, string, error) {
	var id uuid.UUID
	var state string
	err := d.pool.QueryRow(ctx, `SELECT id, state FROM sightings WHERE restaurant_id = $1 AND serial = $2`,
		restaurantID, serial).Scan(&id, &state)
	return id, state, err
}
