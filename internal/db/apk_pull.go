package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// Save to library (26 Oct 2026): a Standard-client device uploads an installed app's
// APK into the app library (command pull_apk), by hand from the device page or by
// itself when the devices run a newer build of one of our apps than the library holds.

// CommandTargetsDevice reports whether the command names this device (directly or
// through its group).
func (d *DB) CommandTargetsDevice(ctx context.Context, commandID, deviceID uuid.UUID) (bool, error) {
	return d.commandTargetsDevice(ctx, commandID, deviceID)
}

// PullCandidate is one of our apps, at one version, on a device that can upload it.
type PullCandidate struct {
	DeviceID uuid.UUID
	Serial   string
	Package  string
	Version  string
}

// APKPullCandidates lists our own apps (aio.app.*, but not the MDM agent, whose builds
// are hosted separately) with a version on Standard-client devices that advertise
// apk_pull and were heard since `since`, freshest device first.
func (d *DB) APKPullCandidates(ctx context.Context, since time.Time) ([]PullCandidate, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.id, d.serial_number, dp.package_name, dp.version_name
		FROM device_packages dp
		JOIN devices d ON d.id = dp.device_id
		WHERE d.agent_kind = 'dpc'
		  AND NOT COALESCE(d.hidden, false)
		  AND d.last_seen_at > $1
		  AND d.capabilities ? 'apk_pull'
		  AND dp.package_name LIKE 'aio.app.%'
		  AND dp.package_name NOT LIKE 'aio.app.mdmclient%'
		  AND dp.version_name <> ''
		ORDER BY d.last_seen_at DESC`, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PullCandidate
	for rows.Next() {
		var c PullCandidate
		if err := rows.Scan(&c.DeviceID, &c.Serial, &c.Package, &c.Version); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// RecentAPKPulls is the package@version pairs a pull_apk command was sent for within
// the window, whatever came of it: a pull is asked for once, then left alone a while.
func (d *DB) RecentAPKPulls(ctx context.Context, within time.Duration) (map[string]bool, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT COALESCE(payload->>'package', ''), COALESCE(payload->>'version', '')
		FROM commands
		WHERE type = 'pull_apk' AND created_at > NOW() - $1::interval`, within.String())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var p, v string
		if err := rows.Scan(&p, &v); err != nil {
			return nil, err
		}
		out[p+"@"+v] = true
		out[p+"@"] = true // any pull of the package
	}
	return out, rows.Err()
}

// AppSigner is one library APK's stored signing certificate ("" until it is read).
type AppSigner struct {
	ID     uuid.UUID
	S3Key  string
	Signer string
}

// AppSigners lists the library's uploads of one package with their signers.
func (d *DB) AppSigners(ctx context.Context, pkg string) ([]AppSigner, error) {
	rows, err := d.pool.Query(ctx, `SELECT id, COALESCE(s3_key, ''), signer_sha256 FROM apps WHERE package_name = $1`, pkg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AppSigner
	for rows.Next() {
		var a AppSigner
		if err := rows.Scan(&a.ID, &a.S3Key, &a.Signer); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetAppSigner stores a library APK's signing certificate.
func (d *DB) SetAppSigner(ctx context.Context, id uuid.UUID, signer string) error {
	_, err := d.pool.Exec(ctx, `UPDATE apps SET signer_sha256 = $2 WHERE id = $1`, id, signer)
	return err
}
