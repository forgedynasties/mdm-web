package db

import (
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ── Updates (Deployments) ─────────────────────────────────────────────────────

func (d *DB) CreateUpdate(ctx context.Context, otaPackageID int, rebootBehavior string, scheduledTime *time.Time, createdBy string) (*Update, error) {
	var u Update
	err := d.pool.QueryRow(ctx, `
		INSERT INTO updates (ota_package_id, release_id, reboot_behavior, scheduled_time, status, created_by)
		SELECT $1, p.release_id, $2, $3, 'pending', $4 FROM ota_packages p WHERE p.id = $1
		RETURNING id, COALESCE(ota_package_id, 0), release_id, reboot_behavior, scheduled_time, status, created_at, created_by
	`, otaPackageID, rebootBehavior, scheduledTime, createdBy).
		Scan(&u.ID, &u.OtaPackageID, &u.ReleaseID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt, &u.CreatedBy)
	return &u, err
}

// CreateReleaseUpdate creates a deployment for a whole release. ota_package_id is
// set to the release's full package when present (representative / back-compat);
// the per-device artifact is chosen at resolve time by ResolveUpdateForDevice.
func (d *DB) CreateReleaseUpdate(ctx context.Context, releaseID int, rebootBehavior string, scheduledTime *time.Time, createdBy string) (*Update, error) {
	var u Update
	err := d.pool.QueryRow(ctx, `
		INSERT INTO updates (release_id, ota_package_id, reboot_behavior, scheduled_time, status, created_by)
		VALUES ($1,
		        (SELECT id FROM ota_packages WHERE release_id = $1 AND type = 'full' ORDER BY created_at DESC LIMIT 1),
		        $2, $3, 'pending', $4)
		RETURNING id, COALESCE(ota_package_id, 0), release_id, reboot_behavior, scheduled_time, status, created_at, created_by
	`, releaseID, rebootBehavior, scheduledTime, createdBy).
		Scan(&u.ID, &u.OtaPackageID, &u.ReleaseID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt, &u.CreatedBy)
	return &u, err
}

func (d *DB) ListDeploymentsByPackage(ctx context.Context, pkgID int) ([]Update, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT u.id, u.ota_package_id, u.reboot_behavior, u.scheduled_time, u.status, u.created_at,
		       COUNT(ud.device_id) AS device_total,
		       COUNT(CASE WHEN ud.status = 'installed' THEN 1 END) AS device_installed
		FROM updates u
		LEFT JOIN update_devices ud ON ud.update_id = u.id
		WHERE u.ota_package_id = $1
		GROUP BY u.id
		ORDER BY u.created_at DESC
	`, pkgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Update
	for rows.Next() {
		var u Update
		if err := rows.Scan(&u.ID, &u.OtaPackageID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt,
			&u.DeviceTotal, &u.DeviceInstalled); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListDeploymentsByRelease returns deployments for a release with device counts.
func (d *DB) ListDeploymentsByRelease(ctx context.Context, releaseID int) ([]Update, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT u.id, COALESCE(u.ota_package_id, 0), u.release_id, u.reboot_behavior, u.scheduled_time, u.status, u.created_at,
		       COUNT(ud.device_id) AS device_total,
		       COUNT(CASE WHEN ud.status = 'installed' THEN 1 END) AS device_installed
		FROM updates u
		LEFT JOIN update_devices ud ON ud.update_id = u.id
		WHERE u.release_id = $1
		GROUP BY u.id
		ORDER BY u.created_at DESC
	`, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Update
	for rows.Next() {
		var u Update
		if err := rows.Scan(&u.ID, &u.OtaPackageID, &u.ReleaseID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt,
			&u.DeviceTotal, &u.DeviceInstalled); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListDeployments returns every deployment (newest first) with its release version
// and device counts — backs the global Updates hub.
func (d *DB) ListDeployments(ctx context.Context) ([]Update, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT u.id, COALESCE(u.ota_package_id, 0), COALESCE(u.release_id, 0), u.reboot_behavior,
		       u.scheduled_time, u.status, u.created_at, u.created_by, COALESCE(rel.version, ''), COALESCE(rel.product, 't7'),
		       COALESCE(rel.is_dev, false),
		       COUNT(ud.device_id) AS device_total,
		       COUNT(CASE WHEN ud.status = 'installed' THEN 1 END) AS device_installed,
		       COUNT(CASE WHEN ud.status = 'downloading' THEN 1 END) AS device_downloading,
		       COUNT(CASE WHEN ud.status IN ('installing', 'verifying', 'finalizing') THEN 1 END) AS device_installing,
		       COUNT(CASE WHEN ud.status IN ('awaiting_reboot', 'reboot_sent') THEN 1 END) AS device_awaiting,
		       COUNT(CASE WHEN ud.status = 'failed' THEN 1 END) AS device_failed,
		       -- rebooted to apply it 15+ minutes ago and not confirmed on the new build
		       COUNT(CASE WHEN ud.status = 'reboot_sent' AND COALESCE(ud.reboot_sent_at, ud.updated_at) < NOW() - INTERVAL '15 minutes' THEN 1 END) AS device_not_back
		FROM updates u
		LEFT JOIN releases rel ON rel.id = u.release_id
		LEFT JOIN update_devices ud ON ud.update_id = u.id
		GROUP BY u.id, rel.version, rel.product, rel.is_dev
		ORDER BY u.created_at DESC
		LIMIT 200
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Update
	for rows.Next() {
		var u Update
		var version, product string
		if err := rows.Scan(&u.ID, &u.OtaPackageID, &u.ReleaseID, &u.RebootBehavior, &u.ScheduledTime,
			&u.Status, &u.CreatedAt, &u.CreatedBy, &version, &product, &u.ReleaseIsDev, &u.DeviceTotal, &u.DeviceInstalled,
			&u.DeviceDownloading, &u.DeviceInstalling, &u.DeviceAwaiting, &u.DeviceFailed, &u.DeviceNotBack); err != nil {
			return nil, err
		}
		u.Product = product
		u.Release = &Release{ID: u.ReleaseID, Version: version, Product: product, IsDev: u.ReleaseIsDev}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListDeployableReleases returns published releases that have at least one active OTA
// package — the choices offered in the Updates-hub deploy composer.
func (d *DB) ListDeployableReleases(ctx context.Context) ([]Release, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT r.id, r.version, r.name, r.is_dev, r.created_at,
		       bool_or(p.type = 'full') AS has_full,
		       COUNT(*) FILTER (WHERE p.type = 'incremental')::int AS incrementals
		FROM releases r
		JOIN ota_packages p ON p.release_id = r.id AND p.status = 'active'
		WHERE r.status = 'published' AND NOT r.hidden
		GROUP BY r.id, r.version, r.name, r.is_dev, r.created_at
		ORDER BY r.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		var rel Release
		if err := rows.Scan(&rel.ID, &rel.Version, &rel.Name, &rel.IsDev, &rel.CreatedAt, &rel.HasFull, &rel.Incrementals); err != nil {
			return nil, err
		}
		out = append(out, rel)
	}
	return out, rows.Err()
}

func (d *DB) ListUpdates(ctx context.Context) ([]Update, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT u.id, u.ota_package_id, u.reboot_behavior, u.scheduled_time, u.status, u.created_at,
		       p.id, p.type, p.target_build_id, p.source_build_id, p.release_date, p.update_url, p.changelog, p.status, p.created_at
		FROM updates u
		JOIN ota_packages p ON p.id = u.ota_package_id
		ORDER BY u.created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Update
	for rows.Next() {
		var u Update
		var p OTAPackage
		if err := rows.Scan(&u.ID, &u.OtaPackageID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt,
			&p.ID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.Changelog, &p.Status, &p.CreatedAt); err != nil {
			return nil, err
		}
		u.OtaPackage = &p
		out = append(out, u)
	}
	return out, rows.Err()
}

func (d *DB) GetUpdate(ctx context.Context, id int) (*Update, error) {
	var u Update
	var rel Release
	err := d.pool.QueryRow(ctx, `
		SELECT u.id, COALESCE(u.ota_package_id, 0), u.release_id, u.reboot_behavior, u.scheduled_time, u.status, u.created_at, u.created_by,
		       rel.id, rel.version, rel.name, rel.changelog, rel.status, rel.created_at, rel.published_at
		FROM updates u
		JOIN releases rel ON rel.id = u.release_id
		WHERE u.id = $1
	`, id).Scan(&u.ID, &u.OtaPackageID, &u.ReleaseID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt, &u.CreatedBy,
		&rel.ID, &rel.Version, &rel.Name, &rel.Changelog, &rel.Status, &rel.CreatedAt, &rel.PublishedAt)
	if err != nil {
		return nil, err
	}
	u.Release = &rel
	return &u, nil
}

func (d *DB) DeleteUpdate(ctx context.Context, id int) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM updates WHERE id = $1`, id)
	return err
}

func (d *DB) UpdateDeploymentRebootSettings(ctx context.Context, id int, rebootBehavior string, scheduledTime *time.Time) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE updates
		SET reboot_behavior = $2, scheduled_time = $3
		WHERE id = $1
	`, id, rebootBehavior, scheduledTime)
	return err
}

// SendUpdateToDevices adds devices as targets of an update. Skips devices that
// already have an active (non-complete) update. Sets the update status to "active".
func (d *DB) SendUpdateToDevices(ctx context.Context, updateID int, deviceIDs []uuid.UUID, forceFull bool) error {
	// One transaction so target inserts and the activation commit together — a
	// mid-loop failure previously left the deployment armed with missing targets,
	// and the per-row insert error was silently discarded.
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, did := range deviceIDs {
		// force_full pins these rows to the full image so ResolveUpdateForDevice never
		// offers a matching incremental — the operator explicitly chose full delivery.
		if _, err := tx.Exec(ctx, `
			INSERT INTO update_devices (update_id, device_id, status, force_full, source_build_id)
			VALUES ($1, $2, 'pending', $3, COALESCE((SELECT build_id FROM devices WHERE id = $2), ''))
			ON CONFLICT DO NOTHING
		`, updateID, did, forceFull); err != nil {
			return err
		}
		// A device only reaches here because it has no other active/incomplete
		// update (callers only pass already-eligible devices) — so the sole thing
		// that could still block this fresh command is a *stale* failed OTA from
		// an earlier, now-complete deployment: HasPendingOTACommand treats a
		// failure as "still pending" for an hour specifically to stop automatic
		// checkin-driven re-sends from hammering a device. Creating a brand-new
		// deployment is a deliberate, explicit retry, so clear that guard here —
		// otherwise the new row sits at "pending" with nothing ever actually sent
		// until the hour lapses, on both the immediate push and the later
		// checkin-driven resolve path.
		if _, err := tx.Exec(ctx, `
			INSERT INTO command_status (command_id, device_id, status, updated_at)
			SELECT c.id, ct.target_id, 'failed', NOW() - INTERVAL '2 hours'
			FROM commands c
			JOIN command_targets ct ON ct.command_id = c.id AND ct.target_id = $1
			LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = $1
			WHERE c.type = 'ota'
			AND (
				cs.status IS NULL
				OR cs.status NOT IN ('installed', 'failed', 'completed')
				OR (cs.status = 'failed' AND cs.updated_at > NOW() - INTERVAL '2 hours')
			)
			ON CONFLICT (command_id, device_id) DO UPDATE
				SET status = EXCLUDED.status, updated_at = EXCLUDED.updated_at
		`, did); err != nil {
			return err
		}
	}
	// Activate the deployment. Reactivate a 'complete' one too: adding targets to
	// a finished deployment must re-arm it, or the new pending rows are stranded
	// (ResolveUpdateForDevice only serves status='active').
	if _, err := tx.Exec(ctx, `UPDATE updates SET status = 'active' WHERE id = $1 AND status IN ('pending', 'complete')`, updateID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// DeviceHasActiveUpdate returns true if the device is a target of any non-complete update.
func (d *DB) DeviceHasActiveUpdate(ctx context.Context, deviceID uuid.UUID) (bool, error) {
	var exists bool
	err := d.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM update_devices ud
			JOIN updates u ON u.id = ud.update_id
			WHERE ud.device_id = $1 AND u.status = 'active'
			  -- in flight only: a failed / cancelled row on another deployment must
			  -- not block a new push to this device
			  AND ud.status NOT IN ('installed', 'failed', 'canceled')
		)
	`, deviceID).Scan(&exists)
	return exists, err
}

// ResolveUpdateForDevice returns the active Update (with joined OTAPackage) for a device
// from the update_devices table. Returns nil if no active update is assigned.
func (d *DB) ResolveUpdateForDevice(ctx context.Context, deviceID uuid.UUID) (*Update, error) {
	var u Update
	var p OTAPackage
	// Resolve via the deployment's release and pick the per-device artifact: the
	// incremental whose source build matches the device's current build if one
	// exists, otherwise the full image. Only published releases and active
	// packages are eligible.
	err := d.pool.QueryRow(ctx, `
		SELECT u.id, COALESCE(u.ota_package_id, 0), u.release_id, u.reboot_behavior, u.scheduled_time, u.status, u.created_at, ud.status, ud.reboot_sent_at,
		       p.id, p.release_id, p.type, p.target_build_id, p.source_build_id, p.release_date, p.update_url, p.changelog, p.status, p.created_at
		FROM update_devices ud
		JOIN updates u ON u.id = ud.update_id
		JOIN releases rel ON rel.id = u.release_id
		JOIN devices d ON d.id = ud.device_id
		JOIN LATERAL (
			SELECT pk.* FROM ota_packages pk
			WHERE pk.release_id = u.release_id AND pk.status = 'active'
			  AND ((pk.type = 'full' AND (NOT pk.wipe OR u.allow_wipe))
			       OR (pk.type = 'incremental' AND NOT ud.force_full AND pk.source_build_id = d.build_id))
			ORDER BY (pk.type = 'incremental' AND NOT ud.force_full AND pk.source_build_id = d.build_id) DESC, pk.created_at DESC
			LIMIT 1
		) p ON true
		WHERE ud.device_id = $1 AND u.status = 'active' AND ud.status != 'installed' AND rel.status = 'published'
		  -- Not before the update it waits for (after_update_id) is installed on it.
		  AND NOT EXISTS (
			SELECT 1 FROM update_devices pre JOIN updates pu ON pu.id = pre.update_id
			WHERE pre.update_id = ud.after_update_id AND pre.device_id = ud.device_id
			  AND pu.status = 'active' AND pre.status <> 'installed')
		  -- A failed row (update_engine error, failed slot switch, operator cancel) is
		  -- terminal: re-offering it on the next check-in undid every Cancel and made
		  -- slot-switch failures loop. Retry on the deployment page resets the row to
		  -- pending on purpose (and serves the full image after an incremental failed).
		  AND ud.status NOT IN ('failed', 'canceled')
		  -- Per-product safety gate: never resolve a release whose product differs from the
		  -- device's own. A legacy/empty device product counts as t7 (the pre-product fleet).
		  AND rel.product = CASE WHEN d.product = '' THEN 't7' ELSE d.product END
		  -- Monotonicity gate: never OTA a release that is older than (or the same as) the
		  -- release the device is already running. Order within a product is (created_at, id),
		  -- both creation-monotonic. If the device's current build isn't a managed release we
		  -- can't prove a regression, so we allow it (NOT EXISTS is true).
		  AND NOT EXISTS (
		      SELECT 1 FROM releases cur
		      WHERE cur.product = rel.product
		        AND cur.version = d.build_id
		        AND (cur.created_at, cur.id) >= (rel.created_at, rel.id)
		  )
		ORDER BY u.created_at DESC
		LIMIT 1
	`, deviceID).Scan(&u.ID, &u.OtaPackageID, &u.ReleaseID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt, &u.DeviceStatus, &u.DeviceRebootSentAt,
		&p.ID, &p.ReleaseID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.Changelog, &p.Status, &p.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	u.OtaPackage = &p
	return &u, nil
}

// DueReboot is a device whose scheduled-reboot deployment time has arrived.
type DueReboot struct {
	UpdateID int
	DeviceID uuid.UUID
}

// ListDueScheduledReboots returns devices that have installed an update under a
// "scheduled" reboot policy and whose scheduled time has passed.
func (d *DB) ListDueScheduledReboots(ctx context.Context) ([]DueReboot, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT ud.update_id, ud.device_id
		FROM update_devices ud
		JOIN updates u ON u.id = ud.update_id
		WHERE u.reboot_behavior = 'scheduled'
		  AND u.scheduled_time IS NOT NULL
		  AND u.scheduled_time <= NOW()
		  AND u.status = 'active'
		  AND ud.status = 'awaiting_reboot'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueReboot
	for rows.Next() {
		var dr DueReboot
		if err := rows.Scan(&dr.UpdateID, &dr.DeviceID); err != nil {
			return nil, err
		}
		out = append(out, dr)
	}
	return out, rows.Err()
}

// InProgressTarget is one deployment target that is mid-flight (downloading or
// installing) — the rows whose live percent the Updates hub needs to weight a
// rollout's progress bar, since that percent lives in memory, not in the table.
type InProgressTarget struct {
	UpdateID int
	DeviceID uuid.UUID
	Status   string
}

// ListInProgressTargets returns every mid-flight target across all deployments, so
// the hub can fold each device's live download/install percent into its rollout's
// score in one query instead of one per rollout.
func (d *DB) ListInProgressTargets(ctx context.Context) ([]InProgressTarget, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT update_id, device_id, status FROM update_devices
		WHERE status IN ('downloading', 'installing', 'verifying', 'finalizing')
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []InProgressTarget
	for rows.Next() {
		var t InProgressTarget
		if err := rows.Scan(&t.UpdateID, &t.DeviceID, &t.Status); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListAwaitingRebootForUpdate returns the device IDs of an update's targets that have
// installed to the inactive slot and are waiting for a reboot (status 'awaiting_reboot').
// Used by the deployment page's "reboot all installed" bulk action.
func (d *DB) ListAwaitingRebootForUpdate(ctx context.Context, updateID int) ([]uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT device_id FROM update_devices
		WHERE update_id = $1 AND status = 'awaiting_reboot'
	`, updateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ListStaleRebootSent returns devices stuck at 'reboot_sent' on an active
// deployment for longer than staleMinutes — the reboot command was likely lost or
// declined, so it must be re-issued. Without this a device that took the OTA but
// never rebooted sits in limbo forever, keeping the deployment 'active'.
func (d *DB) ListStaleRebootSent(ctx context.Context, staleMinutes int) ([]DueReboot, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT ud.update_id, ud.device_id
		FROM update_devices ud
		JOIN updates u ON u.id = ud.update_id
		WHERE u.status = 'active'
		  AND ud.status = 'reboot_sent'
		  AND ud.updated_at < NOW() - ($1 * INTERVAL '1 minute')
	`, staleMinutes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueReboot
	for rows.Next() {
		var dr DueReboot
		if err := rows.Scan(&dr.UpdateID, &dr.DeviceID); err != nil {
			return nil, err
		}
		out = append(out, dr)
	}
	return out, rows.Err()
}

// SetUpdateDeviceStatus updates the status of a device within an update. It
// stamps updated_at and clears any prior error_code, so a device that moves on
// from a failure (e.g. on retry) doesn't keep showing a stale reason.
func (d *DB) SetUpdateDeviceStatus(ctx context.Context, updateID int, deviceID uuid.UUID, status string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE update_devices SET status = $3, error_code = '', updated_at = NOW(),
			started_at   = CASE WHEN $3 = 'downloading' AND started_at   IS NULL THEN NOW() ELSE started_at   END,
			completed_at = CASE WHEN $3 = 'installed'   AND completed_at IS NULL THEN NOW() ELSE completed_at END,
			reboot_sent_at = CASE WHEN $3 = 'reboot_sent' AND reboot_sent_at IS NULL THEN NOW() ELSE reboot_sent_at END
		WHERE update_id = $1 AND device_id = $2
	`, updateID, deviceID, status)
	return err
}

// RemoveDeviceFromUpdate drops a device's row from a deployment, but only while it
// is still 'pending' — i.e. nothing has been sent to the device yet. Returns true if
// a row was actually removed (false when the device already moved past pending or was
// not a target). Once a device has started downloading it can no longer be removed.
func (d *DB) RemoveDeviceFromUpdate(ctx context.Context, updateID int, deviceID uuid.UUID) (bool, error) {
	// Pending, or terminal-and-not-installed: a row that failed (a cancelled download,
	// a device that never came back) is finished work nobody wants in the rollout's
	// numbers any more, and removing it is how an operator says "not this one". An
	// installed row stays — it is the record that this device got the build.
	tag, err := d.pool.Exec(ctx, `
		DELETE FROM update_devices
		WHERE update_id = $1 AND device_id = $2 AND status IN ('pending', 'failed', 'canceled')
	`, updateID, deviceID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// OptimisticallyCompleteReboot used to mark the row installed and rewrite the
// device's build_id the moment a reboot was pushed. That showed the new build
// before the device had actually booted into it, so it is now a no-op: the row
// stays at reboot_sent ("Reboot pending" on the device page) until the device
// checks in on the target build and CompleteUpdatesAtTargetBuild confirms it.
// RedriveStuckReboots re-issues the reboot if that never happens. Kept as a
// function so the call sites read the same; it only touches updated_at.
func (d *DB) OptimisticallyCompleteReboot(ctx context.Context, updateID int, deviceID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `UPDATE update_devices SET updated_at = NOW() WHERE update_id = $1 AND device_id = $2 AND status IN ('awaiting_reboot', 'reboot_sent')`, updateID, deviceID)
	return err
}

// CompleteUpdatesAtTargetBuild marks installed every active, not-yet-terminal
// deployment row for this device whose release ships a package targeting the
// device's current build. This is the authoritative "the device is now running
// the new build" reconciliation, and it deliberately does NOT go through
// ResolveUpdateForDevice: that resolver only returns a row while an active
// package still matches the device's *current* build (source_build_id for an
// incremental), so once a device updates off the source build onto the target
// it resolves to nil — leaving the row stuck at reboot_sent and, worse, letting
// the OTA be re-sent. Matching on target_build_id closes both gaps. Returns the
// affected update IDs so the caller can complete the deployment and refresh it.
func (d *DB) CompleteUpdatesAtTargetBuild(ctx context.Context, deviceID uuid.UUID, buildID string) ([]int, error) {
	if buildID == "" {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, `
		UPDATE update_devices ud
		SET status = 'installed', error_code = '', updated_at = NOW(),
		    completed_at = COALESCE(ud.completed_at, NOW())
		FROM updates u
		WHERE ud.update_id = u.id
		  AND ud.device_id = $1
		  -- Include 'canceled' deployments so a device that was mid-flight when the
		  -- deployment was canceled still gets its row reconciled to 'installed' when
		  -- it lands on the target build (it was told to finish), rather than being
		  -- stranded at 'downloading'/'reboot_sent'.
		  AND u.status IN ('active', 'canceled')
		  AND ud.status NOT IN ('installed', 'canceled')
		  AND EXISTS (
		      SELECT 1 FROM ota_packages pk
		      WHERE pk.release_id = u.release_id
		        AND pk.status = 'active'
		        AND pk.target_build_id = $2
		  )
		RETURNING ud.update_id
	`, deviceID, buildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SetUpdateDeviceFailed marks a device's deployment row failed and records the
// error_code it reported, so the deployment view can show why it failed.
func (d *DB) SetUpdateDeviceFailed(ctx context.Context, updateID int, deviceID uuid.UUID, errorCode string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE update_devices SET status = 'failed', error_code = $3, updated_at = NOW()
		WHERE update_id = $1 AND device_id = $2
	`, updateID, deviceID, errorCode)
	return err
}

// SetUpdateDeviceForceFull pins a device's deployment row to the full OTA image,
// so ResolveUpdateForDevice stops offering a matching incremental. Used after an
// incremental fails on the device, and on operator retry.
func (d *DB) SetUpdateDeviceForceFull(ctx context.Context, updateID int, deviceID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE update_devices SET force_full = true, updated_at = NOW()
		WHERE update_id = $1 AND device_id = $2
	`, updateID, deviceID)
	return err
}

// ClearUpdateDeviceForceFull un-pins a device from the full image, so
// ResolveUpdateForDevice is free to offer a matching incremental again. Used by a
// plain (non-"full image") retry — without this, a device that was ever force-fulled
// (an earlier full-image retry, or the pre-delivery-choice retry button) would stay
// pinned to full forever, since force_full is otherwise only ever set, never cleared.
func (d *DB) ClearUpdateDeviceForceFull(ctx context.Context, updateID int, deviceID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE update_devices SET force_full = false, updated_at = NOW()
		WHERE update_id = $1 AND device_id = $2
	`, updateID, deviceID)
	return err
}

// CheckAndCompleteUpdate marks an update as "complete" if all its targets are "installed".
func (d *DB) CheckAndCompleteUpdate(ctx context.Context, updateID int) error {
	// A deployment is complete once every device has reached a TERMINAL state.
	// 'installed', 'failed' and 'canceled' are all terminal for the device — using
	// only "!= 'installed'" left a deployment stuck 'active' forever the moment any
	// device failed or was canceled (there is no periodic sweep to unstick it).
	_, err := d.pool.Exec(ctx, `
		UPDATE updates SET status = 'complete'
		WHERE id = $1 AND status = 'active'
		AND NOT EXISTS (
			SELECT 1 FROM update_devices
			WHERE update_id = $1 AND status NOT IN ('installed', 'failed', 'canceled')
		)
	`, updateID)
	return err
}

// CompleteSettledDeployments marks 'complete' every active deployment whose
// devices have all reached a terminal state. CheckAndCompleteUpdate only runs on
// the ack paths; a device that reached the target build through a check-in, or a
// deployment reactivated by add-targets after it had completed, could otherwise
// sit 'active' with every device installed. Called when the hub or a deployment
// page is opened.
func (d *DB) CompleteSettledDeployments(ctx context.Context) error {
	// A device rebooted to install and offline since is not failed (user, 21 Oct 2026:
	// offline devices get the update when they are back). Until 21 Oct it was failed as
	// REBOOT_NO_CHECKIN after 12 hours; now it stays reboot_sent, the rollout stays open
	// for it, the deployment page lists it under "needs a look", and its next check-in
	// on the new build marks it installed (CompleteUpdatesAtTargetBuild).
	_, err := d.pool.Exec(ctx, `
		UPDATE updates u SET status = 'complete'
		WHERE u.status = 'active'
		AND EXISTS (SELECT 1 FROM update_devices ud WHERE ud.update_id = u.id)
		AND NOT EXISTS (
			SELECT 1 FROM update_devices ud
			WHERE ud.update_id = u.id AND ud.status NOT IN ('installed', 'failed', 'canceled')
		)
	`)
	return err
}

// ReactivateUpdate flips a previously-completed update back to 'active'. Needed
// when a device is retried or newly added under a deployment that
// CheckAndCompleteUpdate already marked complete — otherwise
// ResolveUpdateForDevice (which requires status='active') would never serve the
// re-pending device and it would sit at "pending" forever.
func (d *DB) ReactivateUpdate(ctx context.Context, updateID int) error {
	_, err := d.pool.Exec(ctx, `UPDATE updates SET status = 'active' WHERE id = $1 AND status = 'complete'`, updateID)
	return err
}

// ReconcileStrandedUpdates reactivates any deployment that was marked 'complete'
// while it still has a device in a non-terminal state (e.g. one re-pended by a
// retry). Such devices are never served by ResolveUpdateForDevice (status='active'
// only) and sit at "pending" forever. Best-effort: called once at startup, NOT
// part of RunMigrations — a data reconciliation must never crash the server. It
// is idempotent (once the device installs, CheckAndCompleteUpdate completes it
// again) and excludes failed/canceled rows so they are not silently re-pushed.
func (d *DB) ReconcileStrandedUpdates(ctx context.Context) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE updates u SET status = 'active'
		WHERE u.status = 'complete'
		  AND EXISTS (
		    SELECT 1 FROM update_devices ud
		    WHERE ud.update_id = u.id
		      AND ud.status NOT IN ('installed', 'canceled', 'failed')
		  )
	`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CancelDeployment stops an active deployment: devices that haven't started yet
// (pending) are marked canceled, and the update leaves 'active' so
// ResolveUpdateForDevice no longer hands it out on check-in. Devices already
// downloading/installed are left alone — their work continues on-device.
func (d *DB) CancelDeployment(ctx context.Context, updateID int) error {
	// Atomic: cancel the pending device rows and the deployment together, so a
	// failure between the two can't leave devices canceled while the deployment
	// stays 'active' (a half-cancelled state).
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		UPDATE update_devices SET status = 'canceled', error_code = '', updated_at = NOW()
		WHERE update_id = $1 AND status = 'pending'
	`, updateID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE updates SET status = 'canceled' WHERE id = $1 AND status = 'active'
	`, updateID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetUpdateTargets returns the device targets for an update.
func (d *DB) GetUpdateTargets(ctx context.Context, updateID int) ([]UpdateTarget, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT ud.update_id, ud.device_id, d.serial_number, d.build_id, ud.status, ud.error_code, ud.updated_at, d.last_seen_at,
		       ud.started_at, ud.completed_at, ud.reboot_sent_at,
		       d.restaurant_id, COALESCE(r.name, ''), ud.source_build_id, ud.force_full,
		       ud.after_update_id, COALESCE(ar.version, ''),
		       COALESCE(au.status = 'active' AND COALESCE(aud.status, '') <> 'installed', false)
		FROM update_devices ud
		JOIN devices d ON d.id = ud.device_id
		LEFT JOIN restaurants r ON r.id = d.restaurant_id
		LEFT JOIN updates au ON au.id = ud.after_update_id
		LEFT JOIN releases ar ON ar.id = au.release_id
		LEFT JOIN update_devices aud ON aud.update_id = ud.after_update_id AND aud.device_id = ud.device_id
		WHERE ud.update_id = $1
		ORDER BY d.serial_number
	`, updateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UpdateTarget
	for rows.Next() {
		var t UpdateTarget
		if err := rows.Scan(&t.UpdateID, &t.DeviceID, &t.SerialNumber, &t.BuildID, &t.Status, &t.ErrorCode, &t.UpdatedAt, &t.LastSeenAt, &t.StartedAt, &t.CompletedAt, &t.RebootSentAt,
			&t.VenueID, &t.Venue, &t.SourceBuildID, &t.ForceFull, &t.AfterUpdateID, &t.AfterVersion, &t.AfterWaiting); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// HasPendingOTACommand reports whether an OTA is already in flight for a device, so
// the check-in resolver doesn't create a second one on top of it. Two independent
// self-heal properties, both hardening against FW-2026-000XXX (a device stuck
// "downloading 0%" forever because an abandoned/superseded OTA command sat at
// 'delivered' and nothing ever recognized it as done):
//  1. 'expired'/'cancelled' count as terminal here, matching what
//     ExpireOverdueCommands (a 24h backstop for exactly this situation) actually
//     marks a stalled command as — previously this check didn't accept either, so
//     even after that sweep ran, a device stayed permanently blocked anyway.
//  2. A staleness cutoff on its own (30m — long enough that a normal check-in cadence
//     won't mistake genuine in-progress work for stuck, short enough that a truly
//     stuck device self-heals on close to its next check-in instead of staying
//     visibly "stuck" on the dashboard for hours).
// GetActiveOTACommandID returns the most recent non-terminal OTA command targeting
// this device, or ok=false if there is none — used to address a cancel_command frame
// at the right command id (the device dedups cancels by id, so this must match
// exactly what it's currently working on).
func (d *DB) GetActiveOTACommandID(ctx context.Context, deviceID uuid.UUID) (id uuid.UUID, ok bool, err error) {
	err = d.pool.QueryRow(ctx, `
		SELECT c.id FROM commands c
		JOIN command_targets ct ON ct.command_id = c.id AND ct.target_id = $1
		LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = $1
		WHERE c.type = 'ota' AND (cs.status IS NULL OR cs.status NOT IN ('installed', 'failed', 'completed', 'expired', 'cancelled'))
		ORDER BY c.created_at DESC LIMIT 1
	`, deviceID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, err
	}
	return id, true, nil
}

func (d *DB) HasPendingOTACommand(ctx context.Context, deviceID uuid.UUID) (bool, error) {
	var exists bool
	err := d.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM commands c
			JOIN command_targets ct ON ct.command_id = c.id AND ct.target_id = $1
			LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = $1
			WHERE c.type = 'ota'
			AND (
				(
					(cs.status IS NULL OR cs.status NOT IN ('installed', 'failed', 'completed', 'expired', 'cancelled'))
					AND COALESCE(cs.updated_at, c.created_at) > NOW() - INTERVAL '30 minutes'
				)
				OR (cs.status = 'failed' AND cs.updated_at > NOW() - INTERVAL '1 hour')
			)
		)
	`, deviceID).Scan(&exists)
	return exists, err
}

// CreateOTACommandIfNone atomically creates an OTA command for a device unless one
// is already in flight, returning nil when one already exists. A Postgres advisory
// lock keyed on the device serializes concurrent check-ins (a device using both
// HTTP check-in and WS telemetry, or two rapid check-ins) so they can't both pass
// the no-pending check and dispatch duplicate OTA downloads.
func (d *DB) CreateOTACommandIfNone(ctx context.Context, deviceID uuid.UUID, payload json.RawMessage) (*Command, error) {
	var cmd *Command
	err := d.withAdvisoryLock(ctx, advisoryKeyUUID(deviceID), func(ctx context.Context) error {
		hasPending, err := d.HasPendingOTACommand(ctx, deviceID)
		if err != nil {
			return err
		}
		if hasPending {
			return nil
		}
		cmd, err = d.CreateCommand(ctx, "ota", "", payload, "devices", []uuid.UUID{deviceID})
		return err
	})
	return cmd, err
}

// TryCreateOTACommand builds and inserts the OTA command for an already-resolved
// update/device pair, if applicable — an incremental package only applies when
// currentBuildID matches its source build (a full package always applies). Returns
// (nil, nil), not an error, when not applicable or one is already in flight
// (CreateOTACommandIfNone's own advisory-locked check). On success also flips the
// device's update_devices row to "downloading". Shared by the checkin resolver
// (internal/api) and the dashboard's deploy-time immediate push so the exact
// conditions for creating an OTA command can't drift between the two call sites.
func (d *DB) TryCreateOTACommand(ctx context.Context, upd *Update, deviceID uuid.UUID, currentBuildID string) (*Command, error) {
	pkg := upd.OtaPackage
	if pkg == nil {
		return nil, nil
	}
	if pkg.Type == "incremental" && pkg.SourceBuildID != currentBuildID {
		return nil, nil
	}
	p := map[string]any{
		"package_id":      pkg.ID,
		"build_id":        pkg.TargetBuildID,
		"update_url":      pkg.UpdateURL,
		"reboot_behavior": upd.RebootBehavior,
	}
	if upd.ScheduledTime != nil {
		p["scheduled_time"] = upd.ScheduledTime.UTC().Format(time.RFC3339)
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	// No admission control (user, 23 Oct 2026: "remove both of these caps"): every device
	// that checks in is handed the update at once. The waves (3 per venue, 8 in all) came
	// from 12 Oct, when sixteen devices pulling through one access point stalled.
	cmd, err := d.CreateOTACommandIfNone(ctx, deviceID, payload)
	if err != nil || cmd == nil {
		return cmd, err
	}
	if err := d.SetUpdateDeviceStatus(ctx, upd.ID, deviceID, "downloading"); err != nil {
		return nil, err
	}
	return cmd, nil
}

// withAdvisoryLock runs fn while holding a Postgres session advisory lock for key,
// serializing callers so a check-then-insert cannot race into duplicate rows. The
// lock lives on one pooled connection and is released even if ctx is cancelled.
func (d *DB) withAdvisoryLock(ctx context.Context, key int64, fn func(context.Context) error) error {
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
	return fn(ctx)
}

// advisoryKeyUUID derives a stable advisory-lock key from a uuid. Occasional key
// collisions only cause unrelated operations to serialize briefly, which is harmless.
func advisoryKeyUUID(id uuid.UUID) int64 { return int64(binary.BigEndian.Uint64(id[:8])) }

// ClearPendingOTACommands marks in-progress OTA commands for a device as
// failed so HasPendingOTACommand returns false, allowing immediate retry.
func (d *DB) ClearPendingOTACommands(ctx context.Context, deviceID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `
			INSERT INTO command_status (command_id, device_id, status, updated_at)
			SELECT c.id, ct.target_id, 'failed', NOW() - INTERVAL '2 hours'
			FROM commands c
			JOIN command_targets ct ON ct.command_id = c.id AND ct.target_id = $1
			LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = $1
			WHERE c.type = 'ota'
			AND (
				cs.status IS NULL
				OR cs.status NOT IN ('installed', 'failed', 'completed')
				OR (cs.status = 'failed' AND cs.updated_at > NOW() - INTERVAL '2 hours')
			)
			ON CONFLICT (command_id, device_id) DO UPDATE
				SET status = EXCLUDED.status, updated_at = EXCLUDED.updated_at
		`, deviceID)
	return err
}

// OTARowProgress is what a device last told us about one deployment's OTA: the phase it
// reported, how far it had got, and when that report landed. It comes from
// command_status, which is written on every progress frame — unlike update_devices,
// whose updated_at only moves at status checkpoints and so cannot tell a long download
// from a dead one.
type OTARowProgress struct {
	Status     string
	Percent    int
	BytesDone  int64
	BytesTotal int64
	ReportAt   time.Time
	// CmdAt is when the attempt this report belongs to was sent. Everything older than
	// it belongs to a previous attempt and must not describe this one.
	CmdAt time.Time
}

// OTAProgressForUpdate returns the latest OTA report per device for a deployment. The
// OTA command is matched to the deployment the same way the stall sweep matches it, so
// the page and the sweep can never disagree about who is still moving.
func (d *DB) OTAProgressForUpdate(ctx context.Context, updateID int) (map[uuid.UUID]OTARowProgress, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT ON (cs.device_id)
		       cs.device_id, cs.status, COALESCE(cs.progress, 0), cs.bytes_done, cs.bytes_total, cs.updated_at, c.created_at
		FROM update_devices ud
		JOIN updates u ON u.id = ud.update_id
		JOIN command_targets ct ON ct.target_id = ud.device_id
		JOIN commands c ON c.id = ct.command_id AND c.type = 'ota' AND c.created_at >= u.created_at
		JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = ud.device_id
		  -- A terminal ack for the previous attempt can land against the new command
		  -- (seen on 12 Oct: a 'failed' stamped 12:02 attached to a command created at
		  -- 12:05), which then shows "failed 0%" beside a row that is downloading fine.
		  AND cs.updated_at >= c.created_at
		WHERE ud.update_id = $1
		ORDER BY cs.device_id, c.created_at DESC, cs.updated_at DESC
	`, updateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]OTARowProgress{}
	for rows.Next() {
		var id uuid.UUID
		var p OTARowProgress
		if err := rows.Scan(&id, &p.Status, &p.Percent, &p.BytesDone, &p.BytesTotal, &p.ReportAt, &p.CmdAt); err != nil {
			return nil, err
		}
		out[id] = p
	}
	return out, rows.Err()
}

// SetUpdateDeviceAfter makes a device take another update first: its row in updateID is
// not served until afterID is installed on it (or afterID is no longer active).
func (d *DB) SetUpdateDeviceAfter(ctx context.Context, updateID int, deviceID uuid.UUID, afterID int) error {
	_, err := d.pool.Exec(ctx, `UPDATE update_devices SET after_update_id = $3 WHERE update_id = $1 AND device_id = $2`, updateID, deviceID, afterID)
	return err
}
