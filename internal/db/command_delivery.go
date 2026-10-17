package db

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"

	prod "mdm/internal/product"
)

// StuckDelivery is one command wedged at 'delivered' for a specific device — the frame
// was enqueued onto the socket but the device never acknowledged progress or a result.
type StuckDelivery struct {
	CommandID uuid.UUID
	DeviceID  uuid.UUID
	Type      string
	ApkURL    string
	Payload   json.RawMessage
}

// ListStuckDeliveredCommands returns commands sitting at 'delivered' for a device with
// no progress for longer than staleSeconds — the half-open-socket wedge. The caller
// re-pushes them to devices that are currently connected (an offline device gets them
// on its next WS-connect flush instead). Reboot is excluded (it completes via
// CompleteDeliveredReboots, never a re-push), and short-TTL one-shots (shell/screenshot)
// are only re-driven while still within their 5-minute leash so an ancient one isn't
// resurrected. Applies to every other action type — install, uninstall, kiosk, boot
// logo, query, etc. — because any of them can wedge the same way.
func (d *DB) ListStuckDeliveredCommands(ctx context.Context, staleSeconds int) ([]StuckDelivery, error) {
	if staleSeconds <= 0 {
		staleSeconds = 90
	}
	rows, err := d.pool.Query(ctx, `
		SELECT c.id, cs.device_id, c.type, c.apk_url, c.payload
		FROM command_status cs
		JOIN commands c ON c.id = cs.command_id
		WHERE cs.status = 'delivered'
		  AND cs.received_at IS NULL          -- device never confirmed receipt; a confirmed one already has it
		  AND cs.updated_at <= NOW() - make_interval(secs => $1)
		  AND c.type <> 'reboot'
		  AND c.created_at > NOW() - INTERVAL '1 hour'   -- never re-push a stale command
		  AND (c.type NOT IN ('shell', 'screenshot') OR c.created_at > NOW() - INTERVAL '5 minutes')
	`, staleSeconds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StuckDelivery
	for rows.Next() {
		var s StuckDelivery
		if err := rows.Scan(&s.CommandID, &s.DeviceID, &s.Type, &s.ApkURL, &s.Payload); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	rows.Close()
	// The sweep re-pushes these to the device, so the secret fields go back in.
	for i := range out {
		out[i].Payload = d.DeliveryPayload(ctx, out[i].CommandID, out[i].Type, out[i].Payload)
	}
	return out, nil
}

// DevicesWithPendingInstall returns which of the given devices already have an
// in-flight install_apk for apkURL — a command targeting the device (directly, by
// group, or "all") whose per-device status is not yet terminal (pending or
// delivered, i.e. not installed/failed/completed). Used to stop piling up a second
// install of the same APK on a device that's already installing it.
func (d *DB) DevicesWithPendingInstall(ctx context.Context, apkURL string, deviceIDs []uuid.UUID, expirySec int) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool)
	if apkURL == "" || len(deviceIDs) == 0 {
		return out, nil
	}
	// Only a *recent* install counts as in-flight. An install that never completed
	// (device offline / command never delivered) leaves a non-terminal row forever;
	// without this bound it would block every future re-install of the same APK on
	// that device. Mirror the install-expiry window used elsewhere (expiry*3, ≥15m).
	if expirySec <= 0 {
		expirySec = 300
	}
	installExpiry := expirySec * 3
	if installExpiry < 900 {
		installExpiry = 900
	}
	rows, err := d.pool.Query(ctx, fmt.Sprintf(`
		SELECT dev.id
		FROM unnest($2::uuid[]) AS dev(id)
		WHERE EXISTS (
			SELECT 1 FROM commands c
			WHERE c.type = 'install_apk' AND c.apk_url = $1
			  AND c.created_at > NOW() - INTERVAL '%d seconds'
			  AND (
				c.target_type = 'all'
				OR (c.target_type = 'devices' AND EXISTS (
					SELECT 1 FROM command_targets ct WHERE ct.command_id = c.id AND ct.target_id = dev.id))
				OR (c.target_type = 'groups' AND EXISTS (
					SELECT 1 FROM command_targets ct JOIN device_groups dg ON dg.group_id = ct.target_id
					WHERE ct.command_id = c.id AND dg.device_id = dev.id))
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM command_status cs
				WHERE cs.command_id = c.id AND cs.device_id = dev.id
				  AND cs.status IN ('installed','failed','completed'))
		)
	`, installExpiry), apkURL, deviceIDs)
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

// MarkCommandsDelivered records that these commands were sent to the device.
// SetCommandStatusForDevices upserts one command's status for many devices in a
// single statement — batches the pushCommand delivery/ack writes (previously one
// query per online target). overwrite=true updates an existing row (ack), false
// leaves it (delivered). Callers pass already-targeted device IDs.
func (d *DB) SetCommandStatusForDevices(ctx context.Context, commandID uuid.UUID, deviceIDs []uuid.UUID, status string, overwrite bool) error {
	if len(deviceIDs) == 0 {
		return nil
	}
	conflict := "DO NOTHING"
	if overwrite {
		conflict = "DO UPDATE SET status = EXCLUDED.status, updated_at = NOW()"
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO command_status (command_id, device_id, status, updated_at)
		SELECT $1, dev, $3, NOW() FROM UNNEST($2::uuid[]) AS dev
		ON CONFLICT (command_id, device_id) `+conflict, commandID, deviceIDs, status)
	return err
}

func (d *DB) MarkCommandsDelivered(ctx context.Context, deviceID uuid.UUID, commandIDs []uuid.UUID) error {
	for _, cid := range commandIDs {
		_, err := d.pool.Exec(ctx, `
			INSERT INTO command_status (command_id, device_id, status, updated_at)
			VALUES ($1, $2, 'delivered', NOW())
			ON CONFLICT (command_id, device_id) DO NOTHING
		`, cid, deviceID)
		if err != nil {
			return err
		}
	}
	return nil
}

// CompleteDeliveredReboots marks a device's outstanding reboot commands completed,
// but only the ones the device has demonstrably obeyed: bootedAt is when its current
// boot started (now − uptime_seconds, from the check-in), and a reboot delivered
// before that boot is one this boot answers.
//
// Reboot is acked 'delivered' when sent, never 'completed': a successful WS push only
// means "queued to the socket". Reconnecting was the old proof, and it was wrong —
// a network blip reconnects a device that never rebooted, which is the false
// 'completed' testers kept seeing (FW-2026-000033). Uptime is the only honest
// evidence. Only rows still in 'delivered' move, so a routine check-in can't
// resurrect a command that has already reached a terminal state.
func (d *DB) CompleteDeliveredReboots(ctx context.Context, deviceID uuid.UUID, bootedAt time.Time) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE command_status cs
		SET status = 'completed', updated_at = NOW()
		FROM commands c
		WHERE c.id = cs.command_id
		  AND cs.device_id = $1
		  AND c.type = 'reboot'
		  AND cs.status = 'delivered'
		  -- When the device first got it, not when the row was last touched: a reboot is
		  -- pushed again when the device reconnects after rebooting (the client knows the
		  -- id and does not reboot twice), which refreshed updated_at past the boot and left
		  -- the reboot "delivered" for ever, holding up every command queued after it.
		  AND COALESCE(cs.received_at, cs.updated_at) < $2
	`, deviceID, bootedAt)
	return err
}

func (d *DB) commandTargetsDevice(ctx context.Context, commandID, deviceID uuid.UUID) (bool, error) {
	var exists bool
	err := d.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM commands c
			WHERE c.id = $1
			AND (
				c.target_type = 'all'
				OR (c.target_type = 'devices' AND EXISTS (
					SELECT 1 FROM command_targets ct
					WHERE ct.command_id = c.id AND ct.target_id = $2
				))
				OR (c.target_type = 'groups' AND EXISTS (
					SELECT 1 FROM command_targets ct
					JOIN device_groups dg ON dg.group_id = ct.target_id
					WHERE ct.command_id = c.id AND dg.device_id = $2
				))
			)
		)
	`, commandID, deviceID).Scan(&exists)
	return exists, err
}

// AckCommand lets a device report installed or failed for a command.
func (d *DB) AckCommand(ctx context.Context, commandID, deviceID uuid.UUID, status string) error {
	targeted, err := d.commandTargetsDevice(ctx, commandID, deviceID)
	if err != nil {
		return err
	}
	if !targeted {
		return ErrCommandNotTargeted
	}

	// Don't let a device move a command backwards out of a terminal state (e.g.
	// re-report 'downloading'/'failed' after 'installed') — that would corrupt OTA
	// and deployment rollups. Re-acking the same terminal status is a harmless no-op.
	_, err = d.pool.Exec(ctx, `
		INSERT INTO command_status (command_id, device_id, status, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (command_id, device_id) DO UPDATE
			SET status = EXCLUDED.status, progress = NULL, updated_at = NOW()
			WHERE command_status.status NOT IN ('installed', 'failed', 'completed')
	`, commandID, deviceID, status)
	if err != nil {
		return err
	}
	// An unenroll that the device says it carried out is the last thing we will ever hear
	// from it: the agent has dropped Device Owner, forgotten this server and stopped
	// reporting. Record that here rather than at one ack endpoint, because devices ack over
	// several paths — otherwise a device that left would just look like one gone quiet.
	if status == "completed" || status == "installed" {
		_, err = d.pool.Exec(ctx, `
			UPDATE devices SET enrollment_status = $2, unenrolled_at = NOW(),
			       unenrolled_by = COALESCE((SELECT created_by FROM commands WHERE id = $3), '')
			WHERE id = $1 AND EXISTS (SELECT 1 FROM commands c WHERE c.id = $3 AND c.type = 'unenroll')`,
			deviceID, EnrollUnenrolled, commandID)
	}
	// The device is done with it, so it stops holding the lease — nothing is gained by
	// making a finished command wait out a work-time budget it no longer needs.
	if _, lerr := d.pool.Exec(ctx, `
		UPDATE command_status SET lease_id = NULL, lease_expires_at = NULL
		WHERE command_id = $1 AND device_id = $2`, commandID, deviceID); lerr != nil {
		log.Printf("[command-lease] release on ack %s: %v", commandID, lerr)
	}
	d.AppendCommandEvent(ctx, commandID, deviceID, status, nil)
	// A wifi_config's password is needed only until its devices are done with it.
	switch status {
	case "installed", "failed", "completed", "cancelled", "expired":
		d.dropFinishedSecret(ctx, commandID)
	}
	// A command that ran end to end is the only proof this device is controllable, as
	// opposed to merely reporting. Everything else — a socket, a check-in, a fresh
	// temperature — is liveness, which is what misled us about AT070AABU00077.
	if status == "completed" || status == "installed" {
		if rerr := d.StampCommandRoundTrip(ctx, deviceID); rerr != nil {
			log.Printf("[command-lease] round-trip stamp %s: %v", deviceID, rerr)
		}
	}
	return err
}

// MarkCommandReceived stamps received_at when a device confirms — via a 'received' ack —
// that it actually GOT the command, as opposed to 'delivered' which only means the frame
// was enqueued onto the socket. Only the first receipt is stamped and a terminal/progress
// row's status is never disturbed. Redrive and reconnect re-delivery key off
// received_at IS NULL, so this is exactly what stops a received command being re-pushed.
func (d *DB) MarkCommandReceived(ctx context.Context, commandID, deviceID uuid.UUID) error {
	targeted, err := d.commandTargetsDevice(ctx, commandID, deviceID)
	if err != nil {
		return err
	}
	if !targeted {
		return ErrCommandNotTargeted
	}
	_, err = d.pool.Exec(ctx, `
		INSERT INTO command_status (command_id, device_id, status, received_at, updated_at)
		VALUES ($1, $2, 'delivered', NOW(), NOW())
		ON CONFLICT (command_id, device_id) DO UPDATE
			SET received_at = COALESCE(command_status.received_at, NOW())
			WHERE command_status.received_at IS NULL
	`, commandID, deviceID)
	if err == nil {
		d.AppendCommandEvent(ctx, commandID, deviceID, "received", nil)
	}
	return err
}

// CommandBlocked reports whether a command must WAIT before being delivered to a device
// because an earlier (older) command for that device hasn't finished yet. Mirrors the
// per-device serialization gate in GetPendingCommandsForDevice; used to hold the immediate
// push at create time so no two commands run at once on a device (whole queue is FIFO).
func (d *DB) CommandBlocked(ctx context.Context, commandID, deviceID uuid.UUID) (bool, error) {
	var blocked bool
	err := d.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM commands c
			WHERE c.id = $1 AND EXISTS (
				SELECT 1 FROM commands c2
				WHERE c2.id <> c.id AND c2.created_at < c.created_at
				  AND (
					c2.target_type = 'all'
					OR (c2.target_type = 'devices' AND EXISTS (
						SELECT 1 FROM command_targets ct2 WHERE ct2.command_id = c2.id AND ct2.target_id = $2))
					OR (c2.target_type = 'groups' AND EXISTS (
						SELECT 1 FROM command_targets ct2 JOIN device_groups dg2 ON dg2.group_id = ct2.target_id
						WHERE ct2.command_id = c2.id AND dg2.device_id = $2))
				  )
				  AND NOT EXISTS (SELECT 1 FROM command_status s2 WHERE s2.command_id = c2.id AND s2.device_id = $2
					AND s2.status IN ('installed','failed','completed','cancelled','expired'))
				  AND (
						c2.created_at > NOW() - INTERVAL '1 hour'
						OR EXISTS (SELECT 1 FROM command_status s3 WHERE s3.command_id = c2.id AND s3.device_id = $2
							AND s3.status IN ('downloading','installing'))
				  )
			)
		)`, commandID, deviceID).Scan(&blocked)
	return blocked, err
}

// CancelDeviceCommand removes one command from a single device's queue by marking its
// per-device status 'cancelled' — never moving a row that already reached a terminal
// state. Per-device (upsert on command_status), so cancelling a group/all command on one
// device leaves the others untouched, and a not-yet-pushed 'pending' command with no
// status row is cancelled too.
func (d *DB) CancelDeviceCommand(ctx context.Context, commandID, deviceID uuid.UUID) error {
	targeted, err := d.commandTargetsDevice(ctx, commandID, deviceID)
	if err != nil {
		return err
	}
	if !targeted {
		return ErrCommandNotTargeted
	}
	_, err = d.pool.Exec(ctx, `
		INSERT INTO command_status (command_id, device_id, status, updated_at)
		VALUES ($1, $2, 'cancelled', NOW())
		ON CONFLICT (command_id, device_id) DO UPDATE
			SET status = 'cancelled', progress = NULL, updated_at = NOW()
			WHERE command_status.status NOT IN ('installed', 'failed', 'completed', 'cancelled', 'expired')
	`, commandID, deviceID)
	return err
}

// SetCommandProgress records an interim status ('downloading' or 'installing') for a
// command on a device, with an optional percent (0-100, meaningful while
// downloading). Terminal statuses go through AckCommand instead.
func (d *DB) SetCommandProgress(ctx context.Context, commandID, deviceID uuid.UUID, status string, progress *int) error {
	return d.SetCommandProgressBytes(ctx, commandID, deviceID, status, progress, 0, 0)
}

// SetCommandProgressBytes is SetCommandProgress plus the byte counters a client sends
// when it has them. Zero means "not reported": the previous figures are kept, so a
// client that reports bytes once and then falls back to percent does not blank them.
func (d *DB) SetCommandProgressBytes(ctx context.Context, commandID, deviceID uuid.UUID, status string, progress *int, bytesDone, bytesTotal int64) error {
	targeted, err := d.commandTargetsDevice(ctx, commandID, deviceID)
	if err != nil {
		return err
	}
	if !targeted {
		return ErrCommandNotTargeted
	}
	if progress != nil {
		p := *progress
		if p < 0 {
			p = 0
		} else if p > 100 {
			p = 100
		}
		progress = &p
	}
	// Interim progress must never overwrite a terminal state (a late 'downloading'
	// arriving after 'installed' would revert the row).
	_, err = d.pool.Exec(ctx, `
		INSERT INTO command_status (command_id, device_id, status, progress, bytes_done, bytes_total, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (command_id, device_id) DO UPDATE
			SET status = EXCLUDED.status, progress = EXCLUDED.progress,
			    bytes_done  = CASE WHEN EXCLUDED.bytes_done  > 0 THEN EXCLUDED.bytes_done  ELSE command_status.bytes_done  END,
			    bytes_total = CASE WHEN EXCLUDED.bytes_total > 0 THEN EXCLUDED.bytes_total ELSE command_status.bytes_total END,
			    updated_at = NOW()
			WHERE command_status.status NOT IN ('installed', 'failed', 'completed')
	`, commandID, deviceID, status, progress, bytesDone, bytesTotal)
	if err != nil {
		return err
	}
	// Progress is the lease heartbeat. Both clients already send interim
	// 'downloading'/'installing' acks, so a long install extends its own lease with code
	// that already exists — and an install whose device died stops extending it and
	// returns to the queue on its own, which is what expire-stalled-installs was for.
	if _, lerr := d.pool.Exec(ctx, `
		UPDATE command_status cs SET lease_expires_at = GREATEST(
			cs.lease_expires_at, NOW() + INTERVAL '30 minutes')
		WHERE cs.command_id = $1 AND cs.device_id = $2 AND cs.lease_expires_at IS NOT NULL`,
		commandID, deviceID); lerr != nil {
		log.Printf("[command-lease] heartbeat %s: %v", commandID, lerr)
	}
	d.AppendCommandEvent(ctx, commandID, deviceID, "progress", map[string]any{"status": status, "percent": progress})
	return nil
}

// ActiveOTAProgressRow is one device's persisted OTA progress, used to rehydrate
// shell.Manager's in-memory cache on process start — see ActiveOTAProgress.
type ActiveOTAProgressRow struct {
	DeviceID  uuid.UUID
	CommandID uuid.UUID
	Status    string // "downloading" or "installing" — see SetCommandProgress
	Progress  int
}

// ActiveOTAProgress returns the last-persisted progress for every OTA command
// still mid-flight (command_status.status IN downloading/installing). The live
// progress a device page/deployment page renders normally comes from
// shell.Manager's in-memory cache, which is empty right after a server
// restart — the caller rehydrates that cache from this on startup so an
// in-progress OTA doesn't show a blank "—" until the device happens to report
// fresh progress again.
func (d *DB) ActiveOTAProgress(ctx context.Context) ([]ActiveOTAProgressRow, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT cs.device_id, cs.command_id, cs.status, COALESCE(cs.progress, 0)
		FROM command_status cs
		JOIN commands c ON c.id = cs.command_id
		WHERE c.type = 'ota' AND cs.status IN ('downloading', 'installing')
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActiveOTAProgressRow
	for rows.Next() {
		var r ActiveOTAProgressRow
		if err := rows.Scan(&r.DeviceID, &r.CommandID, &r.Status, &r.Progress); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// StalledInstall identifies an install delivery the stall sweep marked failed.
type StalledInstall struct {
	CommandID uuid.UUID
	DeviceID  uuid.UUID
}

// ExpireStalledInstalls marks install_apk deliveries that stopped reporting progress
// as 'failed'. An install stuck 'downloading'/'installing' whose last report is older
// than stallMinutes has lost its device — it gave up after exhausting retries, rebooted,
// or its terminal ack was lost during the same network outage that stalled the download.
// Without this the row sits "in flight" forever, because install commands are exempt
// from the short command TTL. Returns the affected rows so callers can publish updates.
// ExpireStalledOTAs settles OTA device rows that stopped reporting. An agent that dies
// mid-download leaves its row at 'downloading' with the percent it last acked, and the
// deployment stays 'active' forever: RebootBlockedFor then refuses to reboot the device,
// which is the one action that would clear it, so the only way out is someone noticing
// and cancelling by hand. This is the same backstop ExpireStalledInstalls gives
// install_apk — the device is free to be rebooted and re-offered the update.
//
// Only rows whose updated_at has not moved for stallMinutes are touched, so a slow but
// live download (a large package on a poor link acks every few percent) is left alone.
//
// update_devices.updated_at only moves at status checkpoints, so it alone cannot
// tell a dead download from a long one: a download still running 30 minutes after
// it started would be failed while its device was reporting progress every few
// seconds. Progress frames are written to command_status (SetCommandProgress), so
// a row is only stalled when its device has also sent no OTA progress in the window.
func (d *DB) ExpireStalledOTAs(ctx context.Context, stallMinutes int) ([]StalledInstall, error) {
	rows, err := d.pool.Query(ctx, `
		UPDATE update_devices ud
		SET status = 'failed', error_code = 'stalled', updated_at = NOW()
		FROM updates u
		WHERE ud.update_id = u.id
		  AND u.status = 'active'
		  AND ud.status IN ('pending', 'downloading')
		  AND ud.updated_at <= NOW() - make_interval(mins => $1)
		  AND NOT EXISTS (
		      SELECT 1 FROM command_status cs
		      JOIN commands c ON c.id = cs.command_id
		      WHERE cs.device_id = ud.device_id
		        AND c.type = 'ota'
		        AND c.created_at >= u.created_at
		        AND cs.updated_at > NOW() - make_interval(mins => $1))
		RETURNING ud.update_id, ud.device_id
	`, stallMinutes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StalledInstall
	for rows.Next() {
		var updateID int
		var s StalledInstall
		if err := rows.Scan(&updateID, &s.DeviceID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ReviveStalledOTAs undoes a stall verdict the device has since disproved: a row
// failed as 'stalled' whose device is still reporting progress on this deployment's
// OTA goes back to 'downloading', and one whose OTA installed after the verdict moves
// on to 'awaiting_reboot' — where the installed ack would have put it had the row
// not been failed (ResolveUpdateForDevice skips failed rows, so the ack could not).
// A deployment that closed only because those rows counted as terminal is reopened.
// The OTA is matched to the deployment by release version and creation time, and
// only verdicts from the last day are reconsidered.
func (d *DB) ReviveStalledOTAs(ctx context.Context, stallMinutes int) ([]StalledInstall, error) {
	rows, err := d.pool.Query(ctx, `
		WITH s AS (
		    SELECT DISTINCT ON (ud2.update_id, ud2.device_id)
		           ud2.update_id, ud2.device_id, ud2.updated_at AS failed_at,
		           cs.status AS cs_status, cs.updated_at AS cs_at
		    FROM update_devices ud2
		    JOIN updates u ON u.id = ud2.update_id AND u.status IN ('active', 'complete')
		    JOIN releases rel ON rel.id = u.release_id
		    JOIN command_status cs ON cs.device_id = ud2.device_id
		    JOIN commands c ON c.id = cs.command_id
		    WHERE ud2.status = 'failed' AND ud2.error_code = 'stalled'
		      AND ud2.updated_at > NOW() - INTERVAL '1 day'
		      AND c.type = 'ota'
		      AND c.created_at >= u.created_at
		      AND c.payload->>'build_id' = rel.version
		    ORDER BY ud2.update_id, ud2.device_id, c.created_at DESC
		), rev AS (
		    UPDATE update_devices ud
		    SET status = CASE WHEN s.cs_status = 'installed' THEN 'awaiting_reboot' ELSE 'downloading' END,
		        error_code = '', updated_at = NOW()
		    FROM s
		    WHERE ud.update_id = s.update_id AND ud.device_id = s.device_id
		      AND ud.status = 'failed' AND ud.error_code = 'stalled'
		      AND ((s.cs_status = 'installed' AND s.cs_at >= s.failed_at)
		           OR (s.cs_status IN ('downloading', 'installing')
		               AND s.cs_at > NOW() - make_interval(mins => $1)))
		    RETURNING ud.update_id, ud.device_id
		), reopen AS (
		    UPDATE updates SET status = 'active'
		    WHERE status = 'complete' AND id IN (SELECT update_id FROM rev)
		    RETURNING id
		)
		SELECT rev.update_id, rev.device_id FROM rev
	`, stallMinutes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StalledInstall
	for rows.Next() {
		var updateID int
		var s StalledInstall
		if err := rows.Scan(&updateID, &s.DeviceID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *DB) ExpireStalledInstalls(ctx context.Context, stallMinutes int) ([]StalledInstall, error) {
	rows, err := d.pool.Query(ctx, `
		UPDATE command_status cs
		SET status = 'failed', progress = NULL, updated_at = NOW()
		FROM commands c
		WHERE cs.command_id = c.id
		  AND c.type IN `+prod.InstallShapedSQL()+`
		  AND cs.status IN ('downloading', 'installing')
		  AND cs.updated_at <= NOW() - make_interval(mins => $1)
		RETURNING cs.command_id, cs.device_id
	`, stallMinutes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StalledInstall
	for rows.Next() {
		var s StalledInstall
		if err := rows.Scan(&s.CommandID, &s.DeviceID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// shortLivedCommandsSQL: the command types that are only worth running right away. Past
// the expiry window, a still-pending or delivered one is shown 'expired' by every command
// view. A diagnostic query (or mic-gain read) sent to a device that was offline used to sit
// at pending/delivered forever, because 'query' was missing from these lists.
const shortLivedCommandsSQL = "('shell', 'collect_logs', 'screenshot', 'reboot', 'query', 'mic_gain_read', 'mic_gain_set')"

// ExpireOverdueCommands marks 'expired' any command that has sat non-terminal past its
// per-type delivery deadline WITHOUT ever starting — i.e. still 'pending'/'delivered'
// (never progressed to downloading/installing and never got a terminal ack). This is the
// backstop that stops a command wedging forever: a device that never returns, or that got
// the command but never acted, no longer shows "in flight" indefinitely. Deadlines are
// kept in lock-step with the delivery TTL in GetPendingCommandsForDevice, so a command
// expires exactly when it stops being deliverable. Reboot is exempt (its lifecycle is
// owned by CompleteDeliveredReboots / RedriveStuckReboots). Actively-progressing installs
// ('downloading'/'installing') are left to ExpireStalledInstalls. 'expired' is not in the
// terminal guard set, so a device that acts late can still un-expire it with a real ack.
func (d *DB) ExpireOverdueCommands(ctx context.Context) ([]StalledInstall, error) {
	rows, err := d.pool.Query(ctx, `
		UPDATE command_status cs
		SET status = 'expired', progress = NULL, updated_at = NOW()
		FROM commands c
		WHERE cs.command_id = c.id
		  AND cs.status IN ('pending', 'delivered')
		  AND c.type <> 'reboot'
		  AND c.created_at <= NOW() - CASE
				WHEN c.type IN ('shell', 'screenshot', 'query', 'mic_gain_read', 'mic_gain_set')
					THEN INTERVAL '5 minutes'
				ELSE INTERVAL '24 hours'
			END
		RETURNING cs.command_id, cs.device_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StalledInstall
	for rows.Next() {
		var s StalledInstall
		if err := rows.Scan(&s.CommandID, &s.DeviceID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
