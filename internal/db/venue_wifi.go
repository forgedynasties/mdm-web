package db

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ── Venue Wi-Fi (W1, picked 17 Oct) ───────────────────────────────────────────
//
// The networks every device at a venue should have. The dashboard adds them before a
// router or password change; a reconciler (dashboard/venue_wifi.go) queues a wifi_config
// for each device that has not finished the network's current version. Secret is the
// password sealed by the dashboard — this package never sees it in the clear.

// VenueWifiNetwork is one network a venue's devices should have (or, once removed, forget).
type VenueWifiNetwork struct {
	ID           uuid.UUID
	RestaurantID uuid.UUID
	SSID         string
	Security     string // open | wpa2 | wpa3
	Hidden       bool
	Secret       []byte
	Version      int
	CreatedBy    string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	RemovedAt    *time.Time
}

const venueWifiCols = `id, restaurant_id, ssid, security, hidden, secret, version, created_by, created_at, updated_at, removed_at`

func scanVenueWifi(row pgx.Row) (VenueWifiNetwork, error) {
	var n VenueWifiNetwork
	err := row.Scan(&n.ID, &n.RestaurantID, &n.SSID, &n.Security, &n.Hidden, &n.Secret, &n.Version, &n.CreatedBy, &n.CreatedAt, &n.UpdatedAt, &n.RemovedAt)
	return n, err
}

// ListVenueWifi returns a venue's live networks, oldest first.
func (d *DB) ListVenueWifi(ctx context.Context, restaurantID uuid.UUID) ([]VenueWifiNetwork, error) {
	return d.queryVenueWifi(ctx, `SELECT `+venueWifiCols+` FROM venue_wifi_networks WHERE restaurant_id = $1 AND removed_at IS NULL ORDER BY created_at`, restaurantID)
}

// VenueWifiForReconcile returns every live network, and the ones removed within keep
// (their devices are still being told to forget them).
func (d *DB) VenueWifiForReconcile(ctx context.Context, keep time.Duration) ([]VenueWifiNetwork, error) {
	return d.queryVenueWifi(ctx, `SELECT `+venueWifiCols+` FROM venue_wifi_networks
		WHERE removed_at IS NULL OR removed_at > now() - make_interval(secs => $1) ORDER BY created_at`, keep.Seconds())
}

func (d *DB) queryVenueWifi(ctx context.Context, q string, args ...any) ([]VenueWifiNetwork, error) {
	rows, err := d.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VenueWifiNetwork
	for rows.Next() {
		n, err := scanVenueWifi(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// GetVenueWifi returns one network of a venue, live or removed.
func (d *DB) GetVenueWifi(ctx context.Context, restaurantID, id uuid.UUID) (VenueWifiNetwork, error) {
	return scanVenueWifi(d.pool.QueryRow(ctx, `SELECT `+venueWifiCols+` FROM venue_wifi_networks WHERE restaurant_id = $1 AND id = $2`, restaurantID, id))
}

// SaveVenueWifi adds a network to a venue, or — when the venue already has a live one with
// that name (any case) — replaces its security and password and moves it to a new
// version, so every device gets the change. Returns the saved row.
func (d *DB) SaveVenueWifi(ctx context.Context, n VenueWifiNetwork) (VenueWifiNetwork, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return n, err
	}
	defer tx.Rollback(ctx)
	cur, err := scanVenueWifi(tx.QueryRow(ctx, `SELECT `+venueWifiCols+` FROM venue_wifi_networks
		WHERE restaurant_id = $1 AND lower(ssid) = lower($2) AND removed_at IS NULL FOR UPDATE`, n.RestaurantID, n.SSID))
	var saved VenueWifiNetwork
	switch {
	case errors.Is(err, pgx.ErrNoRows):
		saved, err = scanVenueWifi(tx.QueryRow(ctx, `INSERT INTO venue_wifi_networks (restaurant_id, ssid, security, hidden, secret, created_by)
			VALUES ($1, $2, $3, $4, $5, $6) RETURNING `+venueWifiCols, n.RestaurantID, n.SSID, n.Security, n.Hidden, n.Secret, n.CreatedBy))
	case err == nil:
		saved, err = scanVenueWifi(tx.QueryRow(ctx, `UPDATE venue_wifi_networks SET ssid = $2, security = $3, hidden = $4, secret = $5,
			version = version + 1, updated_at = now() WHERE id = $1 RETURNING `+venueWifiCols, cur.ID, n.SSID, n.Security, n.Hidden, n.Secret))
	}
	if err != nil {
		return n, err
	}
	return saved, tx.Commit(ctx)
}

// RemoveVenueWifi marks a network removed; the reconciler then tells the venue's devices
// to forget it.
func (d *DB) RemoveVenueWifi(ctx context.Context, restaurantID, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `UPDATE venue_wifi_networks SET removed_at = now(), version = version + 1, updated_at = now()
		WHERE restaurant_id = $1 AND id = $2 AND removed_at IS NULL`, restaurantID, id)
	return err
}

// VenueWifiPush is the latest push of a network's version to one device, with the
// outcome its command reached ("" while nothing has answered).
type VenueWifiPush struct {
	NetworkID uuid.UUID
	DeviceID  uuid.UUID
	Version   int
	Action    string
	CommandID *uuid.UUID
	At        time.Time
	Status    string
}

// VenueWifiPushes returns every push of the given networks, with each command's status
// for that device.
func (d *DB) VenueWifiPushes(ctx context.Context, networkIDs []uuid.UUID) ([]VenueWifiPush, error) {
	if len(networkIDs) == 0 {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT p.network_id, p.device_id, p.version, p.action, p.command_id, p.at, COALESCE(cs.status, '')
		FROM venue_wifi_pushes p
		LEFT JOIN command_status cs ON cs.command_id = p.command_id AND cs.device_id = p.device_id
		WHERE p.network_id = ANY($1)`, networkIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []VenueWifiPush
	for rows.Next() {
		var p VenueWifiPush
		if err := rows.Scan(&p.NetworkID, &p.DeviceID, &p.Version, &p.Action, &p.CommandID, &p.At, &p.Status); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecordVenueWifiPush notes that a command now carries a network's version to a device. A
// retry of the same version replaces the earlier row.
func (d *DB) RecordVenueWifiPush(ctx context.Context, networkID, deviceID uuid.UUID, version int, action string, commandID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO venue_wifi_pushes (network_id, device_id, version, action, command_id) VALUES ($1, $2, $3, $4, $5)
		ON CONFLICT (network_id, device_id, version, action) DO UPDATE SET command_id = EXCLUDED.command_id, at = now()`,
		networkID, deviceID, version, action, commandID)
	return err
}
