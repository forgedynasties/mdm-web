package db

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// TempFastKeepHours is how long a fast temperature reading is kept. A day covers a
// validation run with room to look at it; the minute-by-minute history in
// device_samples is what outlives it.
const TempFastKeepHours = 24

// TempFastSetting is a device's fast-temperature switch: the interval it reports at and
// when that stops.
type TempFastSetting struct {
	IntervalSec int       `json:"interval_sec"`
	Until       time.Time `json:"until"`
}

// TempFast returns the device's setting while it is running; ok is false when it is off
// or has run out.
func (d *DB) TempFast(ctx context.Context, deviceID uuid.UUID) (s TempFastSetting, ok bool, err error) {
	err = d.pool.QueryRow(ctx, `
		SELECT interval_sec, until FROM temp_fast_devices
		WHERE device_id = $1 AND until > NOW()`, deviceID).Scan(&s.IntervalSec, &s.Until)
	if errors.Is(err, pgx.ErrNoRows) {
		return s, false, nil
	}
	if err != nil {
		return s, false, err
	}
	return s, true, nil
}

// SetTempFast switches fast temperature on for hours at intervalSec, or off when hours
// is 0.
func (d *DB) SetTempFast(ctx context.Context, deviceID uuid.UUID, intervalSec, hours int) error {
	if hours <= 0 {
		_, err := d.pool.Exec(ctx, `DELETE FROM temp_fast_devices WHERE device_id = $1`, deviceID)
		return err
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO temp_fast_devices (device_id, interval_sec, until)
		VALUES ($1, $2, NOW() + make_interval(hours => $3))
		ON CONFLICT (device_id) DO UPDATE
		SET interval_sec = EXCLUDED.interval_sec, until = EXCLUDED.until, created_at = NOW()`,
		deviceID, intervalSec, hours)
	return err
}

// InsertTempFast keeps one fast reading. Two frames in the same microsecond collide on
// the key; the second is the same reading and is dropped.
func (d *DB) InsertTempFast(ctx context.Context, deviceID uuid.UUID, tempC float64, uptimeS *int64) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_temp_fast (device_id, temp_c, uptime_s) VALUES ($1, $2, $3)
		ON CONFLICT DO NOTHING`, deviceID, tempC, uptimeS)
	return err
}

// TempFastReading is one stored fast reading.
type TempFastReading struct {
	At      time.Time `json:"at"`
	TempC   float64   `json:"temp_c"`
	UptimeS *int64    `json:"uptime_s,omitempty"`
}

// TempFastSince returns the device's fast readings after since, oldest first.
func (d *DB) TempFastSince(ctx context.Context, deviceID uuid.UUID, since time.Time) ([]TempFastReading, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT at, temp_c, uptime_s FROM device_temp_fast
		WHERE device_id = $1 AND at > $2 ORDER BY at`, deviceID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []TempFastReading{}
	for rows.Next() {
		var r TempFastReading
		if err := rows.Scan(&r.At, &r.TempC, &r.UptimeS); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// PruneTempFast deletes readings older than TempFastKeepHours and settings that ran out
// more than a day ago (kept that long so the API can still say when a run ended).
func (d *DB) PruneTempFast(ctx context.Context) (int64, error) {
	r, err := d.pool.Exec(ctx, `DELETE FROM device_temp_fast WHERE at < NOW() - make_interval(hours => $1)`, TempFastKeepHours)
	if err != nil {
		return 0, err
	}
	if _, err := d.pool.Exec(ctx, `DELETE FROM temp_fast_devices WHERE until < NOW() - INTERVAL '1 day'`); err != nil {
		return r.RowsAffected(), err
	}
	return r.RowsAffected(), nil
}
