package db

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// WifiOp is the latest Wi-Fi change someone sent a device from its page (W1): what was
// asked, how far it got, and the device's reply.
type WifiOp struct {
	Action    string
	SSID      string
	Status    string // pending | sent | received | running | completed | failed | expired
	Output    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

// LatestWifiOp returns the newest wifi_config a person sent this device in the last
// `within`, nil when none. The venue's own pushes are left out: they are not something
// someone is watching from the device page.
func (d *DB) LatestWifiOp(ctx context.Context, deviceID uuid.UUID, within time.Duration, venueActor string) (*WifiOp, error) {
	var op WifiOp
	err := d.pool.QueryRow(ctx, `
		SELECT COALESCE(c.payload->>'action', ''), COALESCE(c.payload->>'ssid', ''), cs.status,
		       COALESCE(cr.output, ''), c.created_at, cs.updated_at
		FROM commands c
		JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = $1
		LEFT JOIN command_results cr ON cr.command_id = c.id AND cr.device_id = $1
		WHERE c.type = 'wifi_config' AND c.created_at > $2 AND COALESCE(c.created_by, '') <> $3
		ORDER BY c.created_at DESC LIMIT 1`, deviceID, time.Now().Add(-within), venueActor).
		Scan(&op.Action, &op.SSID, &op.Status, &op.Output, &op.CreatedAt, &op.UpdatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &op, nil
}
