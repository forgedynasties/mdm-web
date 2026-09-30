package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// CommandLog is one device's output for a collect_logs command, with what the bundle's
// header says about the device.
type CommandLog struct {
	DeviceID   uuid.UUID
	Serial     string
	Restaurant string
	BuildID    string
	At         time.Time
	Output     string
}

// ListCommandLogs returns the collected output of a command, one row per device that
// sent some, optionally only the device with serial. Output is loaded here and only
// here: the command page lists sizes (see GetCommandDeliveries), not megabytes of text.
func (d *DB) ListCommandLogs(ctx context.Context, commandID uuid.UUID, serial string) ([]CommandLog, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT dv.id, dv.serial_number, COALESCE(r.name, ''), COALESCE(dv.build_id, ''),
		       COALESCE(cs.updated_at, NOW()), cr.output
		FROM command_results cr
		JOIN devices dv ON dv.id = cr.device_id
		LEFT JOIN restaurants r ON r.id = dv.restaurant_id
		LEFT JOIN command_status cs ON cs.command_id = cr.command_id AND cs.device_id = cr.device_id
		WHERE cr.command_id = $1 AND ($2 = '' OR dv.serial_number = $2) AND cr.output <> ''
		ORDER BY dv.serial_number`, commandID, serial)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CommandLog
	for rows.Next() {
		var l CommandLog
		if err := rows.Scan(&l.DeviceID, &l.Serial, &l.Restaurant, &l.BuildID, &l.At, &l.Output); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}
