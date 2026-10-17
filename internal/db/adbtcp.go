package db

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// AdbTCPGrant is the last wireless-adb change a device confirmed: the port it opened (0 =
// it was turned off), and when it switches itself off again (zero = it stays on).
type AdbTCPGrant struct {
	Port  int
	OffAt time.Time
}

// LastAdbTCP returns the last adb_tcp command the device completed. The client reports
// only whether adb is listening on the network, not the port or the deadline, so the
// device page reads those from the command that set them. ok is false when the device
// has never completed one.
func (d *DB) LastAdbTCP(ctx context.Context, deviceID uuid.UUID) (g AdbTCPGrant, ok bool, err error) {
	var payload json.RawMessage
	var at time.Time
	err = d.pool.QueryRow(ctx, `
		SELECT c.payload, s.updated_at
		FROM command_status s JOIN commands c ON c.id = s.command_id
		WHERE s.device_id = $1 AND c.type = 'adb_tcp' AND s.status = 'completed'
		ORDER BY s.updated_at DESC LIMIT 1`, deviceID).Scan(&payload, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return g, false, nil
	}
	if err != nil {
		return g, false, err
	}
	p := struct {
		Port  *int `json:"port"`
		Hours *int `json:"hours"`
	}{}
	_ = json.Unmarshal(payload, &p)
	g.Port = 5555 // the client's defaults
	hours := 24
	if p.Port != nil {
		g.Port = *p.Port
	}
	if p.Hours != nil {
		hours = *p.Hours
	}
	if g.Port > 0 && hours > 0 {
		g.OffAt = at.Add(time.Duration(hours) * time.Hour)
	}
	return g, true, nil
}

// LastAdbTCPAttempt is the device's most recent adb_tcp command whatever became of it,
// with the device's reply: the switch shows why the last try failed (an old client, or
// firmware without aio-adb-tcp.rc) instead of silently staying off.
func (d *DB) LastAdbTCPAttempt(ctx context.Context, deviceID uuid.UUID) (status, output string, at time.Time, ok bool, err error) {
	err = d.pool.QueryRow(ctx, `
		SELECT s.status, COALESCE(cr.output, ''), s.updated_at
		FROM command_status s JOIN commands c ON c.id = s.command_id
		LEFT JOIN command_results cr ON cr.command_id = s.command_id AND cr.device_id = s.device_id
		WHERE s.device_id = $1 AND c.type = 'adb_tcp'
		ORDER BY c.created_at DESC LIMIT 1`, deviceID).Scan(&status, &output, &at)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", "", at, false, nil
	}
	if err != nil {
		return "", "", at, false, err
	}
	return status, output, at, true, nil
}

// AdbTCPFleetRow is one device's wireless-adb history for the Wireless adb page: its
// last adb_tcp attempt (whatever became of it) and, when it completed, what it set.
type AdbTCPFleetRow struct {
	DeviceID   uuid.UUID
	LastStatus string // delivered / received / completed / failed / …
	LastOutput string // the device's reply (why a try failed)
	LastAt     time.Time
	LastBy     string // username snapshotted on the command
	Port       int    // what the last attempt asked for (0 = off)
	Hours      int
}

// AdbTCPFleet returns the last adb_tcp command per device, for every device that ever
// got one. One query for the page instead of two per device (LastAdbTCP +
// LastAdbTCPAttempt), which would not scale past a handful of rows.
func (d *DB) AdbTCPFleet(ctx context.Context) (map[uuid.UUID]AdbTCPFleetRow, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT ON (s.device_id) s.device_id, s.status, COALESCE(cr.output, ''), s.updated_at,
		       c.created_by, c.payload
		FROM command_status s
		JOIN commands c ON c.id = s.command_id
		LEFT JOIN command_results cr ON cr.command_id = s.command_id AND cr.device_id = s.device_id
		WHERE c.type = 'adb_tcp'
		ORDER BY s.device_id, c.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]AdbTCPFleetRow{}
	for rows.Next() {
		var r AdbTCPFleetRow
		var payload json.RawMessage
		if err := rows.Scan(&r.DeviceID, &r.LastStatus, &r.LastOutput, &r.LastAt, &r.LastBy, &payload); err != nil {
			return nil, err
		}
		p := struct {
			Port  *int `json:"port"`
			Hours *int `json:"hours"`
		}{}
		_ = json.Unmarshal(payload, &p)
		r.Port, r.Hours = 5555, 24
		if p.Port != nil {
			r.Port = *p.Port
		}
		if p.Hours != nil {
			r.Hours = *p.Hours
		}
		out[r.DeviceID] = r
	}
	return out, rows.Err()
}

// AdbTCPLatest is a device's newest adb_tcp command, answered or not, for the connect
// progress (adb_flow.go): AdbTCPFleet only sees commands a device has picked up.
type AdbTCPLatest struct {
	CommandID uuid.UUID
	CreatedAt time.Time
	CreatedBy string
	Port      int    // 0 = turn off
	Hours     int    // 0 = stays on
	Status    string // '' until the device picks it up; delivered / received / completed / failed / expired …
	Output    string
	UpdatedAt time.Time
}

// LatestAdbTCPFor returns the newest adb_tcp command that names each of the devices.
func (d *DB) LatestAdbTCPFor(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]AdbTCPLatest, error) {
	out := map[uuid.UUID]AdbTCPLatest{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT ON (t.target_id) t.target_id, c.id, c.created_at, c.created_by, c.payload,
		       COALESCE(s.status, ''), COALESCE(cr.output, ''), COALESCE(s.updated_at, c.created_at)
		FROM commands c
		JOIN command_targets t ON t.command_id = c.id
		LEFT JOIN command_status s ON s.command_id = c.id AND s.device_id = t.target_id
		LEFT JOIN command_results cr ON cr.command_id = c.id AND cr.device_id = t.target_id
		WHERE c.type = 'adb_tcp' AND c.target_type = 'devices' AND t.target_id = ANY($1)
		ORDER BY t.target_id, c.created_at DESC`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var l AdbTCPLatest
		var payload json.RawMessage
		if err := rows.Scan(&id, &l.CommandID, &l.CreatedAt, &l.CreatedBy, &payload, &l.Status, &l.Output, &l.UpdatedAt); err != nil {
			return nil, err
		}
		p := struct {
			Port  *int `json:"port"`
			Hours *int `json:"hours"`
		}{}
		_ = json.Unmarshal(payload, &p)
		l.Port, l.Hours = 5555, 24
		if p.Port != nil {
			l.Port = *p.Port
		}
		if p.Hours != nil {
			l.Hours = *p.Hours
		}
		out[id] = l
	}
	return out, rows.Err()
}
