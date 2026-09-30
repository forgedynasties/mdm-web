package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// KioskState is one device's kiosk configuration and what it last reported, for the
// kiosk rule enforcer and the Policies page.
type KioskState struct {
	DeviceID    uuid.UUID
	Enabled     bool
	Package     string
	Mode        string
	Rule        *uuid.UUID // the rule that set this configuration, nil when set by hand
	Override    bool       // changed by hand on the device page while a rule covers it
	OfflineExit bool
	ConfigAt    *time.Time // when the configuration last changed
	LastSeen    time.Time
	Suspended   bool // staff left kiosk on the device with an offline exit code
}

// ListKioskStates returns the kiosk state of every device in the fleet (not hidden,
// not retired), keyed by device.
func (d *DB) ListKioskStates(ctx context.Context) (map[uuid.UUID]KioskState, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.id, COALESCE(dc.kiosk_enabled, false), COALESCE(dc.kiosk_package, ''), COALESCE(dc.kiosk_mode, ''),
		       dc.kiosk_rule, COALESCE(dc.kiosk_override, false), COALESCE(dc.offline_exit_enabled, false),
		       dc.updated_at, d.last_seen_at,
		       COALESCE(d.latest_extra->>'kiosk_suspended', '') = 'true'
		FROM devices d LEFT JOIN device_config dc ON dc.device_id = d.id
		WHERE NOT d.hidden AND d.enrollment_status NOT IN ('retired', 'wiped')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]KioskState{}
	for rows.Next() {
		var s KioskState
		if err := rows.Scan(&s.DeviceID, &s.Enabled, &s.Package, &s.Mode, &s.Rule, &s.Override, &s.OfflineExit,
			&s.ConfigAt, &s.LastSeen, &s.Suspended); err != nil {
			return nil, err
		}
		out[s.DeviceID] = s
	}
	return out, rows.Err()
}

// InstalledPackages reports, for the given packages, which devices have each installed.
func (d *DB) InstalledPackages(ctx context.Context, pkgs []string) (map[uuid.UUID]map[string]bool, error) {
	out := map[uuid.UUID]map[string]bool{}
	if len(pkgs) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `SELECT device_id, package_name FROM device_packages WHERE package_name = ANY($1)`, pkgs)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return out, err
		}
		if out[id] == nil {
			out[id] = map[string]bool{}
		}
		out[id][p] = true
	}
	return out, rows.Err()
}

// SetKioskByRule writes the single-app kiosk a rule wants on a device, recording the
// rule. rule nil with enabled false releases a device a rule had locked.
func (d *DB) SetKioskByRule(ctx context.Context, deviceID uuid.UUID, enabled bool, pkg string, rule *uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_config (device_id, kiosk_enabled, kiosk_package, kiosk_features, kiosk_mode, kiosk_rule, updated_at)
		VALUES ($1, $2, $3, $5, 'app', $4, NOW())
		ON CONFLICT (device_id) DO UPDATE
			SET kiosk_enabled = EXCLUDED.kiosk_enabled,
			    kiosk_package = EXCLUDED.kiosk_package,
			    kiosk_mode    = 'app',
			    kiosk_rule    = EXCLUDED.kiosk_rule,
			    updated_at    = NOW()`, deviceID, enabled, pkg, rule, DefaultKioskFeatures)
	return err
}

// SetKioskOverride marks (or clears) a device's kiosk as changed by hand while a rule
// covers it: the enforcer leaves an overridden device alone.
func (d *DB) SetKioskOverride(ctx context.Context, deviceID uuid.UUID, override bool) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_config (device_id, kiosk_override, updated_at) VALUES ($1, $2, NOW())
		ON CONFLICT (device_id) DO UPDATE SET kiosk_override = EXCLUDED.kiosk_override, updated_at = NOW()`,
		deviceID, override)
	return err
}

// KioskRulesInUse reports whether the enforcer has anything to do: a rule exists, or a
// device is still marked as locked by one (a deleted rule's devices must be released).
func (d *DB) KioskRulesInUse(ctx context.Context) (bool, error) {
	var in bool
	err := d.pool.QueryRow(ctx, `
		SELECT EXISTS (SELECT 1 FROM kiosk_policies) OR EXISTS (SELECT 1 FROM device_config WHERE kiosk_rule IS NOT NULL)`).Scan(&in)
	return in, err
}

// MarkKioskManual records that a device's kiosk was just set by hand: it no longer
// carries a rule's mark, and override says whether a rule covers it (so the enforcer
// leaves it alone until someone chooses to follow the rule again).
func (d *DB) MarkKioskManual(ctx context.Context, deviceID uuid.UUID, override bool) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE device_config SET kiosk_rule = NULL, kiosk_override = $2, updated_at = NOW()
		WHERE device_id = $1`, deviceID, override)
	return err
}

// KioskRuleMark returns which rule set a device's kiosk (nil: none, or set by hand) and
// whether it was overridden by hand while a rule covers it.
func (d *DB) KioskRuleMark(ctx context.Context, deviceID uuid.UUID) (rule *uuid.UUID, override bool) {
	_ = d.pool.QueryRow(ctx, `SELECT kiosk_rule, COALESCE(kiosk_override, false) FROM device_config WHERE device_id = $1`, deviceID).
		Scan(&rule, &override)
	return rule, override
}
