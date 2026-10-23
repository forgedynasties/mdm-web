package db

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	"mdm/internal/aiobackend"
)

// AIO backend copy (23 Oct 2026). The sync job replaces an environment's restaurants and
// devices wholesale; venues link to one restaurant in one environment (P1, confirmed by a
// person).

// BackendSyncState is one environment's last sync.
type BackendSyncState struct {
	Env         string
	SyncedAt    *time.Time
	AttemptedAt *time.Time
	Error       string
	Restaurants int
	Devices     int
	Requested   bool
}

// BackendSyncStates returns the sync state of every environment that has one.
func (d *DB) BackendSyncStates(ctx context.Context) (map[string]BackendSyncState, error) {
	rows, err := d.pool.Query(ctx, `SELECT env, synced_at, attempted_at, error, restaurants, devices, requested FROM backend_sync`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]BackendSyncState{}
	for rows.Next() {
		var s BackendSyncState
		if err := rows.Scan(&s.Env, &s.SyncedAt, &s.AttemptedAt, &s.Error, &s.Restaurants, &s.Devices, &s.Requested); err != nil {
			return nil, err
		}
		out[s.Env] = s
	}
	return out, rows.Err()
}

// RequestBackendSync asks the sync job to run for every environment on its next tick.
func (d *DB) RequestBackendSync(ctx context.Context) error {
	_, err := d.pool.Exec(ctx, `INSERT INTO backend_sync (env, requested) SELECT e, TRUE FROM unnest($1::text[]) e
		ON CONFLICT (env) DO UPDATE SET requested = TRUE`, []string{string(aiobackend.Prod), string(aiobackend.Stage)})
	return err
}

// NoteBackendSyncFailed records a failed attempt (the copy is kept as it was).
func (d *DB) NoteBackendSyncFailed(ctx context.Context, env string, msg string) error {
	_, err := d.pool.Exec(ctx, `INSERT INTO backend_sync (env, attempted_at, error) VALUES ($1, NOW(), $2)
		ON CONFLICT (env) DO UPDATE SET attempted_at = NOW(), error = $2, requested = FALSE`, env, msg)
	return err
}

// ReplaceBackendCopy swaps env's restaurants and devices for these, in one transaction.
func (d *DB) ReplaceBackendCopy(ctx context.Context, env string, rs []aiobackend.Restaurant, ds []aiobackend.Device) error {
	if len(rs) == 0 {
		return errors.New("backend sync returned no restaurants; keeping the old copy")
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM backend_restaurants WHERE env = $1`, env); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM backend_devices WHERE env = $1`, env); err != nil {
		return err
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"backend_restaurants"},
		[]string{"env", "id", "name", "business_id", "business_name", "address", "city", "state", "zip", "latitude", "longitude", "timezone", "phone", "status"},
		pgx.CopyFromSlice(len(rs), func(i int) ([]any, error) {
			r := rs[i]
			return []any{env, r.ID, r.Name, r.BusinessID, r.BusinessName, r.Address, r.City, r.State, r.Zip, r.Latitude, r.Longitude, r.Timezone, r.Phone, r.Status}, nil
		})); err != nil {
		return err
	}
	if _, err := tx.CopyFrom(ctx, pgx.Identifier{"backend_devices"},
		[]string{"env", "id", "restaurant_id", "device_type", "serial", "ip", "onboarded", "revoked", "updated"},
		pgx.CopyFromSlice(len(ds), func(i int) ([]any, error) {
			v := ds[i]
			return []any{env, v.ID, v.RestaurantID, v.Type, v.Serial, v.IP, v.Onboarded, v.Revoked, v.UpdatedAt}, nil
		})); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO backend_sync (env, synced_at, attempted_at, error, restaurants, devices, requested)
		VALUES ($1, NOW(), NOW(), '', $2, $3, FALSE)
		ON CONFLICT (env) DO UPDATE SET synced_at = NOW(), attempted_at = NOW(), error = '', restaurants = $2, devices = $3, requested = FALSE`,
		env, len(rs), len(ds)); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// BackendRestaurant is a copied restaurant row.
type BackendRestaurant struct {
	Env string
	aiobackend.Restaurant
}

// ListBackendRestaurants returns the copied restaurants of every environment.
func (d *DB) ListBackendRestaurants(ctx context.Context) ([]BackendRestaurant, error) {
	rows, err := d.pool.Query(ctx, `SELECT env, id, name, business_id, business_name, address, city, state, zip,
		latitude, longitude, timezone, phone, status FROM backend_restaurants ORDER BY env, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BackendRestaurant
	for rows.Next() {
		var r BackendRestaurant
		if err := rows.Scan(&r.Env, &r.ID, &r.Name, &r.BusinessID, &r.BusinessName, &r.Address, &r.City, &r.State, &r.Zip,
			&r.Latitude, &r.Longitude, &r.Timezone, &r.Phone, &r.Status); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// BackendDevice is a copied device row.
type BackendDevice struct {
	Env string
	aiobackend.Device
}

// ListBackendDevices returns the copied devices of every environment (revoked ones left
// out).
func (d *DB) ListBackendDevices(ctx context.Context) ([]BackendDevice, error) {
	rows, err := d.pool.Query(ctx, `SELECT env, id, restaurant_id, device_type, serial, ip, onboarded, revoked, updated
		FROM backend_devices WHERE NOT revoked ORDER BY env, restaurant_id, device_type, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BackendDevice
	for rows.Next() {
		var v BackendDevice
		if err := rows.Scan(&v.Env, &v.ID, &v.RestaurantID, &v.Type, &v.Serial, &v.IP, &v.Onboarded, &v.Revoked, &v.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// VenueBackendLink is a venue's confirmed backend restaurant.
type VenueBackendLink struct {
	Env          string
	RestaurantID int64
	LinkedBy     string
	LinkedAt     *time.Time
}

// VenueBackendLinks maps every linked venue to its link.
func (d *DB) VenueBackendLinks(ctx context.Context) (map[uuid.UUID]VenueBackendLink, error) {
	rows, err := d.pool.Query(ctx, `SELECT id, backend_env, backend_restaurant_id, backend_linked_by, backend_linked_at
		FROM restaurants WHERE backend_env <> '' AND backend_restaurant_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]VenueBackendLink{}
	for rows.Next() {
		var id uuid.UUID
		var l VenueBackendLink
		if err := rows.Scan(&id, &l.Env, &l.RestaurantID, &l.LinkedBy, &l.LinkedAt); err != nil {
			return nil, err
		}
		out[id] = l
	}
	return out, rows.Err()
}

// SetVenueBackendLink links a venue (env "" unlinks it).
func (d *DB) SetVenueBackendLink(ctx context.Context, venue uuid.UUID, env string, restaurantID int64, by string) error {
	if env == "" {
		_, err := d.pool.Exec(ctx, `UPDATE restaurants SET backend_env = '', backend_restaurant_id = NULL,
			backend_linked_by = $2, backend_linked_at = NOW() WHERE id = $1`, venue, by)
		return err
	}
	_, err := d.pool.Exec(ctx, `UPDATE restaurants SET backend_env = $2, backend_restaurant_id = $3,
		backend_linked_by = $4, backend_linked_at = NOW() WHERE id = $1`, venue, env, restaurantID, by)
	return err
}
