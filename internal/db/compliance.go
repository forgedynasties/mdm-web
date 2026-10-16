package db

import (
	"context"
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// ── Compliance ────────────────────────────────────────────────────────────────
//
// Storage for the compliance engine (dashboard/compliance.go): the fleet baseline, the
// ordered overrides, each device's current result per check and the history of changes.
// What a check means and how it is evaluated lives in the dashboard's registry; this
// layer stores levels, thresholds and states and does not interpret them.

// Compliance levels and states.
const (
	ComplianceRequired    = "required"
	ComplianceRecommended = "recommended"
	ComplianceOff         = "off"

	CompliancePass    = "pass"
	ComplianceFail    = "fail"
	ComplianceUnknown = "unknown"
	ComplianceNA      = "na"
)

// ComplianceSetting is one check's level and threshold, in the baseline or an override.
// An empty Level in an override leaves the level alone; a zero Param leaves the threshold.
type ComplianceSetting struct {
	Level string `json:"level,omitempty"`
	Param int    `json:"param,omitempty"`
	// AutoFix is the baseline's switch for checks that have a fix; nil = the check's
	// default. Overrides do not carry it.
	AutoFix *bool `json:"-"`
}

// ComplianceBaselineRow is the fleet's setting for one check.
type ComplianceBaselineRow struct {
	CheckID   string
	Setting   ComplianceSetting
	UpdatedBy string
	UpdatedAt time.Time
}

// ComplianceOverride is a scoped exception to the baseline.
type ComplianceOverride struct {
	ID        uuid.UUID
	Position  int
	Name      string
	ScopeType string     // venue | group | role | lab
	ScopeID   *uuid.UUID // venue or group
	ScopeKey  string     // role
	Cap       string     // "" | recommended | off
	Checks    map[string]ComplianceSetting
	CreatedBy string
	UpdatedAt time.Time
}

// ComplianceResult is one device's current state for one check.
type ComplianceResult struct {
	DeviceID  uuid.UUID
	CheckID   string
	State     string
	Level     string
	Detail    string
	Since     time.Time
	CheckedAt time.Time
}

// ComplianceResultKey identifies a result.
type ComplianceResultKey struct {
	DeviceID uuid.UUID
	CheckID  string
}

func (d *DB) ComplianceBaseline(ctx context.Context) (map[string]ComplianceBaselineRow, error) {
	rows, err := d.pool.Query(ctx, `SELECT check_id, level, param, auto_fix, updated_by, updated_at FROM compliance_baseline`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]ComplianceBaselineRow{}
	for rows.Next() {
		var r ComplianceBaselineRow
		if err := rows.Scan(&r.CheckID, &r.Setting.Level, &r.Setting.Param, &r.Setting.AutoFix, &r.UpdatedBy, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out[r.CheckID] = r
	}
	return out, rows.Err()
}

// SetComplianceBaseline writes the given checks' settings (others are left as they are).
func (d *DB) SetComplianceBaseline(ctx context.Context, set map[string]ComplianceSetting, by string) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for id, s := range set {
		if _, err := tx.Exec(ctx, `
			INSERT INTO compliance_baseline (check_id, level, param, auto_fix, updated_by, updated_at) VALUES ($1, $2, $3, $4, $5, now())
			ON CONFLICT (check_id) DO UPDATE SET level = EXCLUDED.level, param = EXCLUDED.param, auto_fix = EXCLUDED.auto_fix,
				updated_by = EXCLUDED.updated_by, updated_at = now()`, id, s.Level, s.Param, s.AutoFix, by); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

func (d *DB) ListComplianceOverrides(ctx context.Context) ([]ComplianceOverride, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, position, name, scope_type, scope_id, scope_key, cap, checks, created_by, updated_at
		FROM compliance_overrides ORDER BY position, updated_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ComplianceOverride
	for rows.Next() {
		var o ComplianceOverride
		var checks []byte
		if err := rows.Scan(&o.ID, &o.Position, &o.Name, &o.ScopeType, &o.ScopeID, &o.ScopeKey, &o.Cap, &checks, &o.CreatedBy, &o.UpdatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal(checks, &o.Checks)
		out = append(out, o)
	}
	return out, rows.Err()
}

func (d *DB) GetComplianceOverride(ctx context.Context, id uuid.UUID) (ComplianceOverride, error) {
	var o ComplianceOverride
	var checks []byte
	err := d.pool.QueryRow(ctx, `
		SELECT id, position, name, scope_type, scope_id, scope_key, cap, checks, created_by, updated_at
		FROM compliance_overrides WHERE id = $1`, id).
		Scan(&o.ID, &o.Position, &o.Name, &o.ScopeType, &o.ScopeID, &o.ScopeKey, &o.Cap, &checks, &o.CreatedBy, &o.UpdatedAt)
	if err == nil {
		_ = json.Unmarshal(checks, &o.Checks)
	}
	return o, err
}

// SaveComplianceOverride inserts (ID == uuid.Nil, placed last) or updates an override.
func (d *DB) SaveComplianceOverride(ctx context.Context, o ComplianceOverride) (uuid.UUID, error) {
	checks, _ := json.Marshal(o.Checks)
	if o.Checks == nil {
		checks = []byte("{}")
	}
	if o.ID == uuid.Nil {
		var id uuid.UUID
		err := d.pool.QueryRow(ctx, `
			INSERT INTO compliance_overrides (position, name, scope_type, scope_id, scope_key, cap, checks, created_by)
			VALUES ((SELECT COALESCE(MAX(position), 0) + 1 FROM compliance_overrides), $1, $2, $3, $4, $5, $6, $7)
			RETURNING id`, o.Name, o.ScopeType, o.ScopeID, o.ScopeKey, o.Cap, checks, o.CreatedBy).Scan(&id)
		return id, err
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE compliance_overrides SET name = $2, scope_type = $3, scope_id = $4, scope_key = $5, cap = $6,
			checks = $7, updated_at = now() WHERE id = $1`, o.ID, o.Name, o.ScopeType, o.ScopeID, o.ScopeKey, o.Cap, checks)
	return o.ID, err
}

func (d *DB) DeleteComplianceOverride(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM compliance_overrides WHERE id = $1`, id)
	return err
}

// MoveComplianceOverride swaps an override with its neighbour (dir -1 up, +1 down) and
// renumbers the list 1..n so positions stay dense.
func (d *DB) MoveComplianceOverride(ctx context.Context, id uuid.UUID, dir int) error {
	list, err := d.ListComplianceOverrides(ctx)
	if err != nil {
		return err
	}
	i := -1
	for k := range list {
		if list[k].ID == id {
			i = k
		}
	}
	j := i + dir
	if i < 0 || j < 0 || j >= len(list) {
		return nil
	}
	list[i], list[j] = list[j], list[i]
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for k, o := range list {
		if _, err := tx.Exec(ctx, `UPDATE compliance_overrides SET position = $2 WHERE id = $1`, o.ID, k+1); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// ListComplianceResults returns every stored result.
func (d *DB) ListComplianceResults(ctx context.Context) (map[ComplianceResultKey]ComplianceResult, error) {
	rows, err := d.pool.Query(ctx, `SELECT device_id, check_id, state, level, detail, since, checked_at FROM compliance_results`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[ComplianceResultKey]ComplianceResult{}
	for rows.Next() {
		var r ComplianceResult
		if err := rows.Scan(&r.DeviceID, &r.CheckID, &r.State, &r.Level, &r.Detail, &r.Since, &r.CheckedAt); err != nil {
			return nil, err
		}
		out[ComplianceResultKey{r.DeviceID, r.CheckID}] = r
	}
	return out, rows.Err()
}

// DeviceComplianceResults returns one device's results.
func (d *DB) DeviceComplianceResults(ctx context.Context, deviceID uuid.UUID) ([]ComplianceResult, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT device_id, check_id, state, level, detail, since, checked_at FROM compliance_results WHERE device_id = $1`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ComplianceResult
	for rows.Next() {
		var r ComplianceResult
		if err := rows.Scan(&r.DeviceID, &r.CheckID, &r.State, &r.Level, &r.Detail, &r.Since, &r.CheckedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ComplianceWrite is what one evaluation pass changes: results to upsert (with the state
// they had, "" for new, so an event can be written when the state moved) and results to
// remove (a check turned off for the device, or the device left the fleet).
type ComplianceWrite struct {
	Upsert []ComplianceUpsert
	Remove []ComplianceResultKey
}

type ComplianceUpsert struct {
	ComplianceResult
	PrevState string // "" for a first result
}

// ApplyCompliance writes one pass in a single transaction.
func (d *DB) ApplyCompliance(ctx context.Context, w ComplianceWrite) error {
	if len(w.Upsert) == 0 && len(w.Remove) == 0 {
		return nil
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, u := range w.Upsert {
		moved := u.PrevState != u.State
		if _, err := tx.Exec(ctx, `
			INSERT INTO compliance_results (device_id, check_id, state, level, detail, since, checked_at)
			VALUES ($1, $2, $3, $4, $5, now(), now())
			ON CONFLICT (device_id, check_id) DO UPDATE SET state = EXCLUDED.state, level = EXCLUDED.level,
				detail = EXCLUDED.detail, checked_at = now(),
				since = CASE WHEN compliance_results.state = EXCLUDED.state THEN compliance_results.since ELSE now() END`,
			u.DeviceID, u.CheckID, u.State, u.Level, u.Detail); err != nil {
			return err
		}
		if moved {
			if _, err := tx.Exec(ctx, `
				INSERT INTO compliance_events (device_id, check_id, from_state, to_state, level, detail) VALUES ($1, $2, $3, $4, $5, $6)`,
				u.DeviceID, u.CheckID, u.PrevState, u.State, u.Level, u.Detail); err != nil {
				return err
			}
		}
	}
	for _, k := range w.Remove {
		if _, err := tx.Exec(ctx, `DELETE FROM compliance_results WHERE device_id = $1 AND check_id = $2`, k.DeviceID, k.CheckID); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// OwnKeyDevices is the set of devices that registered their own key.
func (d *DB) OwnKeyDevices(ctx context.Context) (map[uuid.UUID]bool, error) {
	rows, err := d.pool.Query(ctx, `SELECT id FROM devices WHERE device_key_hash IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]bool{}
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// PruneComplianceEvents drops history older than keep.
func (d *DB) PruneComplianceEvents(ctx context.Context, keep time.Duration) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM compliance_events WHERE at < now() - make_interval(secs => $1)`, keep.Seconds())
	return err
}

// SeedComplianceDefaults adds, once, the override the plan starts from: devices in a lab
// session are held only to recommendations. (Devices with no venue are not scored at all
// since 15 Oct, so the "Not at a venue" override that used to be seeded is removed.)
// Deleting the seeded override later is respected.
func (d *DB) SeedComplianceDefaults(ctx context.Context) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	tag, err := tx.Exec(ctx, `INSERT INTO app_flags (flag) VALUES ('compliance_seed_v1') ON CONFLICT DO NOTHING`)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO compliance_overrides (position, name, scope_type, cap, created_by)
			VALUES (1, 'In a lab session', 'lab', 'recommended', 'system')`); err != nil {
			return err
		}
	}
	tag, err = tx.Exec(ctx, `INSERT INTO app_flags (flag) VALUES ('compliance_venue_only_v1') ON CONFLICT DO NOTHING`)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 1 {
		if _, err := tx.Exec(ctx, `DELETE FROM compliance_overrides WHERE scope_type = 'unassigned'`); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM compliance_baseline WHERE check_id = 'has_venue'`); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// AdbTcpLast is a device's most recent adb_tcp command: what it asked for and who sent it.
type AdbTcpLast struct {
	Port, Hours int
	By          string
	At          time.Time
}

// LastAdbTcpCommands returns the latest adb_tcp command for each of the given devices.
func (d *DB) LastAdbTcpCommands(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]AdbTcpLast, error) {
	out := map[uuid.UUID]AdbTcpLast{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT ON (t.target_id) t.target_id, COALESCE((c.payload->>'port')::int, 0), COALESCE((c.payload->>'hours')::int, 0),
		       COALESCE(c.created_by, ''), c.created_at
		FROM commands c JOIN command_targets t ON t.command_id = c.id
		WHERE c.type = 'adb_tcp' AND c.target_type = 'devices' AND t.target_id = ANY($1)
		ORDER BY t.target_id, c.created_at DESC`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var l AdbTcpLast
		if err := rows.Scan(&id, &l.Port, &l.Hours, &l.By, &l.At); err != nil {
			return nil, err
		}
		out[id] = l
	}
	return out, rows.Err()
}
