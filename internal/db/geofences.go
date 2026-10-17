package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ── Geofences ─────────────────────────────────────────────────────────────────

// Geofence is a named circle; devices are matched against it live from the GPS
// fix in their latest_extra, so there is no stored membership to keep in sync.
type Geofence struct {
	ID        uuid.UUID
	Name      string
	Lat       float64
	Lon       float64
	RadiusM   int
	CreatedAt time.Time
}

func (d *DB) CreateGeofence(ctx context.Context, name string, lat, lon float64, radiusM int) (uuid.UUID, error) {
	var id uuid.UUID
	err := d.pool.QueryRow(ctx, `
		INSERT INTO geofences (name, lat, lon, radius_m)
		VALUES ($1, $2, $3, $4) RETURNING id`, name, lat, lon, radiusM).Scan(&id)
	return id, err
}

func (d *DB) ListGeofences(ctx context.Context) ([]Geofence, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, name, lat, lon, radius_m, created_at
		FROM geofences ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Geofence
	for rows.Next() {
		var g Geofence
		if err := rows.Scan(&g.ID, &g.Name, &g.Lat, &g.Lon, &g.RadiusM, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (d *DB) DeleteGeofence(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM geofences WHERE id = $1`, id)
	return err
}
