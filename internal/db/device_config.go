package db

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ── Device Config / Kiosk ─────────────────────────────────────────────────────

// GetDeviceConfigUpdatedAtMap returns device_id -> updated_at for every device with a
// config row, so the Manage page can show "last changed" per device without an N+1.
// Not kiosk-specific — device_config's updated_at bumps on any field in that row
// (kiosk, WLC charging, offline-unlock rotation) — so it's a config-touched proxy,
// not a guarantee the kiosk fields themselves were what last changed.
func (d *DB) GetDeviceConfigUpdatedAtMap(ctx context.Context) (map[uuid.UUID]time.Time, error) {
	rows, err := d.pool.Query(ctx, `SELECT device_id, updated_at FROM device_config`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]time.Time)
	for rows.Next() {
		var id uuid.UUID
		var t time.Time
		if err := rows.Scan(&id, &t); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

func (d *DB) GetOrCreateDeviceConfig(ctx context.Context, deviceID uuid.UUID) (*DeviceConfig, error) {
	// Read first — this runs on every check-in and the row exists after the device's
	// first one, so the INSERT below (a wasted write attempt at 900 dev × every 60s)
	// only ever fires once per device.
	const sel = `SELECT device_id, kiosk_enabled, kiosk_package, kiosk_features,
		offline_exit_enabled, offline_exit_relock, wlc_charging_enabled, updated_at,
		kiosk_mode, kiosk_packages, kiosk_url, kiosk_url_allow
		FROM device_config WHERE device_id = $1`
	var cfg DeviceConfig
	scan := func(row pgx.Row) error {
		return row.Scan(&cfg.DeviceID, &cfg.KioskEnabled, &cfg.KioskPackage, &cfg.KioskFeatures,
			&cfg.OfflineExitEnabled, &cfg.OfflineExitRelock, &cfg.WlcChargingEnabled, &cfg.UpdatedAt,
			&cfg.KioskMode, &cfg.KioskPackages, &cfg.KioskURL, &cfg.KioskURLAllow)
	}
	err := scan(d.pool.QueryRow(ctx, sel, deviceID))
	if err == nil {
		return &cfg, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO device_config (device_id) VALUES ($1) ON CONFLICT (device_id) DO NOTHING`, deviceID); err != nil {
		return nil, err
	}
	if err := scan(d.pool.QueryRow(ctx, sel, deviceID)); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// PendingOfflineExitAt returns the offline kiosk-exit timestamp the device last
// reported (epoch seconds), or 0 when it has none. The value lives in latest_extra
// because a WS delta frame only carries it once — see processOfflineExit, which uses
// this to ack an exit whose original frame was lost.
func (d *DB) PendingOfflineExitAt(ctx context.Context, deviceID uuid.UUID) int64 {
	var at *int64
	err := d.pool.QueryRow(ctx,
		`SELECT (latest_extra->>'offline_exit_at')::bigint FROM devices WHERE id = $1`, deviceID).Scan(&at)
	if err != nil || at == nil {
		return 0
	}
	return *at
}

// RecordOfflineExit logs a device_events row when a device reports it was taken out of
// kiosk mode offline. Idempotent per (device, kind, occurred_at) so repeated reports
// (the client resends until acked) don't create duplicates.
func (d *DB) RecordOfflineExit(ctx context.Context, deviceID uuid.UUID, atEpoch int64) {
	_, _ = d.pool.Exec(ctx, `
		INSERT INTO device_events (device_id, kind, summary, occurred_at)
		SELECT $1, 'kiosk_exit_offline', 'Kiosk exited offline (exit PIN)', to_timestamp($2)
		WHERE NOT EXISTS (
			SELECT 1 FROM device_events
			WHERE device_id = $1 AND kind = 'kiosk_exit_offline' AND occurred_at = to_timestamp($2))`,
		deviceID, atEpoch)
}

func (d *DB) SetKioskConfig(ctx context.Context, deviceID uuid.UUID, enabled bool, pkg string, features int) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_config (device_id, kiosk_enabled, kiosk_package, kiosk_features, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (device_id) DO UPDATE
			SET kiosk_enabled  = EXCLUDED.kiosk_enabled,
			    kiosk_package  = EXCLUDED.kiosk_package,
			    kiosk_features = EXCLUDED.kiosk_features,
			    kiosk_exited_at = CASE WHEN EXCLUDED.kiosk_enabled THEN NULL ELSE device_config.kiosk_exited_at END,
			    updated_at     = NOW()
	`, deviceID, enabled, pkg, features)
	return err
}

// SetKioskModeConfig stores the extended kiosk fields alongside SetKioskConfig's
// classic enable/package pair: the mode ("app"|"multi"|"browser"), the extra
// packages allowed in multi-app lock-task, and the browser-kiosk start URL +
// allowed URL prefixes. Nil slices are stored as empty JSONB arrays so the
// config delivery never carries JSON null.
func (d *DB) SetKioskModeConfig(ctx context.Context, deviceID uuid.UUID, mode string, packages []string, url string, urlAllow []string) error {
	if packages == nil {
		packages = []string{}
	}
	if urlAllow == nil {
		urlAllow = []string{}
	}
	pkgs, _ := json.Marshal(packages)
	allow, _ := json.Marshal(urlAllow)
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_config (device_id, kiosk_mode, kiosk_packages, kiosk_url, kiosk_url_allow, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (device_id) DO UPDATE
			SET kiosk_mode      = EXCLUDED.kiosk_mode,
			    kiosk_packages  = EXCLUDED.kiosk_packages,
			    kiosk_url       = EXCLUDED.kiosk_url,
			    kiosk_url_allow = EXCLUDED.kiosk_url_allow,
			    updated_at      = NOW()
	`, deviceID, mode, pkgs, url, allow)
	return err
}

// SetWlcCharging sets the wireless-charging enable flag for a device.
func (d *DB) SetWlcCharging(ctx context.Context, deviceID uuid.UUID, enabled bool) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_config (device_id, wlc_charging_enabled, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (device_id) DO UPDATE
			SET wlc_charging_enabled = EXCLUDED.wlc_charging_enabled,
			    updated_at           = NOW()
	`, deviceID, enabled)
	return err
}

// CountUnlockedDevices returns the number of non-hidden devices with no kiosk lock
// applied (either no device_config row at all, or one with kiosk_enabled=false) —
// the Manage page's "Default" bucket.
func (d *DB) CountUnlockedDevices(ctx context.Context) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM devices d
		LEFT JOIN device_config dc ON dc.device_id = d.id
		WHERE NOT d.hidden AND COALESCE(dc.kiosk_enabled, false) = false
	`).Scan(&n)
	return n, err
}

func (d *DB) SetKioskConfigForDevices(ctx context.Context, deviceIDs []uuid.UUID, enabled bool, pkg string, features int) error {
	if len(deviceIDs) == 0 {
		return nil
	}
	// Bulk apply means the classic single-app lock, so it also resets kiosk_mode —
	// otherwise a device previously in browser/multi mode would keep interpreting
	// the new package through its stale mode.
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_config (device_id, kiosk_enabled, kiosk_package, kiosk_features, updated_at)
		SELECT d.id, $2, $3, $4, NOW()
		FROM devices d
		WHERE d.id = ANY($1)
		ON CONFLICT (device_id) DO UPDATE
			SET kiosk_enabled  = EXCLUDED.kiosk_enabled,
			    kiosk_package  = EXCLUDED.kiosk_package,
			    kiosk_features = EXCLUDED.kiosk_features,
			    kiosk_mode     = 'app',
			    updated_at     = NOW()
	`, deviceIDs, enabled, pkg, features)
	return err
}

// KioskPolicy is a named, standing kiosk-lock policy (see the kiosk_policies table
// comment). TargetID is nil for target_type "all" or "device".
type KioskPolicy struct {
	ID           uuid.UUID  `json:"id"`
	Name         string     `json:"name"`
	KioskPackage string     `json:"kiosk_package"`
	TargetType   string     `json:"target_type"`
	TargetID     *uuid.UUID `json:"target_id,omitempty"`
	TargetSerial string     `json:"target_serial,omitempty"`
	Priority     int        `json:"priority"` // lower wins where two rules cover one device
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

// ListKioskPolicies returns the kiosk rules in order: the first one covering a device
// is the one it follows.
func (d *DB) ListKioskPolicies(ctx context.Context) ([]KioskPolicy, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, name, kiosk_package, target_type, target_id, target_serial, priority, created_at, updated_at
		FROM kiosk_policies ORDER BY priority ASC, created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KioskPolicy
	for rows.Next() {
		var p KioskPolicy
		if err := rows.Scan(&p.ID, &p.Name, &p.KioskPackage, &p.TargetType, &p.TargetID, &p.TargetSerial, &p.Priority, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *DB) GetKioskPolicy(ctx context.Context, id uuid.UUID) (KioskPolicy, error) {
	var p KioskPolicy
	err := d.pool.QueryRow(ctx, `
		SELECT id, name, kiosk_package, target_type, target_id, target_serial, priority, created_at, updated_at
		FROM kiosk_policies WHERE id = $1
	`, id).Scan(&p.ID, &p.Name, &p.KioskPackage, &p.TargetType, &p.TargetID, &p.TargetSerial, &p.Priority, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

// CreateKioskPolicy adds a rule at the top of the order — a new rule is usually the
// more specific one and should win over the broad ones already there — except an
// all-devices rule, which goes to the bottom so it catches only what nothing else does.
func (d *DB) CreateKioskPolicy(ctx context.Context, name, pkg, targetType string, targetID *uuid.UUID, targetSerial string) (uuid.UUID, error) {
	var id uuid.UUID
	err := d.pool.QueryRow(ctx, `
		INSERT INTO kiosk_policies (name, kiosk_package, target_type, target_id, target_serial, priority)
		VALUES ($1, $2, $3, $4, $5,
		        CASE WHEN $3 = 'all' THEN COALESCE((SELECT MAX(priority) FROM kiosk_policies), -1) + 1
		             ELSE COALESCE((SELECT MIN(priority) FROM kiosk_policies), 1) - 1 END) RETURNING id
	`, name, pkg, targetType, targetID, targetSerial).Scan(&id)
	return id, err
}

func (d *DB) UpdateKioskPolicy(ctx context.Context, id uuid.UUID, name, pkg, targetType string, targetID *uuid.UUID, targetSerial string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE kiosk_policies SET name = $2, kiosk_package = $3, target_type = $4, target_id = $5, target_serial = $6,
		       updated_at = NOW()
		WHERE id = $1
	`, id, name, pkg, targetType, targetID, targetSerial)
	return err
}

func (d *DB) DeleteKioskPolicy(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM kiosk_policies WHERE id = $1`, id)
	return err
}
