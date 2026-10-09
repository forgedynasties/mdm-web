package db

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
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
	ExitedAt    *time.Time // taken out of kiosk on site; stays out until answered
}

// ListKioskStates returns the kiosk state of every device in the fleet (not hidden,
// not retired), keyed by device.
func (d *DB) ListKioskStates(ctx context.Context) (map[uuid.UUID]KioskState, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.id, COALESCE(dc.kiosk_enabled, false), COALESCE(dc.kiosk_package, ''), COALESCE(dc.kiosk_mode, ''),
		       dc.kiosk_rule, COALESCE(dc.kiosk_override, false), COALESCE(dc.offline_exit_enabled, false),
		       dc.updated_at, d.last_seen_at,
		       COALESCE(d.latest_extra->>'kiosk_suspended', '') = 'true', dc.kiosk_exited_at
		FROM devices d LEFT JOIN device_config dc ON dc.device_id = d.id
		WHERE NOT d.hidden AND d.enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]KioskState{}
	for rows.Next() {
		var s KioskState
		if err := rows.Scan(&s.DeviceID, &s.Enabled, &s.Package, &s.Mode, &s.Rule, &s.Override, &s.OfflineExit,
			&s.ConfigAt, &s.LastSeen, &s.Suspended, &s.ExitedAt); err != nil {
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
			    kiosk_exited_at = CASE WHEN EXCLUDED.kiosk_enabled THEN NULL ELSE device_config.kiosk_exited_at END,
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

// DeviceHasOwnKey reports whether a serial has a per-device key (so the shared fleet
// key is no longer accepted for it).
func (d *DB) DeviceHasOwnKey(ctx context.Context, serial string) (bool, error) {
	var has bool
	err := d.pool.QueryRow(ctx, `SELECT COALESCE(device_key_hash, '') <> '' FROM devices WHERE serial_number = $1`, serial).Scan(&has)
	return has, err
}

// DeviceKeyCounts is how many active devices use their own key versus the shared key.
func (d *DB) DeviceKeyCounts(ctx context.Context) (own, shared int, err error) {
	err = d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE COALESCE(device_key_hash, '') <> ''),
		       COUNT(*) FILTER (WHERE COALESCE(device_key_hash, '') = '')
		FROM devices WHERE NOT hidden AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')
		  AND last_seen_at > NOW() - INTERVAL '30 days'`).Scan(&own, &shared)
	return own, shared, err
}

// DeviceKeyClaim is what the server already knows about a device that asks to register
// its own key: the claim must agree with it.
type DeviceKeyClaim struct {
	ID         uuid.UUID
	AgentKind  string
	BuildID    string
	LastBootID string
	HasKey     bool
	KeyHash    string
}

func (d *DB) GetDeviceKeyClaim(ctx context.Context, serial string) (*DeviceKeyClaim, error) {
	var c DeviceKeyClaim
	err := d.pool.QueryRow(ctx, `
		SELECT id, COALESCE(agent_kind, ''), COALESCE(build_id, ''), last_boot_id, COALESCE(device_key_hash, '')
		FROM devices WHERE serial_number = $1`, serial).Scan(&c.ID, &c.AgentKind, &c.BuildID, &c.LastBootID, &c.KeyHash)
	if err != nil {
		return nil, err
	}
	c.HasKey = c.KeyHash != ""
	return &c, nil
}

// ClaimDeviceKey stores a device's own key hash, only if it has none (first claim wins).
// Reports whether it was stored, and whether it followed an admin reset.
func (d *DB) ClaimDeviceKey(ctx context.Context, serial, keyHash string) (stored, afterReset bool, err error) {
	err = d.pool.QueryRow(ctx, `
		UPDATE devices d SET device_key_hash = $2, key_rotated_at = NOW(), key_reset_at = NULL
		FROM (SELECT id, key_reset_at FROM devices WHERE serial_number = $1 FOR UPDATE) old
		WHERE d.id = old.id AND d.device_key_hash IS NULL
		RETURNING old.key_reset_at IS NOT NULL`, serial, keyHash).Scan(&afterReset)
	if err == pgx.ErrNoRows {
		return false, false, nil
	}
	return err == nil, afterReset, err
}

// ResetDeviceKey forgets a firmware device's own key so it can register a new one (a
// device that lost its key after a wipe). Until it does, the shared key works for it
// again. Reports whether a key was cleared.
func (d *DB) ResetDeviceKey(ctx context.Context, serial string) (bool, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE devices SET device_key_hash = NULL, key_reset_at = NOW()
		WHERE serial_number = $1 AND agent_kind = 'firmware' AND device_key_hash IS NOT NULL`, serial)
	return tag.RowsAffected() == 1, err
}

// DeviceKeyState is what the device page says about a device's credential.
type DeviceKeyState struct {
	Own     bool
	Since   *time.Time // when it registered its current key
	ResetAt *time.Time // an admin reset it and it has not registered since
}

func (d *DB) GetDeviceKeyState(ctx context.Context, id uuid.UUID) (DeviceKeyState, error) {
	var s DeviceKeyState
	err := d.pool.QueryRow(ctx, `
		SELECT COALESCE(device_key_hash, '') <> '', key_rotated_at, key_reset_at FROM devices WHERE id = $1`, id).
		Scan(&s.Own, &s.Since, &s.ResetAt)
	return s, err
}

// OverdueKeyResets lists devices reset longer ago than age that have not registered.
func (d *DB) OverdueKeyResets(ctx context.Context, age time.Duration) (map[uuid.UUID]string, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, serial_number FROM devices
		WHERE key_reset_at IS NOT NULL AND device_key_hash IS NULL AND key_reset_at < NOW() - $1::interval`,
		fmt.Sprintf("%d seconds", int(age.Seconds())))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]string{}
	for rows.Next() {
		var id uuid.UUID
		var s string
		if err := rows.Scan(&id, &s); err != nil {
			return nil, err
		}
		out[id] = s
	}
	return out, rows.Err()
}

// KioskExit is a device that was taken out of kiosk on site and not yet answered.
type KioskExit struct {
	DeviceID   uuid.UUID
	Serial     string
	Restaurant string
	At         time.Time
	Package    string     // what it was locked to
	RuleID     *uuid.UUID // the rule that had locked it, nil when locked by hand
	RuleName   string
}

const kioskExitSelect = `
	SELECT d.id, d.serial_number, COALESCE(r.name, ''), dc.kiosk_exited_at, dc.kiosk_exited_package,
	       dc.kiosk_exited_rule, COALESCE(p.name, '')
	FROM device_config dc
	JOIN devices d ON d.id = dc.device_id
	LEFT JOIN restaurants r ON r.id = d.restaurant_id
	LEFT JOIN kiosk_policies p ON p.id = dc.kiosk_exited_rule
	WHERE dc.kiosk_exited_at IS NOT NULL AND NOT d.hidden AND d.enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')`

func scanKioskExits(rows pgx.Rows) ([]KioskExit, error) {
	defer rows.Close()
	var out []KioskExit
	for rows.Next() {
		var e KioskExit
		if err := rows.Scan(&e.DeviceID, &e.Serial, &e.Restaurant, &e.At, &e.Package, &e.RuleID, &e.RuleName); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ListKioskExits returns the devices taken out of kiosk on site, newest first.
func (d *DB) ListKioskExits(ctx context.Context) ([]KioskExit, error) {
	rows, err := d.pool.Query(ctx, kioskExitSelect+` ORDER BY dc.kiosk_exited_at DESC`)
	if err != nil {
		return nil, err
	}
	return scanKioskExits(rows)
}

// GetKioskExit returns the device's unanswered exit, or nil.
func (d *DB) GetKioskExit(ctx context.Context, deviceID uuid.UUID) *KioskExit {
	rows, err := d.pool.Query(ctx, kioskExitSelect+` AND d.id = $1`, deviceID)
	if err != nil {
		return nil
	}
	es, err := scanKioskExits(rows)
	if err != nil || len(es) == 0 {
		return nil
	}
	return &es[0]
}

// MarkKioskExited records that the device was taken out of kiosk on site: kiosk off,
// and what it was locked to (and by which rule) kept for "Lock again". Only a device
// whose kiosk was on is marked. Reports whether it was.
func (d *DB) MarkKioskExited(ctx context.Context, deviceID uuid.UUID, at time.Time) (bool, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE device_config
		   SET kiosk_exited_at = $2, kiosk_exited_package = kiosk_package, kiosk_exited_rule = kiosk_rule,
		       kiosk_enabled = false, updated_at = NOW()
		 WHERE device_id = $1 AND kiosk_enabled`, deviceID, at)
	return tag.RowsAffected() == 1, err
}

// ClearKioskExit answers an exit and returns it (nil if there was none).
func (d *DB) ClearKioskExit(ctx context.Context, deviceID uuid.UUID) (*KioskExit, error) {
	e := d.GetKioskExit(ctx, deviceID)
	if e == nil {
		return nil, nil
	}
	_, err := d.pool.Exec(ctx, `UPDATE device_config SET kiosk_exited_at = NULL, updated_at = NOW() WHERE device_id = $1`, deviceID)
	return e, err
}

// ResolveAnsweredKioskExitAlerts closes "taken out of kiosk" alerts whose device has been
// locked again or left out, whichever way that happened.
func (d *DB) ResolveAnsweredKioskExitAlerts(ctx context.Context) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE alerts a SET status = 'resolved', resolved_at = NOW(), updated_at = NOW()
		WHERE a.type = 'kiosk_exited' AND a.status <> 'resolved'
		  AND NOT EXISTS (SELECT 1 FROM device_config dc WHERE dc.device_id = a.device_id AND dc.kiosk_exited_at IS NOT NULL)`)
	return tag.RowsAffected(), err
}

// NoteOwnKeyIP records the address a device last used its own key from.
func (d *DB) NoteOwnKeyIP(ctx context.Context, serial, ip string) error {
	_, err := d.pool.Exec(ctx, `UPDATE devices SET key_last_ip = $2 WHERE serial_number = $1 AND key_last_ip IS DISTINCT FROM $2`, serial, ip)
	return err
}

// AutoResetDeviceKey forgets the key of a firmware device that has lost it (it keeps
// calling with the shared key) when the calls come from the address it last used its
// own key from, at most once a day. Reports whether it did.
func (d *DB) AutoResetDeviceKey(ctx context.Context, serial, ip string) (bool, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE devices SET device_key_hash = NULL, key_reset_at = NOW(), key_auto_reset_at = NOW()
		WHERE serial_number = $1 AND agent_kind = 'firmware' AND device_key_hash IS NOT NULL
		  AND key_last_ip <> '' AND key_last_ip = $2
		  AND (key_auto_reset_at IS NULL OR key_auto_reset_at < NOW() - interval '24 hours')`, serial, ip)
	return tag.RowsAffected() == 1, err
}

// AutoResetDeviceKeyAnywhere forgets a firmware device's own key on the first shared-key
// call, from any address, with no daily gate. Since 2026-10-08 that is the policy: a
// serial calling with the shared key is almost always one of ours that was just
// reflashed or had its data cleared, and the cost of being wrong (an alert nobody acted
// on) is lower than a device in a restaurant sitting dark until someone finds the Reset
// button. The caller bounds how often this may happen and raises the alert; this only
// does the write. Reports whether a key was actually forgotten.
func (d *DB) AutoResetDeviceKeyAnywhere(ctx context.Context, serial string) (bool, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE devices SET device_key_hash = NULL, key_reset_at = NOW(), key_auto_reset_at = NOW()
		WHERE serial_number = $1 AND agent_kind = 'firmware' AND device_key_hash IS NOT NULL`, serial)
	return tag.RowsAffected() == 1, err
}
