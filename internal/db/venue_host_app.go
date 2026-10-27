package db

import (
	"context"
	"encoding/json"

	"github.com/google/uuid"
)

// Host app per venue, per product (V1 23 Oct 2026, per product since H1 27 Oct; user:
// "venue config for host app should be per host app"). restaurants.host_app_channels is
// role → channel suffix ("t7" → "uatv2" lands aio.app.nugget.uatv2 on the venue's TSAI
// tablets); a role not listed stays on prod. The old one-for-every-product column
// host_app_channel was folded into it by the migration and is no longer read.

// VenueHostChannels maps each venue with any product off prod to its role → channel map.
func (d *DB) VenueHostChannels(ctx context.Context) (map[uuid.UUID]map[string]string, error) {
	rows, err := d.pool.Query(ctx, `SELECT id, host_app_channels FROM restaurants WHERE host_app_channels <> '{}'::jsonb`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]map[string]string{}
	for rows.Next() {
		var id uuid.UUID
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, err
		}
		m := map[string]string{}
		if json.Unmarshal(raw, &m) == nil && len(m) > 0 {
			out[id] = m
		}
	}
	return out, rows.Err()
}

// SetVenueHostChannels replaces a venue's role → channel map; an empty channel (prod) is
// left out.
func (d *DB) SetVenueHostChannels(ctx context.Context, id uuid.UUID, chans map[string]string) error {
	m := map[string]string{}
	for role, ch := range chans {
		if ch != "" {
			m[role] = ch
		}
	}
	raw, _ := json.Marshal(m)
	_, err := d.pool.Exec(ctx, `UPDATE restaurants SET host_app_channels = $2 WHERE id = $1`, id, raw)
	return err
}
