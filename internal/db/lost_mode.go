package db

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// DeviceCommandOutcome is the newest command of one type sent to one device, with how
// that device answered ("" while it has not).
type DeviceCommandOutcome struct {
	Command
	Status string
}

// LatestDeviceCommand returns the newest command of a type that names this device
// directly, or nil when there is none. Used by lost mode: who marked it, when, and
// whether the device has picked it up yet.
func (d *DB) LatestDeviceCommand(ctx context.Context, deviceID uuid.UUID, cmdType string) (*DeviceCommandOutcome, error) {
	var o DeviceCommandOutcome
	err := d.pool.QueryRow(ctx, `
		SELECT c.id, c.type, c.apk_url, c.payload, c.target_type, c.created_by, c.created_at, COALESCE(cs.status, '')
		FROM commands c
		JOIN command_targets ct ON ct.command_id = c.id AND ct.target_id = $1
		LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = $1
		WHERE c.type = $2 AND c.target_type = 'devices'
		ORDER BY c.created_at DESC LIMIT 1`, deviceID, cmdType).
		Scan(&o.ID, &o.Type, &o.ApkURL, &o.Payload, &o.TargetType, &o.CreatedBy, &o.CreatedAt, &o.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &o, nil
}
