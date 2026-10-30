package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// App follows (QA builds, option C picked 30 Oct 2026): a device follows a channel
// package (aio.app.kiosk.qa) and the library's newest upload of it installs there by
// itself (dashboard/app_follows.go). Key clash retries: a Lab device that refused a build
// for its signing key gets the app uninstalled and the build installed again.
const appFollowsSchema = `
CREATE TABLE IF NOT EXISTS device_app_follows (
	device_id  UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	package    TEXT NOT NULL,
	created_by TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (device_id, package)
);
CREATE TABLE IF NOT EXISTS key_clash_retries (
	command_id    UUID NOT NULL,
	device_id     UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	apk_url       TEXT NOT NULL,
	package       TEXT NOT NULL,
	stage         TEXT NOT NULL DEFAULT 'uninstall', -- uninstall | install | done | gave_up
	uninstall_cmd UUID,
	install_cmd   UUID,
	created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
	updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (command_id, device_id)
);
`

// AppFollow is one followed package on one device.
type AppFollow struct {
	DeviceID  uuid.UUID
	Package   string
	CreatedBy string
	CreatedAt time.Time
}

// DeviceFollows lists what one device follows, oldest first.
func (d *DB) DeviceFollows(ctx context.Context, deviceID uuid.UUID) ([]AppFollow, error) {
	rows, err := d.pool.Query(ctx, `SELECT device_id, package, created_by, created_at FROM device_app_follows
		WHERE device_id = $1 ORDER BY created_at, package`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppFollow
	for rows.Next() {
		var f AppFollow
		if err := rows.Scan(&f.DeviceID, &f.Package, &f.CreatedBy, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// AllFollows is every follow, per device.
func (d *DB) AllFollows(ctx context.Context) (map[uuid.UUID][]string, error) {
	rows, err := d.pool.Query(ctx, `SELECT device_id, package FROM device_app_follows ORDER BY created_at, package`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID][]string{}
	for rows.Next() {
		var id uuid.UUID
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, err
		}
		out[id] = append(out[id], p)
	}
	return out, rows.Err()
}

// FollowApp makes a device follow a package (idempotent).
func (d *DB) FollowApp(ctx context.Context, deviceID uuid.UUID, pkg, by string) error {
	_, err := d.pool.Exec(ctx, `INSERT INTO device_app_follows (device_id, package, created_by) VALUES ($1, $2, $3)
		ON CONFLICT (device_id, package) DO NOTHING`, deviceID, pkg, by)
	return err
}

// UnfollowApp stops a device following a package.
func (d *DB) UnfollowApp(ctx context.Context, deviceID uuid.UUID, pkg string) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM device_app_follows WHERE device_id = $1 AND package = $2`, deviceID, pkg)
	return err
}

// DevicePackageVersions is one device's installed packages with their versions.
func (d *DB) DevicePackageVersions(ctx context.Context, deviceID uuid.UUID) (map[string]string, error) {
	rows, err := d.pool.Query(ctx, `SELECT package_name, version_name FROM device_packages WHERE device_id = $1`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var p, v string
		if err := rows.Scan(&p, &v); err != nil {
			return nil, err
		}
		out[p] = v
	}
	return out, rows.Err()
}

// PackagesReported is, of these devices, the ones that have reported an app list.
func (d *DB) PackagesReported(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `SELECT DISTINCT device_id FROM device_packages WHERE device_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// InstallProgress is the newest install of a package on a device that is still on its
// way: its status and percent (-1 when unknown).
type InstallProgress struct {
	Package string
	Status  string
	Percent int
}

// PendingInstalls returns, for one device, the packages with an install on its way
// (last 2 hours), keyed by package (library or learned package of the APK).
func (d *DB) PendingInstalls(ctx context.Context, deviceID uuid.UUID) (map[string]InstallProgress, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT COALESCE(NULLIF(a.package_name, ''), ap.package_name, ''), COALESCE(cs.status, 'pending'), COALESCE(cs.progress, -1)
		FROM commands c
		JOIN command_targets ct ON ct.command_id = c.id AND ct.target_id = $1
		LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = $1
		LEFT JOIN apps a ON a.apk_url = c.apk_url
		LEFT JOIN apk_packages ap ON ap.apk_url = c.apk_url
		WHERE c.type = 'install_apk' AND c.target_type = 'devices'
		  AND c.created_at > now() - interval '2 hours'
		  AND (cs.status IS NULL OR cs.status IN ('pending', 'delivered', 'downloading', 'installing'))
		ORDER BY c.created_at`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]InstallProgress{}
	for rows.Next() {
		var p InstallProgress
		if err := rows.Scan(&p.Package, &p.Status, &p.Percent); err != nil {
			return nil, err
		}
		if p.Package != "" {
			out[p.Package] = p
		}
	}
	return out, rows.Err()
}

// KeyClash is a failed install whose result says the APK is signed with another key
// than the app on the device.
type KeyClash struct {
	CommandID uuid.UUID
	DeviceID  uuid.UUID
	ApkURL    string
	Package   string
	AtVenue   bool
	Output    string
	CreatedBy string
}

// KeyClashSQL matches a key-clash install result (the same words RefusedInstallsOf reads).
const KeyClashSQL = `(cr.output ILIKE '%INSTALL_FAILED_UPDATE_INCOMPATIBLE%' OR cr.output ILIKE '%signed with a different key%'
		       OR cr.output ILIKE '%signatures do not match%')`

// NewKeyClashes is the installs that failed for their signing key in the last 2 hours
// and are not yet in key_clash_retries (the retry itself is never retried again).
func (d *DB) NewKeyClashes(ctx context.Context) ([]KeyClash, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT c.id, cs.device_id, c.apk_url, COALESCE(NULLIF(a.package_name, ''), ap.package_name, ''),
		       dv.restaurant_id IS NOT NULL, cr.output, COALESCE(c.created_by, '')
		FROM commands c
		JOIN command_status cs ON cs.command_id = c.id AND cs.status = 'failed'
		JOIN command_results cr ON cr.command_id = c.id AND cr.device_id = cs.device_id
		JOIN devices dv ON dv.id = cs.device_id
		LEFT JOIN apps a ON a.apk_url = c.apk_url
		LEFT JOIN apk_packages ap ON ap.apk_url = c.apk_url
		WHERE c.type = 'install_apk' AND c.created_at > now() - interval '2 hours'
		  AND `+KeyClashSQL+`
		  AND NOT EXISTS (SELECT 1 FROM key_clash_retries k WHERE k.command_id = c.id AND k.device_id = cs.device_id)
		  AND NOT EXISTS (SELECT 1 FROM key_clash_retries k WHERE k.install_cmd = c.id)`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyClash
	for rows.Next() {
		var k KeyClash
		if err := rows.Scan(&k.CommandID, &k.DeviceID, &k.ApkURL, &k.Package, &k.AtVenue, &k.Output, &k.CreatedBy); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// StartKeyClashRetry records a key clash: stage is "uninstall" with the uninstall command
// queued, or "gave_up" when nothing is to be done (a device at a venue, no package known).
func (d *DB) StartKeyClashRetry(ctx context.Context, k KeyClash, stage string, uninstall *uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `INSERT INTO key_clash_retries (command_id, device_id, apk_url, package, stage, uninstall_cmd)
		VALUES ($1, $2, $3, $4, $5, $6) ON CONFLICT DO NOTHING`, k.CommandID, k.DeviceID, k.ApkURL, k.Package, stage, uninstall)
	return err
}

// KeyClashRetry is a retry waiting for its uninstall to finish.
type KeyClashRetry struct {
	CommandID       uuid.UUID
	DeviceID        uuid.UUID
	ApkURL          string
	Package         string
	UninstallStatus string // "" while it has not finished
	CreatedAt       time.Time
}

// WaitingKeyClashRetries is the retries whose uninstall was queued, with how it went.
func (d *DB) WaitingKeyClashRetries(ctx context.Context) ([]KeyClashRetry, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT k.command_id, k.device_id, k.apk_url, k.package,
		       COALESCE((SELECT cs.status FROM command_status cs WHERE cs.command_id = k.uninstall_cmd AND cs.device_id = k.device_id
		                 AND cs.status IN ('completed', 'installed', 'failed', 'expired', 'cancelled', 'canceled')), ''), k.created_at
		FROM key_clash_retries k WHERE k.stage = 'uninstall'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KeyClashRetry
	for rows.Next() {
		var k KeyClashRetry
		if err := rows.Scan(&k.CommandID, &k.DeviceID, &k.ApkURL, &k.Package, &k.UninstallStatus, &k.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, k)
	}
	return out, rows.Err()
}

// SetKeyClashStage moves a retry on, recording the reinstall command when there is one.
func (d *DB) SetKeyClashStage(ctx context.Context, commandID, deviceID uuid.UUID, stage string, install *uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `UPDATE key_clash_retries SET stage = $3, install_cmd = COALESCE($4, install_cmd), updated_at = now()
		WHERE command_id = $1 AND device_id = $2`, commandID, deviceID, stage, install)
	return err
}
