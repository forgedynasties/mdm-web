package db

import (
	"context"
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Device struct {
	ID           uuid.UUID `json:"id"`
	SerialNumber string    `json:"serial_number"`
	BuildID      string    `json:"build_id"`
	BatteryPct   int       `json:"battery_pct"`
	LastSeenAt   time.Time `json:"last_seen_at"`
	CreatedAt    time.Time `json:"created_at"`
}

type Checkin struct {
	ID         uuid.UUID       `json:"id"`
	DeviceID   uuid.UUID       `json:"device_id"`
	BatteryPct int             `json:"battery_pct"`
	BuildID    string          `json:"build_id"`
	Extra      json.RawMessage `json:"extra"`
	CreatedAt  time.Time       `json:"created_at"`
}

type DB struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, connStr string) (*DB, error) {
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		return nil, err
	}
	return &DB{pool: pool}, nil
}

func (d *DB) Close() {
	d.pool.Close()
}

func (d *DB) Ping(ctx context.Context) error {
	return d.pool.Ping(ctx)
}

func (d *DB) RunMigrations(ctx context.Context) error {
	_, err := d.pool.Exec(ctx, migrationSQL)
	return err
}

func (d *DB) UpsertCheckin(ctx context.Context, serial, buildID string, batteryPct int, extra json.RawMessage) error {
	if len(extra) == 0 {
		extra = json.RawMessage("{}")
	}

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	var deviceID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO devices (serial_number, build_id, last_seen_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (serial_number) DO UPDATE
			SET build_id     = EXCLUDED.build_id,
			    last_seen_at = NOW()
		RETURNING id
	`, serial, buildID).Scan(&deviceID)
	if err != nil {
		return err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO checkins (device_id, battery_pct, build_id, extra)
		VALUES ($1, $2, $3, $4)
	`, deviceID, batteryPct, buildID, extra)
	if err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (d *DB) ListDevices(ctx context.Context) ([]Device, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT
			d.id, d.serial_number, d.build_id, d.last_seen_at, d.created_at,
			COALESCE(c.battery_pct, 0) AS battery_pct
		FROM devices d
		LEFT JOIN LATERAL (
			SELECT battery_pct FROM checkins
			WHERE device_id = d.id
			ORDER BY created_at DESC
			LIMIT 1
		) c ON true
		ORDER BY d.last_seen_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []Device
	for rows.Next() {
		var dev Device
		if err := rows.Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct); err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	return devices, rows.Err()
}

func (d *DB) GetDevice(ctx context.Context, serial string) (*Device, error) {
	var dev Device
	err := d.pool.QueryRow(ctx, `
		SELECT
			d.id, d.serial_number, d.build_id, d.last_seen_at, d.created_at,
			COALESCE(c.battery_pct, 0) AS battery_pct
		FROM devices d
		LEFT JOIN LATERAL (
			SELECT battery_pct FROM checkins
			WHERE device_id = d.id
			ORDER BY created_at DESC
			LIMIT 1
		) c ON true
		WHERE d.serial_number = $1
	`, serial).Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct)
	if err != nil {
		return nil, fmt.Errorf("device not found: %w", err)
	}
	return &dev, nil
}

func (d *DB) GetCheckins(ctx context.Context, deviceID uuid.UUID, limit int) ([]Checkin, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, device_id, battery_pct, build_id, extra, created_at
		FROM checkins
		WHERE device_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var checkins []Checkin
	for rows.Next() {
		var c Checkin
		var extra []byte
		if err := rows.Scan(&c.ID, &c.DeviceID, &c.BatteryPct, &c.BuildID, &extra, &c.CreatedAt); err != nil {
			return nil, err
		}
		if len(extra) > 0 {
			c.Extra = json.RawMessage(extra)
		} else {
			c.Extra = json.RawMessage("{}")
		}
		checkins = append(checkins, c)
	}
	return checkins, rows.Err()
}

const migrationSQL = `
CREATE TABLE IF NOT EXISTS devices (
	id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	serial_number TEXT        NOT NULL UNIQUE,
	build_id      TEXT        NOT NULL DEFAULT '',
	last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS checkins (
	id          UUID     PRIMARY KEY DEFAULT gen_random_uuid(),
	device_id   UUID     NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	battery_pct SMALLINT NOT NULL,
	build_id    TEXT     NOT NULL DEFAULT '',
	extra       JSONB    NOT NULL DEFAULT '{}',
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_checkins_device_id  ON checkins(device_id);
CREATE INDEX IF NOT EXISTS idx_checkins_created_at ON checkins(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_devices_last_seen   ON devices(last_seen_at DESC);
`
