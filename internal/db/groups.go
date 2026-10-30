package db

import (
	"context"

	"github.com/google/uuid"
)

// ── Groups ────────────────────────────────────────────────────────────────────

func (d *DB) CreateGroup(ctx context.Context, name string) (*Group, error) {
	var g Group
	err := d.pool.QueryRow(ctx, `
		INSERT INTO groups (name) VALUES ($1)
		RETURNING id, name, created_at
	`, name).Scan(&g.ID, &g.Name, &g.CreatedAt)
	return &g, err
}

// RenameGroup changes a group's name in place — a group has no other editable
// fields, so this is the whole "edit" for a group (used by the fleet toolbar).
func (d *DB) RenameGroup(ctx context.Context, id uuid.UUID, name string) error {
	_, err := d.pool.Exec(ctx, `UPDATE groups SET name = $2 WHERE id = $1`, id, name)
	return err
}

// hiddenGroupsSchema: a group can be hidden (user, 30 Oct 2026: "hide groups so only super
// admins can see them, and not in the side rail"). ListGroups leaves hidden groups out, so
// every list and picker drops them; the super admin's pages ask ListAllGroups.
const hiddenGroupsSchema = `ALTER TABLE groups ADD COLUMN IF NOT EXISTS hidden BOOLEAN NOT NULL DEFAULT false;`

// ListGroups lists the groups that are not hidden.
func (d *DB) ListGroups(ctx context.Context) ([]Group, error) { return d.listGroups(ctx, false) }

// ListAllGroups lists every group, hidden ones included (Hidden set).
func (d *DB) ListAllGroups(ctx context.Context) ([]Group, error) { return d.listGroups(ctx, true) }

func (d *DB) listGroups(ctx context.Context, withHidden bool) ([]Group, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT g.id, g.name, g.created_at, g.hidden, COUNT(dg.device_id) FILTER (WHERE NOT COALESCE(dv.hidden, false)) AS device_count
		FROM groups g
		LEFT JOIN device_groups dg ON dg.group_id = g.id
		LEFT JOIN devices dv ON dv.id = dg.device_id
		WHERE $1 OR NOT g.hidden
		GROUP BY g.id, g.name, g.created_at, g.hidden
		ORDER BY g.name
	`, withHidden)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.CreatedAt, &g.Hidden, &g.DeviceCount); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

// SetGroupHidden hides or shows a group.
func (d *DB) SetGroupHidden(ctx context.Context, id uuid.UUID, hidden bool) error {
	_, err := d.pool.Exec(ctx, `UPDATE groups SET hidden = $2 WHERE id = $1`, id, hidden)
	return err
}

func (d *DB) GetGroup(ctx context.Context, id uuid.UUID) (*Group, error) {
	var g Group
	err := d.pool.QueryRow(ctx, `
		SELECT g.id, g.name, g.created_at, g.hidden, COUNT(dg.device_id) FILTER (WHERE NOT COALESCE(dv.hidden, false)) AS device_count
		FROM groups g
		LEFT JOIN device_groups dg ON dg.group_id = g.id
		LEFT JOIN devices dv ON dv.id = dg.device_id
		WHERE g.id = $1
		GROUP BY g.id, g.name, g.created_at, g.hidden
	`, id).Scan(&g.ID, &g.Name, &g.CreatedAt, &g.Hidden, &g.DeviceCount)
	if err != nil {
		return nil, err
	}
	return &g, nil
}

func (d *DB) DeleteGroup(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM groups WHERE id = $1`, id)
	return err
}

func (d *DB) AddDeviceToGroup(ctx context.Context, serial string, groupID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_groups (device_id, group_id)
		SELECT id, $2 FROM devices WHERE serial_number = $1
		ON CONFLICT DO NOTHING
	`, serial, groupID)
	return err
}

// AddDevicesToGroup tags every named device, ignoring serials already in the group,
// and returns how many rows it actually added. The count is what the dashboard
// reports back: a pasted list whose serials are not in the fleet adds nothing, and
// the operator has to be told that instead of seeing a silent success.
//
// Serials are matched case-insensitively — a serial pasted in the wrong case is the
// same device, and an exact-match-only lookup made it look like an unknown one.
func (d *DB) AddDevicesToGroup(ctx context.Context, serials []string, groupID uuid.UUID) (int, error) {
	if len(serials) == 0 {
		return 0, nil
	}
	tag, err := d.pool.Exec(ctx, `
		INSERT INTO device_groups (device_id, group_id)
		SELECT d.id, $2 FROM devices d
		WHERE upper(d.serial_number) = ANY(SELECT upper(s) FROM unnest($1::text[]) s)
		ON CONFLICT DO NOTHING
	`, serials, groupID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (d *DB) RemoveDeviceFromGroup(ctx context.Context, serial string, groupID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `
		DELETE FROM device_groups
		WHERE device_id = (SELECT id FROM devices WHERE serial_number = $1)
		AND group_id = $2
	`, serial, groupID)
	return err
}

// RemoveDevicesFromGroup removes many devices from a group in one statement, so the
// group members list can offer a bulk "Remove from group" instead of one-by-one.
func (d *DB) RemoveDevicesFromGroup(ctx context.Context, serials []string, groupID uuid.UUID) (int, error) {
	if len(serials) == 0 {
		return 0, nil
	}
	tag, err := d.pool.Exec(ctx, `
		DELETE FROM device_groups
		WHERE group_id = $2
		AND device_id IN (
			SELECT id FROM devices
			WHERE upper(serial_number) = ANY(SELECT upper(s) FROM unnest($1::text[]) s)
		)
	`, serials, groupID)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

// ListDeviceGroups returns the groups a single device belongs to (for the device
// detail page's placement block).
func (d *DB) ListDeviceGroups(ctx context.Context, deviceID uuid.UUID) ([]Group, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT g.id, g.name, g.created_at, g.hidden
		FROM groups g JOIN device_groups dg ON dg.group_id = g.id
		WHERE dg.device_id = $1
		ORDER BY g.name`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.CreatedAt, &g.Hidden); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// GroupNamesByDevice returns every device's group names (sorted by name), for
// report rows that print a joined group list without a per-device query.
func (d *DB) GroupNamesByDevice(ctx context.Context) (map[uuid.UUID][]string, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT dg.device_id, g.name
		FROM device_groups dg JOIN groups g ON g.id = dg.group_id
		ORDER BY g.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID][]string)
	for rows.Next() {
		var id uuid.UUID
		var name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = append(out[id], name)
	}
	return out, rows.Err()
}

func (d *DB) ListGroupDevices(ctx context.Context, groupID uuid.UUID) ([]Device, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT
			d.id, d.serial_number, d.build_id, d.last_seen_at, d.created_at,
			d.latest_battery_pct AS battery_pct,
			d.poll_interval_ms,
			COALESCE(dc.kiosk_enabled, false),
			COALESCE(dc.kiosk_package, '')
		FROM devices d
		JOIN device_groups dg ON dg.device_id = d.id
		LEFT JOIN device_config dc ON dc.device_id = d.id
		WHERE dg.group_id = $1 AND NOT d.hidden
		ORDER BY d.serial_number
	`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []Device
	for rows.Next() {
		var dev Device
		if err := rows.Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct, &dev.PollIntervalMs, &dev.KioskEnabled, &dev.KioskPackage); err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	return devices, rows.Err()
}
