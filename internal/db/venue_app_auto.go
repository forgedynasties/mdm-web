package db

import (
	"context"

	"github.com/google/uuid"
)

// Role app auto-install per venue (restaurants.app_auto_install, default true): off makes
// RunRoleAppInstalls skip the venue's devices.

// VenueAppAutoInstall reports whether the venue installs the role app automatically.
func (d *DB) VenueAppAutoInstall(ctx context.Context, id uuid.UUID) (bool, error) {
	on := true
	err := d.pool.QueryRow(ctx, `SELECT app_auto_install FROM restaurants WHERE id = $1`, id).Scan(&on)
	return on, err
}

// VenueAppAutoInstallOff is the set of venues with auto-install switched off.
func (d *DB) VenueAppAutoInstallOff(ctx context.Context) (map[uuid.UUID]bool, error) {
	rows, err := d.pool.Query(ctx, `SELECT id FROM restaurants WHERE NOT app_auto_install`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// SetVenueAppAutoInstall switches it.
func (d *DB) SetVenueAppAutoInstall(ctx context.Context, id uuid.UUID, on bool) error {
	_, err := d.pool.Exec(ctx, `UPDATE restaurants SET app_auto_install = $2 WHERE id = $1`, id, on)
	return err
}
