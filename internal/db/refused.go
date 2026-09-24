package db

import (
	"context"
	"time"
)

// RefusedCheckin is one source of check-ins the server refused for a corrupted serial
// (see api.validSerial): nothing of theirs is stored, but a tablet stuck like that must
// not simply vanish from the dashboard, so the server keeps who, from where, and how often.
type RefusedCheckin struct {
	Serial   string
	RemoteIP string
	BuildID  string
	FirstAt  time.Time
	LastAt   time.Time
	Attempts int64
}

// RecordRefusedCheckin counts one refused check-in, per serial and source address.
func (d *DB) RecordRefusedCheckin(ctx context.Context, serial, remoteIP, buildID string) error {
	if len(serial) > 64 {
		serial = serial[:64]
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO refused_checkins (serial, remote_ip, build_id) VALUES ($1, $2, $3)
		ON CONFLICT (serial, remote_ip) DO UPDATE
			SET last_at = NOW(), attempts = refused_checkins.attempts + 1,
			    build_id = CASE WHEN EXCLUDED.build_id <> '' THEN EXCLUDED.build_id ELSE refused_checkins.build_id END`,
		serial, remoteIP, buildID)
	return err
}

// RecentRefusedCheckins returns the sources refused within `within`, most recent first.
func (d *DB) RecentRefusedCheckins(ctx context.Context, within time.Duration) ([]RefusedCheckin, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT serial, remote_ip, build_id, first_at, last_at, attempts
		FROM refused_checkins WHERE last_at > NOW() - make_interval(secs => $1)
		ORDER BY last_at DESC LIMIT 50`, within.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RefusedCheckin
	for rows.Next() {
		var r RefusedCheckin
		if err := rows.Scan(&r.Serial, &r.RemoteIP, &r.BuildID, &r.FirstAt, &r.LastAt, &r.Attempts); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}
