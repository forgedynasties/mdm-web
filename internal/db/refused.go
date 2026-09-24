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

// RefusedCheckinsForSerial returns every source refused under this serial, most recent first.
func (d *DB) RefusedCheckinsForSerial(ctx context.Context, serial string) ([]RefusedCheckin, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT serial, remote_ip, build_id, first_at, last_at, attempts
		FROM refused_checkins WHERE serial = $1 ORDER BY last_at DESC LIMIT 50`, serial)
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

// NoteDeviceRemoteIP records the public address a device last reached the server from,
// written only when it changes. It is what lets a refused tablet — which has no record
// of its own — be placed: the devices already checking in from the same address are
// almost always at the same site.
func (d *DB) NoteDeviceRemoteIP(ctx context.Context, serial, ip string) error {
	if ip == "" {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE devices SET last_remote_ip = $2
		WHERE serial_number = $1 AND last_remote_ip IS DISTINCT FROM $2`, serial, ip)
	return err
}

// DeviceAtAddress is a device that last reached the server from a given public address.
type DeviceAtAddress struct {
	Serial     string
	Restaurant string
	LastSeen   *time.Time
}

// DevicesAtRemoteIP lists the devices whose last public address is ip, most recently
// seen first — the likely location of a refused tablet reaching us from the same place.
func (d *DB) DevicesAtRemoteIP(ctx context.Context, ip string, limit int) ([]DeviceAtAddress, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.serial_number, COALESCE(r.name, ''), d.last_seen_at
		FROM devices d LEFT JOIN restaurants r ON r.id = d.restaurant_id
		WHERE d.last_remote_ip = $1
		ORDER BY d.last_seen_at DESC NULLS LAST LIMIT $2`, ip, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceAtAddress
	for rows.Next() {
		var x DeviceAtAddress
		if err := rows.Scan(&x.Serial, &x.Restaurant, &x.LastSeen); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
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
