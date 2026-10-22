package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Online / offline history (network graph N1, 22 Oct 2026; user: "in network graph, also
// when device was offline and when online"). Until now nothing recorded it: only gaps in
// readings and offline alerts hinted at it. Each change is kept as a device_state_events
// row with key "online" ("true" / "false"), so it shares the state history's retention
// and its "last value before the window" handling. Written by dashboard.RunPresenceRecord.

// PresenceKey is the state key the online history is kept under.
const PresenceKey = "online"

// PresenceRow is a device's last contact and its last recorded online state ("" = none).
type PresenceRow struct {
	ID       uuid.UUID
	LastSeen time.Time
	Recorded string
}

// PresenceRows lists every visible device with its last recorded online state.
func (d *DB) PresenceRows(ctx context.Context) ([]PresenceRow, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT dv.id, dv.last_seen_at, COALESCE(p.to_val, '')
		FROM devices dv
		LEFT JOIN LATERAL (
			SELECT to_val FROM device_state_events e
			WHERE e.device_id = dv.id AND e.key = 'online'
			ORDER BY e.at DESC LIMIT 1) p ON true
		WHERE NOT dv.hidden`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PresenceRow
	for rows.Next() {
		var r PresenceRow
		if err := rows.Scan(&r.ID, &r.LastSeen, &r.Recorded); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// RecordPresence stores an online/offline change at the given instant.
func (d *DB) RecordPresence(ctx context.Context, deviceID uuid.UUID, at time.Time, online bool, prev string) error {
	to := "false"
	if online {
		to = "true"
	}
	_, err := d.pool.Exec(ctx, `INSERT INTO device_state_events (device_id, at, key, from_val, to_val) VALUES ($1, $2, 'online', $3, $4)`,
		deviceID, at.UTC(), prev, to)
	return err
}
