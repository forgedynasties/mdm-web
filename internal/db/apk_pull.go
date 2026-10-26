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
// are hosted separately) with a version on devices that can upload it (a Standard client
// listing apk_pull, a firmware client 1.21.0+ sending apk_pull_cmd) and were heard since
// `since`: devices with no venue first (an upload is large, and a lab unit's Wi-Fi is not
// a restaurant's), then the most recently heard.
func (d *DB) APKPullCandidates(ctx context.Context, since time.Time) ([]PullCandidate, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.id, d.serial_number, dp.package_name, dp.version_name
		FROM device_packages dp
		JOIN devices d ON d.id = dp.device_id
		WHERE NOT COALESCE(d.hidden, false)
		  AND d.last_seen_at > $1
		  AND ((d.agent_kind = 'dpc' AND d.capabilities ? 'apk_pull')
		    OR (d.agent_kind = 'firmware' AND d.latest_extra->'apk_pull_cmd' = 'true'::jsonb))
		  AND dp.package_name LIKE 'aio.app.%'
		  AND dp.package_name NOT LIKE 'aio.app.mdmclient%'
		  AND dp.version_name <> ''
		ORDER BY (d.restaurant_id IS NOT NULL), d.last_seen_at DESC`, since)
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
// the window: a pull is asked for once, then left alone a while. A failed one counts only
// for retryFailed after it failed, so a cut upload is asked for again soon.
func (d *DB) RecentAPKPulls(ctx context.Context, within, retryFailed time.Duration) (map[string]bool, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT COALESCE(c.payload->>'package', ''), COALESCE(c.payload->>'version', '')
		FROM commands c
		WHERE c.type = 'pull_apk' AND c.created_at > NOW() - $1::interval
		  AND NOT EXISTS (
			SELECT 1 FROM command_status cs
			WHERE cs.command_id = c.id AND cs.status = 'failed'
			  AND cs.updated_at < NOW() - $2::interval)`, within.String(), retryFailed.String())
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

// RefusedAPKPulls is the package@version pairs whose upload the library refused because
// the APK is signed with another key than the package's library versions (a developer
// build, say) within the window. Asking again would upload the same refused file, so the
// auto-pull skips that version and takes the newest one it can use.
func (d *DB) RefusedAPKPulls(ctx context.Context, within time.Duration) (map[string]bool, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT COALESCE(c.payload->>'package', ''), COALESCE(c.payload->>'version', '')
		FROM commands c
		JOIN command_results r ON r.command_id = c.id
		WHERE c.type = 'pull_apk' AND c.created_at > NOW() - $1::interval
		  AND r.output ILIKE '%signed with a different key%'`, within.String())
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
