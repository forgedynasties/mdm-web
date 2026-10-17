package db

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// DeviceBatteryClimb is one stored charge climb with the device it came from, for the
// fleet-wide wear comparison (/battery "Wear").
type DeviceBatteryClimb struct {
	DeviceID uuid.UUID
	Serial   string
	Venue    string
	At       time.Time
	Payload  json.RawMessage
}

// BatteryClimbsSince returns every visible device's climbs since `since`, oldest first.
// The table holds about one row per T7 per night, so a month of the whole fleet is a few
// thousand rows.
func (d *DB) BatteryClimbsSince(ctx context.Context, since time.Time) ([]DeviceBatteryClimb, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT c.device_id, d.serial_number, COALESCE(r.name, ''), c.at, c.payload
		FROM battery_climbs c
		JOIN devices d ON d.id = c.device_id
		LEFT JOIN restaurants r ON r.id = d.restaurant_id
		WHERE c.at >= $1 AND NOT d.hidden
		ORDER BY c.at`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceBatteryClimb
	for rows.Next() {
		var c DeviceBatteryClimb
		if err := rows.Scan(&c.DeviceID, &c.Serial, &c.Venue, &c.At, &c.Payload); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}
