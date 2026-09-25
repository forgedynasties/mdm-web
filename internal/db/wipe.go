package db

import "context"

// SetOTAPackageWipe records whether installing the package factory-resets the device,
// and that its zip has been read (wipe_checked), so the startup backfill skips it.
func (d *DB) SetOTAPackageWipe(ctx context.Context, id int, wipe bool) error {
	_, err := d.pool.Exec(ctx, `UPDATE ota_packages SET wipe = $2, wipe_checked = true WHERE id = $1`, id, wipe)
	return err
}

// PackageToCheck is an active package whose zip has not been read for the wipe flag yet.
type PackageToCheck struct {
	ID  int
	URL string
}

// PackagesNeedingWipeCheck lists active packages added before the wipe flag existed (or
// whose zip could not be read at the time), for the startup backfill.
func (d *DB) PackagesNeedingWipeCheck(ctx context.Context, limit int) ([]PackageToCheck, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, update_url FROM ota_packages
		WHERE status = 'active' AND NOT wipe_checked AND update_url <> ''
		ORDER BY created_at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PackageToCheck
	for rows.Next() {
		var p PackageToCheck
		if err := rows.Scan(&p.ID, &p.URL); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ReleaseHasWipeFull reports whether the release's active full image wipes the device —
// the case where a deployment must ask before using it as the fallback.
func (d *DB) ReleaseHasWipeFull(ctx context.Context, releaseID int) (bool, error) {
	var ok bool
	err := d.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM ota_packages
		               WHERE release_id = $1 AND status = 'active' AND type = 'full' AND wipe)`, releaseID).Scan(&ok)
	return ok, err
}

// SetUpdateAllowWipe lets (or stops) a deployment hand its wipe full image to devices
// that have no incremental for their build.
func (d *DB) SetUpdateAllowWipe(ctx context.Context, updateID int, allow bool) error {
	_, err := d.pool.Exec(ctx, `UPDATE updates SET allow_wipe = $2 WHERE id = $1`, updateID, allow)
	return err
}

// UpdateAllowsWipe is SetUpdateAllowWipe's read side, for adding targets to a deployment
// later under the rule it was created with.
func (d *DB) UpdateAllowsWipe(ctx context.Context, updateID int) (bool, error) {
	var ok bool
	err := d.pool.QueryRow(ctx, `SELECT allow_wipe FROM updates WHERE id = $1`, updateID).Scan(&ok)
	return ok, err
}
