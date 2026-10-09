package db

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Having no command channel is this fleet's quietest failure: the firmware client takes
// commands over the WebSocket only, so a device whose socket is gone keeps reporting
// telemetry over the HTTP check-in, keeps looking healthy on every page that reads
// last_seen_at, and executes nothing that is sent to it. AT070AABU00077 sat like that
// for 15 hours (9 Oct 2026) — fresh check-ins, a queued shell command stuck at
// 'delivered', and no way to reach it short of someone power-cycling the tablet by hand.
//
// ws_missing_since is when that state started, stamped on the check-in path, so the
// condition can be alerted on and measured instead of inferred by hand.

// MarkWSMissing stamps the start of this device's socket-less spell, leaving an existing
// stamp alone, and returns the start. Idempotent: every check-in calls it, only the first
// one writes a time.
func (d *DB) MarkWSMissing(ctx context.Context, deviceID uuid.UUID) (time.Time, error) {
	var since time.Time
	err := d.pool.QueryRow(ctx, `
		UPDATE devices SET ws_missing_since = COALESCE(ws_missing_since, NOW())
		WHERE id = $1
		RETURNING ws_missing_since`, deviceID).Scan(&since)
	return since, err
}

// ClearWSMissing forgets the spell — the device has a socket again. Returns whether a
// stamp was actually cleared, so the caller only resolves an alert when something changed.
func (d *DB) ClearWSMissing(ctx context.Context, deviceID uuid.UUID) (bool, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE devices SET ws_missing_since = NULL
		WHERE id = $1 AND ws_missing_since IS NOT NULL`, deviceID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// CreateOrEscalateAlert is CreateAlertIfAbsent that also reports an escalation, for a
// condition that worsens while it stays open: twenty minutes with no command channel is
// a warning, two hours is a device nobody can reach, and the second one has to notify
// even though the alert row already exists.
func (d *DB) CreateOrEscalateAlert(ctx context.Context, ruleID *uuid.UUID, typ string, deviceID uuid.UUID, severity, summary string, detail any) (inserted, escalated bool, err error) {
	u, err := d.upsertAlert(ctx, ruleID, typ, deviceID, severity, summary, detail)
	return u.Inserted, u.Escalated, err
}

// WSMissingDevice is one device that is reporting but holds no socket, for the fleet-level
// gauge: the single number that would have shown this the day it started.
type WSMissingDevice struct {
	DeviceID uuid.UUID
	Serial   string
	Since    time.Time
}

// ListWSMissingDevices returns devices whose socket-less spell has lasted at least
// minDuration and that have checked in within seenWithin (so a powered-off device, which
// also has no socket, is not counted — that is plain Offline and already alerted).
func (d *DB) ListWSMissingDevices(ctx context.Context, minDuration, seenWithin time.Duration) ([]WSMissingDevice, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, serial_number, ws_missing_since
		FROM devices
		WHERE ws_missing_since IS NOT NULL
		  AND ws_missing_since <= NOW() - $1::interval
		  AND last_seen_at > NOW() - $2::interval
		  AND NOT hidden
		  AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')
		ORDER BY ws_missing_since ASC`, minDuration, seenWithin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WSMissingDevice
	for rows.Next() {
		var m WSMissingDevice
		if err := rows.Scan(&m.DeviceID, &m.Serial, &m.Since); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// ClearWSMissingForDevices drops the stamp for devices that turned out to hold a socket
// after all — the sweep's correction, for a device that reconnected without checking in.
func (d *DB) ClearWSMissingForDevices(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE devices SET ws_missing_since = NULL WHERE id = ANY($1::uuid[])`, ids)
	return err
}

// agentReadsHTTPCommands is the stored half of api.readsHTTPCommands: whether this
// device's last check-in said it can take commands over HTTP. Used by the dashboard to
// say whether a socket-less device is merely slow to reach or wholly unreachable.
func AgentReadsHTTPCommands(latestExtra json.RawMessage) bool {
	var probe struct {
		HTTPCommands bool `json:"http_commands"`
	}
	return len(latestExtra) > 0 && json.Unmarshal(latestExtra, &probe) == nil && probe.HTTPCommands
}

// CommandChannelState returns when this device's socket-less spell started, when a command
// last completed for it, and the channel its last check-in implied. Read by the device page
// so the one question an operator has before pressing a button — will this run? — has an
// answer on the page.
func (d *DB) CommandChannelState(ctx context.Context, deviceID uuid.UUID) (since, roundTrip time.Time, channel string, err error) {
	var s, rt *time.Time
	err = d.pool.QueryRow(ctx, `
		SELECT ws_missing_since, last_round_trip_ok_at, command_channel
		FROM devices WHERE id = $1`, deviceID).Scan(&s, &rt, &channel)
	if err != nil {
		return time.Time{}, time.Time{}, "", err
	}
	if s != nil {
		since = *s
	}
	if rt != nil {
		roundTrip = *rt
	}
	return since, roundTrip, channel, nil
}

// CanaryCandidate is a device due a control probe.
type CanaryCandidate struct {
	DeviceID uuid.UUID
	Serial   string
}

// DevicesNeedingCanary returns devices that are reporting but have no recent proof that a
// command can actually reach them: nothing has completed for them in staleAfter, and they
// have no command queued right now.
//
// The "nothing queued" condition keeps the probe honest — if there is already work waiting
// for this device, that work is the measurement, and adding a probe would only measure the
// probe. Devices that have left the fleet are skipped; so are ones we have not heard from
// at all, because a powered-off device is plain Offline and already alerted.
func (d *DB) DevicesNeedingCanary(ctx context.Context, staleAfter, seenWithin time.Duration, limit int) ([]CanaryCandidate, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.id, d.serial_number
		FROM devices d
		WHERE NOT d.hidden
		  AND d.enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')
		  AND d.custody_server = ''
		  AND d.last_seen_at > NOW() - $2::interval
		  AND (d.last_round_trip_ok_at IS NULL OR d.last_round_trip_ok_at < NOW() - $1::interval)
		  -- One probe per staleAfter window, full stop. Keyed on the command existing at
		  -- all rather than on its status: a command that has not been handed out yet has
		  -- NO command_status row, so a status-based check saw nothing queued and probed
		  -- the same device every minute (observed on stage: 3 devices re-probed on every
		  -- tick, which would have been 1440 probes a day each instead of one).
		  AND NOT EXISTS (
			SELECT 1 FROM commands c2
			JOIN command_targets ct2 ON ct2.command_id = c2.id
			WHERE ct2.target_id = d.id AND c2.type = 'canary'
			  AND c2.created_at > NOW() - $1::interval)
		  -- And never while this device has real work outstanding: that work is the
		  -- measurement, and a probe would only measure the probe. Again by existence, not
		  -- status, so a command still waiting to be delivered counts.
		  AND NOT EXISTS (
			SELECT 1 FROM commands c3
			JOIN command_targets ct3 ON ct3.command_id = c3.id
			WHERE ct3.target_id = d.id
			  AND c3.created_at > NOW() - INTERVAL '1 hour'
			  AND NOT EXISTS (
				SELECT 1 FROM command_status s3
				WHERE s3.command_id = c3.id AND s3.device_id = d.id
				  AND s3.status IN ('installed', 'failed', 'completed', 'cancelled', 'expired')))
		ORDER BY d.last_round_trip_ok_at ASC NULLS FIRST
		LIMIT $3`, staleAfter, seenWithin, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CanaryCandidate
	for rows.Next() {
		var c CanaryCandidate
		if err := rows.Scan(&c.DeviceID, &c.Serial); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// StaleControlDevices returns devices that are reporting and whose last proven round trip
// is older than staleAfter — i.e. devices we believe we manage and have no evidence we do.
// Used for the control_stale alert, which is the question nobody could ask before:
// AT070AABU00077 was in this state for 15 hours and every page called it healthy.
func (d *DB) StaleControlDevices(ctx context.Context, staleAfter, seenWithin time.Duration) ([]WSMissingDevice, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.id, d.serial_number, COALESCE(d.last_round_trip_ok_at, d.created_at)
		FROM devices d
		WHERE NOT d.hidden
		  AND d.enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')
		  AND d.custody_server = ''
		  AND d.last_seen_at > NOW() - $2::interval
		  AND (d.last_round_trip_ok_at IS NULL OR d.last_round_trip_ok_at < NOW() - $1::interval)
		ORDER BY d.last_round_trip_ok_at ASC NULLS FIRST`, staleAfter, seenWithin)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WSMissingDevice
	for rows.Next() {
		var m WSMissingDevice
		if err := rows.Scan(&m.DeviceID, &m.Serial, &m.Since); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
