package db

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// Apps a device should have (17 Nov 2026, from the firmware team's report FW-2026-000066:
// an install queued for an offline device expired after an hour). User: "app install should
// be more like a config than a command … so that it applies when device comes online".
// Installing is adding an app to the device: a row here, want 'present'. The minute job
// (dashboard/app_wants.go) sends the library's newest build when the device is online and
// lacks the app, and again if it disappears later; versions are not tracked after that, AIO
// apps update themselves. Remove in the MDM is want 'absent': uninstalled when online, and
// kept off. Follows (device_app_follows) were folded in here on the first start.
const appWantsSchema = `
CREATE TABLE IF NOT EXISTS device_app_wants (
	device_id  UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	package    TEXT NOT NULL,
	want       TEXT NOT NULL DEFAULT 'present', -- present | absent
	created_by TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (device_id, package)
);
-- Follows become added apps (user: "A. yes"): moved once, then the old table is empty.
WITH moved AS (DELETE FROM device_app_follows RETURNING device_id, package, created_by, created_at)
INSERT INTO device_app_wants (device_id, package, want, created_by, created_at)
SELECT device_id, package, 'present', created_by, created_at FROM moved
ON CONFLICT (device_id, package) DO NOTHING;
`

// Want values.
const (
	WantPresent = "present"
	WantAbsent  = "absent"
)

// AppWant is one app a device should have, or should not.
type AppWant struct {
	DeviceID  uuid.UUID
	Package   string
	Want      string
	CreatedBy string
	CreatedAt time.Time
}

// SetAppWant records that a device should have a package (present) or not (absent).
func (d *DB) SetAppWant(ctx context.Context, deviceID uuid.UUID, pkg, want, by string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_app_wants (device_id, package, want, created_by) VALUES ($1, $2, $3, $4)
		ON CONFLICT (device_id, package) DO UPDATE
		   SET want = EXCLUDED.want, created_by = EXCLUDED.created_by, created_at = NOW()
		 WHERE device_app_wants.want <> EXCLUDED.want`, deviceID, pkg, want, by)
	return err
}

// DeleteAppWant forgets a want (Cancel on a waiting add or removal). Reports whether one was.
func (d *DB) DeleteAppWant(ctx context.Context, deviceID uuid.UUID, pkg string) (bool, error) {
	tag, err := d.pool.Exec(ctx, `DELETE FROM device_app_wants WHERE device_id = $1 AND package = $2`, deviceID, pkg)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// AppWantsOf lists one device's wants, oldest first.
func (d *DB) AppWantsOf(ctx context.Context, deviceID uuid.UUID) ([]AppWant, error) {
	return d.appWants(ctx, `WHERE device_id = $1`, deviceID)
}

// AllAppWants lists every device's wants.
func (d *DB) AllAppWants(ctx context.Context) ([]AppWant, error) {
	return d.appWants(ctx, ``)
}

func (d *DB) appWants(ctx context.Context, where string, args ...any) ([]AppWant, error) {
	rows, err := d.pool.Query(ctx, `SELECT device_id, package, want, created_by, created_at FROM device_app_wants `+where+` ORDER BY created_at, package`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppWant
	for rows.Next() {
		var w AppWant
		if err := rows.Scan(&w.DeviceID, &w.Package, &w.Want, &w.CreatedBy, &w.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// DevicePackagesOf is, per device, which of these packages it last reported.
func (d *DB) DevicePackagesOf(ctx context.Context, ids []uuid.UUID, pkgs []string) (map[uuid.UUID]map[string]bool, error) {
	out := map[uuid.UUID]map[string]bool{}
	if len(ids) == 0 || len(pkgs) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `SELECT device_id, package_name FROM device_packages WHERE device_id = ANY($1) AND package_name = ANY($2)`, ids, pkgs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[string]bool{}
		}
		out[id][p] = true
	}
	return out, rows.Err()
}

// RecentUninstallsOf is, per device, the packages it was asked to uninstall lately: in the
// last 15 minutes whatever came of it, or three failures in a day. The job leaves those be.
func (d *DB) RecentUninstallsOf(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]map[string]bool, error) {
	out := map[uuid.UUID]map[string]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT ct.target_id, c.payload->>'package'
		  FROM commands c JOIN command_targets ct ON ct.command_id = c.id
		 WHERE c.type = 'uninstall' AND c.target_type = 'devices' AND ct.target_id = ANY($1)
		   AND c.created_at > now() - interval '15 minutes'
		UNION
		SELECT cs.device_id, c.payload->>'package'
		  FROM commands c JOIN command_status cs ON cs.command_id = c.id
		 WHERE c.type = 'uninstall' AND cs.device_id = ANY($1) AND cs.status = 'failed'
		   AND c.created_at > now() - interval '24 hours'
		 GROUP BY cs.device_id, c.payload->>'package'
		HAVING count(*) >= 3`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var p *string
		if err := rows.Scan(&id, &p); err != nil {
			return nil, err
		}
		if p == nil {
			continue
		}
		if out[id] == nil {
			out[id] = map[string]bool{}
		}
		out[id][*p] = true
	}
	return out, rows.Err()
}

// WantFailure is the newest failure of a want's command on one device: what the device
// said, and how many times it failed in the last day.
type WantFailure struct {
	Output string
	Count  int
	At     time.Time
}

// WantFailures returns, per package, the newest failed install (of any library build of
// it) or uninstall on this device in the last day.
func (d *DB) WantFailures(ctx context.Context, deviceID uuid.UUID) (map[string]WantFailure, error) {
	rows, err := d.pool.Query(ctx, `
		WITH f AS (
			SELECT CASE WHEN c.type = 'uninstall' THEN c.payload->>'package'
			            ELSE COALESCE(NULLIF(a.package_name, ''), ap.package_name, '') END AS pkg,
			       COALESCE(cr.output, '') AS output, cs.updated_at AS at
			  FROM commands c
			  JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = $1 AND cs.status = 'failed'
			  LEFT JOIN command_results cr ON cr.command_id = c.id AND cr.device_id = $1
			  LEFT JOIN apps a ON a.apk_url = c.apk_url AND c.apk_url <> ''
			  LEFT JOIN apk_packages ap ON ap.apk_url = c.apk_url AND c.apk_url <> ''
			 WHERE c.type IN ('install_apk', 'uninstall') AND c.created_at > now() - interval '24 hours'
		)
		SELECT DISTINCT ON (pkg) pkg, output, at, count(*) OVER (PARTITION BY pkg)
		  FROM f WHERE pkg <> '' ORDER BY pkg, at DESC`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]WantFailure{}
	for rows.Next() {
		var p string
		var f WantFailure
		if err := rows.Scan(&p, &f.Output, &f.At, &f.Count); err != nil {
			return nil, err
		}
		out[p] = f
	}
	return out, rows.Err()
}

// ForgetPackage drops one package from a device's reported list: a successful uninstall
// is proof enough (user: "if uninstall is successful we dont ever need a new list again").
func (d *DB) ForgetPackage(ctx context.Context, deviceID uuid.UUID, pkg string) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM device_packages WHERE device_id = $1 AND package_name = $2`, deviceID, pkg)
	return err
}

// UninstallPackageOf is the package a command uninstalls ("" when it is not an uninstall).
func (d *DB) UninstallPackageOf(ctx context.Context, cmdID uuid.UUID) (string, error) {
	var p *string
	err := d.pool.QueryRow(ctx, `SELECT payload->>'package' FROM commands WHERE id = $1 AND type = 'uninstall'`, cmdID).Scan(&p)
	if err == pgx.ErrNoRows || p == nil {
		return "", nil
	}
	return *p, err
}
