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
