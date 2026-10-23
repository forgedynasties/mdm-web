package db

import (
	"context"

	"github.com/google/uuid"
)

// Host app channel per venue (V1, 23 Oct 2026; user: "setting which variant of host app
// to land per venue, override, default is prod"). restaurants.host_app_channel: '' = prod,
// else the variant suffix ("uatv2") every product's app lands as at that venue.

// VenueHostChannels maps each venue with a channel set to it.
func (d *DB) VenueHostChannels(ctx context.Context) (map[uuid.UUID]string, error) {
	rows, err := d.pool.Query(ctx, `SELECT id, host_app_channel FROM restaurants WHERE host_app_channel <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var ch string
		if err := rows.Scan(&id, &ch); err != nil {
			return nil, err
		}
		out[id] = ch
	}
	return out, rows.Err()
}

// SetVenueHostChannel sets a venue's channel (” = back to prod).
func (d *DB) SetVenueHostChannel(ctx context.Context, id uuid.UUID, channel string) error {
	_, err := d.pool.Exec(ctx, `UPDATE restaurants SET host_app_channel = $2 WHERE id = $1`, id, channel)
	return err
}
