package db

import (
	"context"
	"errors"
	"time"

	"github.com/jackc/pgx/v5"
)

// DeviceAdbKey is which of our adb keys a device takes, and how we found out.
type DeviceAdbKey struct {
	Serial string
	Label  string
	Source string // enroll-app | scout | client
	SeenBy string
	SeenAt time.Time
}

// PutDeviceAdbKey records the key a device accepted. The newest report wins: an image can
// be rebuilt with a different key, and then the old record is simply wrong.
func (d *DB) PutDeviceAdbKey(ctx context.Context, serial, label, source, seenBy string) error {
	if serial == "" || label == "" {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_adb_key (serial, label, source, seen_by, seen_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (serial) DO UPDATE SET
		  label = EXCLUDED.label, source = EXCLUDED.source, seen_by = EXCLUDED.seen_by, seen_at = NOW()`,
		serial, label, source, seenBy)
	return err
}

// GetDeviceAdbKey is what one device takes, or nil when nobody has reported it.
func (d *DB) GetDeviceAdbKey(ctx context.Context, serial string) (*DeviceAdbKey, error) {
	var k DeviceAdbKey
	err := d.pool.QueryRow(ctx, `
		SELECT serial, label, source, seen_by, seen_at FROM device_adb_key WHERE serial = $1`, serial).
		Scan(&k.Serial, &k.Label, &k.Source, &k.SeenBy, &k.SeenAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &k, nil
}

// CountDevicesByAdbKey is how many devices take each key — the number that matters when one
// has to be rotated.
func (d *DB) CountDevicesByAdbKey(ctx context.Context) (map[string]int, error) {
	rows, err := d.pool.Query(ctx, `SELECT label, COUNT(*) FROM device_adb_key GROUP BY label`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var l string
		var n int
		if err := rows.Scan(&l, &n); err != nil {
			return nil, err
		}
		out[l] = n
	}
	return out, rows.Err()
}

// DevicesByAdbKey lists the serials that take one key, newest first.
func (d *DB) DevicesByAdbKey(ctx context.Context, label string, limit int) ([]DeviceAdbKey, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT serial, label, source, seen_by, seen_at FROM device_adb_key
		WHERE label = $1 ORDER BY seen_at DESC LIMIT $2`, label, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceAdbKey
	for rows.Next() {
		var k DeviceAdbKey
		if err := rows.Scan(&k.Serial, &k.Label, &k.Source, &k.SeenBy, &k.SeenAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}
