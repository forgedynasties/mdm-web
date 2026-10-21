package db

import (
	"context"
	"fmt"
	"time"
)

// RoleMoveDay is one day of devices of a role moving to a new build: firmware installs
// finished (an OTA reached "installed") and client self-updates finished.
type RoleMoveDay struct {
	Day      time.Time
	Firmware int
	Client   int
}

// RoleMovesDaily returns the last `days` days for a role (device class), oldest first,
// every day present. It feeds the Lineup rail's bars and the cards' "this week".
func (d *DB) RoleMovesDaily(ctx context.Context, class string, days int) ([]RoleMoveDay, error) {
	if days < 1 {
		days = 7
	}
	rows, err := d.pool.Query(ctx, `
		WITH role_dev AS (
			SELECT id FROM devices WHERE NOT hidden AND `+derivedClassSQL()+` = $1
		), fw AS (
			SELECT (ud.completed_at AT TIME ZONE 'UTC')::date AS day, COUNT(*) AS n
			FROM update_devices ud JOIN role_dev r ON r.id = ud.device_id
			WHERE ud.status = 'installed' AND ud.completed_at >= CURRENT_DATE - ($2::int - 1)
			GROUP BY 1
		), cl AS (
			SELECT (cs.updated_at AT TIME ZONE 'UTC')::date AS day, COUNT(*) AS n
			FROM command_status cs
			JOIN commands c ON c.id = cs.command_id
			JOIN role_dev r ON r.id = cs.device_id
			WHERE c.type = 'app_update' AND cs.status IN ('completed', 'installed')
			  AND cs.updated_at >= CURRENT_DATE - ($2::int - 1)
			GROUP BY 1
		)
		SELECT g.day::date, COALESCE(fw.n, 0), COALESCE(cl.n, 0)
		FROM generate_series(CURRENT_DATE - ($2::int - 1), CURRENT_DATE, interval '1 day') AS g(day)
		LEFT JOIN fw ON fw.day = g.day::date
		LEFT JOIN cl ON cl.day = g.day::date
		ORDER BY 1`, class, days)
	if err != nil {
		return nil, fmt.Errorf("role moves: %w", err)
	}
	defer rows.Close()
	var out []RoleMoveDay
	for rows.Next() {
		var m RoleMoveDay
		if err := rows.Scan(&m.Day, &m.Firmware, &m.Client); err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}

// FirmwareProdChange is one time a model's prod firmware was set, from the audit log
// ("release.firmware_prod": target = model, detail = version).
type FirmwareProdChange struct {
	Model, Version, By string
	At                 time.Time
}

// FirmwareProdHistory lists every prod firmware change, oldest first, by model.
func (d *DB) FirmwareProdHistory(ctx context.Context) (map[string][]FirmwareProdChange, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT target, detail, actor, created_at FROM audit_log
		WHERE action = 'release.firmware_prod'
		ORDER BY created_at`)
	if err != nil {
		return nil, fmt.Errorf("firmware prod history: %w", err)
	}
	defer rows.Close()
	out := map[string][]FirmwareProdChange{}
	for rows.Next() {
		var p FirmwareProdChange
		if err := rows.Scan(&p.Model, &p.Version, &p.By, &p.At); err != nil {
			return nil, err
		}
		out[p.Model] = append(out[p.Model], p)
	}
	return out, rows.Err()
}
