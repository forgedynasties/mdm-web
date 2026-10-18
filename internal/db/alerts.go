package db

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ── Alerts ────────────────────────────────────────────────────────────────────

// AlertRule is a rule definition the evaluator checks each housekeeping pass.
type AlertRule struct {
	ID           uuid.UUID       `json:"id"`
	Type         string          `json:"type"`
	Name         string          `json:"name"`
	Enabled      bool            `json:"enabled"`
	Params       json.RawMessage `json:"params"`
	ScopeType    string          `json:"scope_type"`
	ScopeID      *uuid.UUID      `json:"scope_id"`
	ActiveWindow string          `json:"active_window"` // "" / always | service | overnight
	DeployedOnly bool            `json:"deployed_only"` // fire only for devices assigned to a restaurant
	CreatedAt    time.Time       `json:"created_at"`
}

// Alert is a fired alert instance. Serial is joined from devices for display.
type Alert struct {
	ID             uuid.UUID       `json:"id"`
	RuleID         *uuid.UUID      `json:"rule_id"`
	Type           string          `json:"type"`
	DeviceID       *uuid.UUID      `json:"device_id"`
	Serial         string          `json:"serial"`
	RestaurantName string          `json:"restaurant_name,omitempty"`
	Severity       string          `json:"severity"`
	Status         string          `json:"status"`
	Summary        string          `json:"summary"`
	Detail         json.RawMessage `json:"detail"`
	Occurrences    int             `json:"occurrences"`
	MutedUntil     *time.Time      `json:"muted_until,omitempty"`
	FiredAt        time.Time       `json:"fired_at"`
	LastSeenAt     time.Time       `json:"last_seen_at"`
	ResolvedAt     *time.Time      `json:"resolved_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
	// Problem workflow (set only by the active-alert lists).
	Assignee    string `json:"assignee,omitempty"`
	Note        string `json:"note,omitempty"`
	DeviceClass  string `json:"device_class,omitempty"`
	RestaurantID string `json:"restaurant_id,omitempty"`
}

// AlertSummary holds dashboard headline counts. Severity counts cover only
// non-resolved alerts (what still needs attention); Total/Resolved cover all rows.
type AlertSummary struct {
	Total        int `json:"total"`
	Open         int `json:"open"`
	Acknowledged int `json:"acknowledged"`
	Resolved     int `json:"resolved"`
	Critical     int `json:"critical"`
	Warning      int `json:"warning"`
	Info         int `json:"info"`
}

// defaultAlertRules are seeded once (per type) by EnsureDefaultRules so alerting
// works out of the box; admins can edit/disable/delete them afterward.
var defaultAlertRules = []struct {
	Type, Name, Params, ActiveWindow string
	Enabled                          bool
}{
	// Lifecycle.
	{"new_device", "New device onboarded", `{}`, "always", true},
	// Daily-tier rules.
	// overheating is WLC-aware: temp_c off the pad, temp_c_wlc while wireless-charging.
	{"overheating", "Device overheating", `{"temp_c":52,"temp_c_wlc":65}`, "always", true},
	// Memory pressure gives the report a configurable RAM cutoff; off by default.
	{"memory_pressure", "Memory pressure", `{"ram_pct":85}`, "always", false},
	{"storage_filling", "Storage filling fast", `{"drop_gb":0.2}`, "always", true},
	{"wlc_dead", "Wireless charger not functional all day", `{}`, "always", true},
	// Recent-tier rules (T7 matrix).
	{"offline", "Device offline", `{"offline_minutes":5}`, "always", true},
	{"storage_low", "Storage critically low", `{"free_gb":1}`, "always", true},
	{"storage_warning", "Storage low", `{"free_gb":14}`, "always", true},
	// Off by default: the T7 lives in this band every day (30 Sep alert review).
	{"temp_elevated", "Temperature elevated", `{"temp_min":38,"temp_max":45}`, "always", false},
	{"memory_low", "Memory low (available)", `{"avail_mb":400}`, "always", true},
	{"wifi_weak", "Weak Wi-Fi signal", `{"rssi_dbm":-75,"sustain_min":10}`, "always", true},
	// Off by default: a tablet on its charger reaching 100% overnight is normal.
	{"battery_high_night", "Battery high overnight", `{"soc_pct":60}`, "overnight", false},
	{"wlc_continuous", "Continuous wireless charging", `{"sustain_min":60}`, "always", true},
	{"charger_flapping", "Charger flapping / faulty", `{"window_min":5,"flaps_per_min":10}`, "always", true},
	{"battery_low", "Battery low during peak", `{"soc_pct":20}`, "peak", true},
	{"offline_peak", "Offline during peak", `{"offline_minutes":5}`, "peak", true},
	// Client-telemetry rules (need the new charger/wifi/crash fields the client reports).
	{"wifi_unstable", "Frequent Wi-Fi disconnects", `{"disconnects":3}`, "always", true},
	{"device_crash", "Device crash / ANR", `{"window_min":15}`, "always", true},
	{"slow_charge_night", "Slow overnight charging", `{"max_gain_pct":15,"window_hours":2,"below_pct":80}`, "overnight", true},
	// 17 Oct: the host app gone from the screen during service (firmware client 1.11.0+).
	{"host_app_away", "Host app not on screen", `{"minutes":5}`, "service", true},
	// Off by default (15 Oct): compliance alerts wait for a few weeks of results.
	{"compliance_failed", "Fails a required compliance check", `{"minutes":30}`, "always", false},
	// A device in lost mode reporting in. Every device, assigned to a venue or not.
	{"lost_device_seen", "Lost device checked in", `{}`, "always", true},
	// 18 Oct, lost-device reporting: a tablet dropping off the Wi-Fi off its charger during
	// service (A), a lost device heard by its neighbours (B), one that locked itself (D).
	{"left_venue", "Left the venue", `{"offline_minutes":3}`, "service", true},
	{"lost_device_heard", "Lost device heard nearby", `{"minutes":10}`, "always", true},
	{"self_locked", "Locked itself after leaving", `{}`, "always", true},
}

// defaultRulesFleetWide are default rules seeded with deployed_only off: a lost device
// matters most when it is not at a venue at all.
var defaultRulesFleetWide = map[string]bool{"lost_device_seen": true, "lost_device_heard": true, "self_locked": true}

// EnsureDefaultRules inserts each default rule only if no rule of that type exists.
func (d *DB) EnsureDefaultRules(ctx context.Context) error {
	for _, r := range defaultAlertRules {
		if _, err := d.pool.Exec(ctx, `
			INSERT INTO alert_rules (type, name, enabled, params, active_window, deployed_only)
			SELECT $1, $2, $3, $4::jsonb, $5, $6
			WHERE NOT EXISTS (SELECT 1 FROM alert_rules WHERE type = $1)
		`, r.Type, r.Name, r.Enabled, r.Params, r.ActiveWindow, !defaultRulesFleetWide[r.Type]); err != nil {
			return err
		}
	}
	return nil
}

// EnabledRuleIDByType returns the id of the enabled alert rule of the given
// type. ok is false when no such rule exists or it is disabled, so callers can
// skip firing the alert when the admin has turned the rule off.
func (d *DB) EnabledRuleIDByType(ctx context.Context, typ string) (id uuid.UUID, ok bool, err error) {
	err = d.pool.QueryRow(ctx, `
		SELECT id FROM alert_rules WHERE type = $1 AND enabled LIMIT 1
	`, typ).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	return id, true, nil
}

// ListAlertRules returns alert rules, optionally only the enabled ones.
func (d *DB) ListAlertRules(ctx context.Context, onlyEnabled bool) ([]AlertRule, error) {
	q := `SELECT id, type, name, enabled, params, scope_type, scope_id, active_window, deployed_only, created_at FROM alert_rules`
	if onlyEnabled {
		q += ` WHERE enabled`
	}
	q += ` ORDER BY type`
	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertRule
	for rows.Next() {
		var r AlertRule
		if err := rows.Scan(&r.ID, &r.Type, &r.Name, &r.Enabled, &r.Params, &r.ScopeType, &r.ScopeID, &r.ActiveWindow, &r.DeployedOnly, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateAlertRule sets a rule's enabled flag and threshold params. activeWindow is
// applied only when non-empty (so updating a non-windowed rule leaves it unchanged);
// pass "always"/"service"/"overnight" to set it.
func (d *DB) UpdateAlertRule(ctx context.Context, id uuid.UUID, enabled bool, params json.RawMessage, activeWindow string, deployedOnly bool) error {
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE alert_rules
		SET enabled = $2, params = $3::jsonb,
		    active_window = COALESCE(NULLIF($4, ''), active_window),
		    deployed_only = $5
		WHERE id = $1
	`, id, enabled, params, activeWindow, deployedOnly)
	return err
}

// CreateAlertIfAbsent inserts a new alert unless a non-resolved one already exists
// for (type, device). Returns true only when a row was actually created, so callers
// broadcast/notify exactly once per occurrence.
func (d *DB) CreateAlertIfAbsent(ctx context.Context, ruleID *uuid.UUID, typ string, deviceID uuid.UUID, severity, summary string, detail any) (bool, error) {
	u, err := d.upsertAlert(ctx, ruleID, typ, deviceID, severity, summary, detail)
	return u.Inserted, err
}

// alertUpsert is what one pass of the evaluator did to a (type, device) alert.
type alertUpsert struct {
	ID        uuid.UUID
	Inserted  bool // a new episode: notify
	Escalated bool // already open, and just became critical: notify as well
}

// upsertAlert is CreateAlertIfAbsent that also reports the row id and whether an open
// alert was escalated to critical by this pass.
func (d *DB) upsertAlert(ctx context.Context, ruleID *uuid.UUID, typ string, deviceID uuid.UUID, severity, summary string, detail any) (alertUpsert, error) {
	// A device in a lab session raises no restaurant alerts (role model, 14 Oct): it is
	// unlocked, off prod and being worked on by someone who already knows.
	var inLab bool
	if err := d.pool.QueryRow(ctx, `SELECT EXISTS (SELECT 1 FROM device_lab WHERE device_id = $1 AND until > now())`, deviceID).Scan(&inLab); err == nil && inLab {
		return alertUpsert{}, nil
	}
	detailJSON := []byte("{}")
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			return alertUpsert{}, err
		}
		detailJSON = b
	}
	// Episode folding, in three cases:
	//
	//  1. The condition is still open — refresh summary/detail/last-seen, clear any
	//     pending cleared_at (a dip that came back is the same episode), and do NOT bump
	//     occurrences: the evaluator runs every minute, so counting passes produced
	//     meaningless "×4000" badges for one ongoing outage.
	//  2. It resolved recently (within alertCooldown) and is back — REOPEN that row and
	//     bump occurrences, instead of inserting a new one. This is what turns "95 rows
	//     for device 746" into "746 overheated ×95", and it is why resolving everything
	//     no longer refills the list a minute later.
	//  3. Nothing recent — a genuinely new episode, so insert and let the caller notify.
	//
	// A muted (type, device) is skipped entirely: flap detection and "Clear all" both
	// snooze rather than delete, and a snoozed condition must stay quiet.
	var inserted bool
	var id uuid.UUID
	var prevSev string
	err := d.pool.QueryRow(ctx, `
		WITH prev AS (
			SELECT severity FROM alerts
			WHERE type = $2 AND device_id = $3 AND status <> 'resolved'
		), recent AS (
			SELECT id, status, occurrences, muted_until
			FROM alerts
			WHERE type = $2 AND device_id = $3
			ORDER BY fired_at DESC
			LIMIT 1
		), muted AS (
			SELECT 1 FROM recent WHERE muted_until IS NOT NULL AND muted_until > NOW()
		), reopened AS (
			UPDATE alerts a SET
				status       = 'open',
				resolved_at  = NULL,
				cleared_at   = NULL,
				occurrences  = a.occurrences + 1,
				severity     = $4,
				summary      = $5,
				detail       = $6::jsonb,
				last_seen_at = NOW(),
				updated_at   = NOW()
			FROM recent r
			WHERE a.id = r.id
			  AND r.status = 'resolved'
			  AND NOT EXISTS (SELECT 1 FROM muted)
			  AND a.resolved_at > NOW() - make_interval(mins => $7)
			RETURNING a.id
		), fresh AS (
			INSERT INTO alerts (rule_id, type, device_id, severity, summary, detail)
			SELECT $1, $2, $3, $4, $5, $6::jsonb
			WHERE NOT EXISTS (SELECT 1 FROM reopened)
			  AND NOT EXISTS (SELECT 1 FROM muted)
			ON CONFLICT (type, device_id) WHERE status <> 'resolved'
			DO UPDATE SET last_seen_at = NOW(),
			              cleared_at   = NULL,
			              severity     = EXCLUDED.severity,
			              summary      = EXCLUDED.summary,
			              detail       = EXCLUDED.detail,
			              updated_at    = NOW()
			RETURNING id, (xmax = 0) AS ins
		)
		SELECT COALESCE((SELECT ins FROM fresh), false),
		       COALESCE((SELECT id FROM fresh), (SELECT id FROM reopened), '00000000-0000-0000-0000-000000000000'::uuid),
		       COALESCE((SELECT severity FROM prev), '')
	`, ruleID, typ, deviceID, severity, summary, detailJSON, alertCooldownMin).Scan(&inserted, &id, &prevSev)
	if err != nil {
		return alertUpsert{}, err
	}
	u := alertUpsert{ID: id, Inserted: inserted,
		Escalated: !inserted && prevSev != "" && prevSev != "critical" && severity == "critical"}
	if err := d.muteFlappingAlert(ctx, typ, deviceID); err != nil {
		return u, err
	}
	return u, nil
}

// alertCooldownMin is how long after resolving the same condition counts as the same
// episode rather than a new one.
const alertCooldownMin = 30

// alertFlapEpisodes is how many episodes in a day make a condition a flapper: the device
// is telling us it sits on the threshold, not that something new keeps happening.
const alertFlapEpisodes = 5

// muteFlappingAlert snoozes a condition that has come back too many times in a day. The
// row stays open and carries its count, so the list reads "746 overheated ×12" on one line
// instead of twelve, and nothing more is raised for it until an operator resolves it.
func (d *DB) muteFlappingAlert(ctx context.Context, typ string, deviceID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE alerts SET
			muted_until = NOW() + INTERVAL '24 hours',
			detail      = detail || jsonb_build_object('flapping', true, 'episodes', occurrences),
			updated_at  = NOW()
		WHERE type = $1 AND device_id = $2 AND status <> 'resolved'
		  AND occurrences >= $3
		  AND muted_until IS NULL
		  AND fired_at > NOW() - INTERVAL '24 hours'
	`, typ, deviceID, alertFlapEpisodes)
	return err
}

// alertClearMin is how long a condition must stay clear before its alert resolves. The
// readings that flooded the list sat on a threshold and crossed it repeatedly; without a
// clear window each crossing closed one alert and opened another.
const alertClearMin = 10

// resolveClearedAlerts closes alerts whose condition has stayed clear for the whole window.
// Muted rows are left open deliberately: a flapping condition waits for an operator rather
// than quietly resolving itself and starting over.
func (d *DB) resolveClearedAlerts(ctx context.Context) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE alerts SET status = 'resolved', resolved_at = NOW(), updated_at = NOW()
		WHERE status <> 'resolved'
		  AND cleared_at IS NOT NULL
		  AND cleared_at < NOW() - make_interval(mins => $1::int)
		  AND (muted_until IS NULL OR muted_until < NOW())
	`, alertClearMin)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ResolveOpenAlert resolves any non-resolved alert for (type, device); used when a
// condition clears. Returns the number of alerts resolved.
func (d *DB) ResolveOpenAlert(ctx context.Context, typ string, deviceID uuid.UUID) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE alerts SET status = 'resolved', resolved_at = NOW(), updated_at = NOW()
		WHERE type = $1 AND device_id = $2 AND status <> 'resolved'
	`, typ, deviceID)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ResolveOpenAlertsOfType resolves every open alert of one type, on any device.
func (d *DB) ResolveOpenAlertsOfType(ctx context.Context, typ string) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE alerts SET status = 'resolved', resolved_at = NOW(), updated_at = NOW()
		WHERE type = $1 AND status <> 'resolved'
	`, typ)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ListAlerts returns alerts (newest first), optionally filtered by status, with the
// device serial joined. limit <= 0 means 200.
func (d *DB) ListAlerts(ctx context.Context, status string, limit int) ([]Alert, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.pool.Query(ctx, `
		SELECT a.id, a.rule_id, a.type, a.device_id, COALESCE(d.serial_number, ''),
		       COALESCE(r.name, ''), a.severity, a.status, a.summary, a.detail,
		       a.occurrences, a.muted_until, a.fired_at, a.last_seen_at, a.resolved_at, a.updated_at
		FROM alerts a
		LEFT JOIN devices d ON d.id = a.device_id
		LEFT JOIN restaurants r ON r.id = d.restaurant_id
		WHERE ($1 = '' OR a.status = $1)
		ORDER BY a.fired_at DESC
		LIMIT $2
	`, status, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.RuleID, &a.Type, &a.DeviceID, &a.Serial,
			&a.RestaurantName, &a.Severity, &a.Status, &a.Summary, &a.Detail,
			&a.Occurrences, &a.MutedUntil, &a.FiredAt, &a.LastSeenAt, &a.ResolvedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListActiveAlerts returns non-resolved alerts (open + acknowledged) for the alerts
// inbox, worst-severity first then newest. Alerts on inactive (hidden) devices are
// excluded so silenced units don't clutter the list. limit <= 0 means 200.
func (d *DB) ListActiveAlerts(ctx context.Context, limit int) ([]Alert, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.pool.Query(ctx, `
		SELECT a.id, a.rule_id, a.type, a.device_id, COALESCE(d.serial_number, ''),
		       COALESCE(r.name, ''), a.severity, a.status, a.summary, a.detail,
		       a.occurrences, a.muted_until, a.fired_at, a.last_seen_at, a.resolved_at, a.updated_at,
		       a.assignee, a.note, COALESCE(d.device_class, ''), COALESCE(d.restaurant_id::text, '')
		FROM alerts a
		LEFT JOIN devices d ON d.id = a.device_id
		LEFT JOIN restaurants r ON r.id = d.restaurant_id
		WHERE a.status <> 'resolved' AND (a.device_id IS NULL OR NOT d.hidden)
		ORDER BY CASE a.severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,
		         a.fired_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.RuleID, &a.Type, &a.DeviceID, &a.Serial,
			&a.RestaurantName, &a.Severity, &a.Status, &a.Summary, &a.Detail,
			&a.Occurrences, &a.MutedUntil, &a.FiredAt, &a.LastSeenAt, &a.ResolvedAt, &a.UpdatedAt,
			&a.Assignee, &a.Note, &a.DeviceClass, &a.RestaurantID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAlertsPage returns one page of alerts matching the status/severity/type filters,
// newest first, plus the total number of matches (via COUNT(*) OVER()) for pagination.
// All filtering is done in SQL so only the page's rows are loaded, not the whole set.
// types == nil/empty means "any type"; a non-empty slice restricts to those alert types
// (the set of types belonging to a category).
func (d *DB) ListAlertsPage(ctx context.Context, status, severity string, types []string, limit, offset int) ([]Alert, int, error) {
	if limit <= 0 {
		limit = 10
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := d.pool.Query(ctx, `
		SELECT a.id, a.rule_id, a.type, a.device_id, COALESCE(d.serial_number, ''),
		       COALESCE(r.name, ''), a.severity, a.status, a.summary, a.detail,
		       a.occurrences, a.muted_until, a.fired_at, a.last_seen_at, a.resolved_at, a.updated_at,
		       COUNT(*) OVER() AS total
		FROM alerts a
		LEFT JOIN devices d ON d.id = a.device_id
		LEFT JOIN restaurants r ON r.id = d.restaurant_id
		WHERE ($1 = '' OR a.status = $1)
		  AND ($2 = '' OR a.severity = $2)
		  AND (array_length($3::text[], 1) IS NULL OR a.type = ANY($3))
		ORDER BY a.fired_at DESC, a.id DESC
		LIMIT $4 OFFSET $5
	`, status, severity, types, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []Alert
	total := 0
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.RuleID, &a.Type, &a.DeviceID, &a.Serial,
			&a.RestaurantName, &a.Severity, &a.Status, &a.Summary, &a.Detail,
			&a.Occurrences, &a.MutedUntil, &a.FiredAt, &a.LastSeenAt, &a.ResolvedAt, &a.UpdatedAt, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
}

// AlertTypeIdentityConflict is the "Possible impersonation" alert: something used a
// device's serial without its own key, or one serial connected from two addresses at
// once. It is a security finding about the fleet's identity, not a device fault an
// operator can fix, and it names the addresses involved — so it is shown to super
// admins only. Every read path that renders or counts alerts for a signed-in user
// passes hideIdentity for anyone else (dashboard: access.hidesIdentityAlerts).
const AlertTypeIdentityConflict = "identity_conflict"

// AlertSummaryCounts returns headline counts for the alerts page in a single query.
// hideIdentity leaves identity_conflict out, so the headline agrees with the list
// below it for a user who cannot see those alerts.
func (d *DB) AlertSummaryCounts(ctx context.Context, hideIdentity bool) (AlertSummary, error) {
	var s AlertSummary
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE status = 'open'),
		       COUNT(*) FILTER (WHERE status = 'acknowledged'),
		       COUNT(*) FILTER (WHERE status = 'resolved'),
		       COUNT(*) FILTER (WHERE severity = 'critical' AND status <> 'resolved'),
		       COUNT(*) FILTER (WHERE severity = 'warning'  AND status <> 'resolved'),
		       COUNT(*) FILTER (WHERE severity = 'info'     AND status <> 'resolved')
		FROM alerts
		WHERE NOT ($1 AND type = '`+AlertTypeIdentityConflict+`')
	`, hideIdentity).Scan(&s.Total, &s.Open, &s.Acknowledged, &s.Resolved, &s.Critical, &s.Warning, &s.Info)
	return s, err
}

// CountOpenAlerts returns the number of alerts in the 'open' status (for nav badge).
// hideIdentity leaves identity_conflict out — a badge counting an alert the user
// cannot open is worse than no badge.
func (d *DB) CountOpenAlerts(ctx context.Context, hideIdentity bool) (int, error) {
	var n int
	// Exclude alerts on inactive (hidden) devices so silenced units don't inflate the
	// count; fleet-level alerts (no device) always count.
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM alerts a
		LEFT JOIN devices d ON d.id = a.device_id
		WHERE a.status = 'open' AND (a.device_id IS NULL OR NOT d.hidden)
		  AND NOT ($1 AND a.type = '`+AlertTypeIdentityConflict+`')`, hideIdentity).Scan(&n)
	return n, err
}

// DockCounts feeds the live dock: open critical alerts that are not snoozed, and
// commands sent in the last 15 minutes that some device has not finished. The
// window keeps a command wedged at 'delivered' for days from spinning forever.
func (d *DB) DockCounts(ctx context.Context, hideIdentity bool) (critical, running int, err error) {
	err = d.pool.QueryRow(ctx, `
		SELECT
			(SELECT COUNT(*) FROM alerts a
			 LEFT JOIN devices dv ON dv.id = a.device_id
			 WHERE a.status = 'open' AND a.severity = 'critical'
			   AND (a.muted_until IS NULL OR a.muted_until < NOW())
			   AND (a.device_id IS NULL OR NOT dv.hidden)
			   AND NOT ($1 AND a.type = '`+AlertTypeIdentityConflict+`')),
			(SELECT COUNT(*) FROM commands c
			 WHERE c.created_at > NOW() - INTERVAL '15 minutes'
			   AND EXISTS (SELECT 1 FROM command_status cs
			               WHERE cs.command_id = c.id AND cs.status IN ('pending', 'delivered')))`, hideIdentity).Scan(&critical, &running)
	return critical, running, err
}

// RestaurantAlerts is a per-restaurant open-alert rollup for the Daily Report's
// live breakdown.
type RestaurantAlerts struct {
	RestaurantID uuid.UUID
	Name         string
	Critical     int
	Warning      int
	Total        int
}

// AlertsByRestaurant groups open alerts by the restaurant the alerting device
// belongs to, worst-first (most critical, then most total). Restaurants with no
// open alerts are omitted; alerts on unassigned devices don't appear. Uses
// status = 'open' so the total reconciles with CountOpenAlerts / the nav badge.
func (d *DB) AlertsByRestaurant(ctx context.Context) ([]RestaurantAlerts, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT r.id, r.name,
			COUNT(*) FILTER (WHERE a.severity = 'critical'),
			COUNT(*) FILTER (WHERE a.severity <> 'critical'),
			COUNT(*)
		FROM alerts a
		JOIN devices d ON d.id = a.device_id
		JOIN restaurants r ON r.id = d.restaurant_id
		WHERE a.status = 'open' AND NOT d.hidden
		GROUP BY r.id, r.name
		ORDER BY COUNT(*) FILTER (WHERE a.severity = 'critical') DESC, COUNT(*) DESC, r.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RestaurantAlerts
	for rows.Next() {
		var ra RestaurantAlerts
		if err := rows.Scan(&ra.RestaurantID, &ra.Name, &ra.Critical, &ra.Warning, &ra.Total); err != nil {
			return nil, err
		}
		out = append(out, ra)
	}
	return out, rows.Err()
}

// BulkSetAlertStatus transitions every applicable alert to status and returns the
// number changed. "acknowledged" affects open alerts; "resolved" affects every
// non-resolved alert (open + acknowledged). resolved sets resolved_at.
func (d *DB) BulkSetAlertStatus(ctx context.Context, status string) (int64, error) {
	where := "status = 'open'"
	if status == "resolved" {
		where = "status <> 'resolved'"
	}
	tag, err := d.pool.Exec(ctx, `
		UPDATE alerts
		SET status = $1,
		    resolved_at = CASE WHEN $1 = 'resolved' THEN NOW() ELSE resolved_at END,
		    updated_at = NOW()
		WHERE `+where, status)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// BulkSetAlertStatusByIDs transitions the given alerts to status (used by the
// selection-based bulk actions on the alerts page). resolved sets resolved_at.
func (d *DB) BulkSetAlertStatusByIDs(ctx context.Context, ids []uuid.UUID, status string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	tag, err := d.pool.Exec(ctx, `
		UPDATE alerts
		SET status = $2,
		    resolved_at = CASE WHEN $2 = 'resolved' THEN NOW() ELSE resolved_at END,
		    updated_at = NOW()
		WHERE id = ANY($1)
	`, ids, status)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// DeleteAllAlerts removes every alert row (fired instances), returning the number
// deleted. Rule definitions in alert_rules are untouched, so alerts re-fire on the
// next evaluation if their conditions still hold.
// DeleteAllAlerts is now resolve-and-snooze, not a delete. It used to be
// `DELETE FROM alerts`, which destroyed the history AND achieved nothing: the evaluator
// re-created every condition that still held within the minute, so the list refilled
// while the operator watched. Resolving and muting for a day empties the list and keeps
// it empty for anything unchanged, while the rows survive for trend analysis.
func (d *DB) DeleteAllAlerts(ctx context.Context) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE alerts SET
			status      = 'resolved',
			resolved_at = NOW(),
			muted_until = NOW() + INTERVAL '24 hours',
			updated_at  = NOW()
		WHERE status <> 'resolved'`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// SetAlertStatus transitions a single alert (acknowledged/resolved). resolved sets
// resolved_at; other statuses clear it.
func (d *DB) SetAlertStatus(ctx context.Context, id uuid.UUID, status string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE alerts
		SET status = $2,
		    resolved_at = CASE WHEN $2 = 'resolved' THEN NOW() ELSE NULL END,
		    updated_at = NOW()
		WHERE id = $1
	`, id, status)
	return err
}

// alertHit is one device flagged by a rule, with display summary + detail payload.
type alertHit struct {
	DeviceID uuid.UUID
	Serial   string
	Summary  string
	Detail   map[string]any
}

// AlertNotification is a freshly-created alert handed back to the caller so it can
// notify (webhook) exactly once per occurrence.
type AlertNotification struct {
	Type     string
	Severity string
	Summary  string
	Serial   string
	DeviceID uuid.UUID // the device the alert fired for (used for follow-up actions)
	// EventAt is when the underlying problem occurred (e.g. the peak-temperature
	// reading); falls back to the detection time. Timezone is the device's reported
	// IANA zone for rendering EventAt locally ("" → UTC).
	EventAt  time.Time
	Timezone string
	// AlertID is the row this notification is about; Escalated marks an open alert
	// that just became critical (after hours → opening), which pages like a new one.
	AlertID   uuid.UUID
	Escalated bool
	Detail    map[string]any
}

// notifyTimeFrom pulls an event time and timezone out of an alert's detail map for
// the notification. It prefers an explicit event_at/last_seen/peak_at timestamp the
// evaluator recorded; otherwise the alert fired now. Timezone is the device's zone.
func notifyTimeFrom(detail map[string]any) (time.Time, string) {
	at := time.Now().UTC()
	for _, k := range []string{"event_at", "last_seen", "peak_at"} {
		if v, ok := detail[k].(time.Time); ok && !v.IsZero() {
			at = v
			break
		}
	}
	tz, _ := detail["timezone"].(string)
	return at, tz
}

// param reads a numeric threshold from a rule's params JSON, falling back to def.
func param(p map[string]float64, key string, def float64) float64 {
	if v, ok := p[key]; ok {
		return v
	}
	return def
}

// criticalStorageFloor returns the free-GB level the critical storage_low rule fires
// at. The storage_warning and storage_filling rules use it as their lower hand-off
// point so a low device escalates to the critical alert instead of double-firing —
// keeping those rules to a single user-facing setting each. Defaults to 0.5 (matching
// storage_low's own fallback) if the rule row has no free_gb.
func (d *DB) criticalStorageFloor(ctx context.Context) float64 {
	var raw json.RawMessage
	if err := d.pool.QueryRow(ctx,
		`SELECT params FROM alert_rules WHERE type = 'storage_low' LIMIT 1`).Scan(&raw); err != nil {
		return 0.5
	}
	var p map[string]float64
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &p)
	}
	return param(p, "free_gb", 0.5)
}

// chargingFlapSQL counts how many times a device's charging state toggled within a
// recent window. A faulty charger/dock connection drops in and out, so `charging`
// oscillates true↔false — a healthy unit shows 0–1 transitions over hours, a flapping
// one dozens per minute.
//
// This reads device_state_events, where every row already IS a transition, so the
// count is a count of rows rather than a window function over the old check-in
// history. It is also the more direct statement of the
// question: the old form reconstructed transitions by comparing consecutive stored
// check-ins, which worked only because a charging change is non-volatile and therefore
// always forces a row to be stored.
//
// Seed rows are excluded. A seed records a state's first known value with an empty
// from_val and is not a transition; counting it would report a flap for every device
// the first time it was seen.
//
// It counts per device through the (device_id, key, at) primary key: one short range
// probe each, instead of a scan of the whole table filtered to the last few minutes,
// which is what the planner chose for the grouped form. $2 is the minimum count.
const chargingFlapSQL = `
	SELECT d.id, f.flaps
	FROM devices d
	CROSS JOIN LATERAL (
		SELECT COUNT(*) AS flaps
		FROM device_state_events e
		WHERE e.device_id = d.id AND e.key = 'charging'
		  AND e.at > NOW() - ($1 * INTERVAL '1 minute')
		  AND e.from_val IN ('true','false')
		  AND e.to_val   IN ('true','false')
		  AND e.to_val <> e.from_val
	) f
	WHERE f.flaps > $2`

// FlappingChargers returns devices whose charging state is toggling faster than
// flapsPerMin times per minute over the last windowMin minutes — the signature of a
// faulty charger. Maps device ID → observed toggles-per-minute rate (rounded). Used
// by both the fleet-list symbol and the charger_flapping alert rule.
func (d *DB) FlappingChargers(ctx context.Context, windowMin, flapsPerMin int) (map[uuid.UUID]int, error) {
	if windowMin <= 0 {
		windowMin = 5
	}
	if flapsPerMin <= 0 {
		flapsPerMin = 10
	}
	// "More than flapsPerMin per minute" over the window = strictly more than
	// flapsPerMin*windowMin total transitions.
	minCount := flapsPerMin * windowMin
	rows, err := d.pool.Query(ctx, chargingFlapSQL, windowMin, minCount)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]int)
	for rows.Next() {
		var id uuid.UUID
		var n int
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = flapRate(n, windowMin)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A device in a flap stores it as one state, so its toggles are no longer events to
	// count above; the tracker has its rate instead.
	for id, r := range d.flaps.rates(time.Now()) {
		if r > flapsPerMin && r > out[id] {
			out[id] = r
		}
	}
	return out, nil
}

// flapRate rounds a transition count over windowMin minutes to a per-minute rate.
func flapRate(count, windowMin int) int {
	if windowMin <= 0 {
		return count
	}
	return (count + windowMin/2) / windowMin
}

// DeviceChargerFlapRate returns one device's charging toggles-per-minute over the last
// windowMin minutes (0 if none) — for the device page's charger-fault callout.
func (d *DB) DeviceChargerFlapRate(ctx context.Context, deviceID uuid.UUID, windowMin int) (int, error) {
	if windowMin <= 0 {
		windowMin = 5
	}
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM device_state_events
		WHERE device_id = $1 AND key = 'charging'
		  AND at > NOW() - ($2 * INTERVAL '1 minute')
		  AND from_val IN ('true','false')
		  AND to_val   IN ('true','false')
		  AND to_val <> from_val`,
		deviceID, windowMin).Scan(&n)
	if err != nil {
		return 0, err
	}
	// Inside a flap the toggles are not events (charger_flap.go); the tracker counts them.
	if r := d.flaps.rates(time.Now())[deviceID]; r > flapRate(n, windowMin) {
		return r, nil
	}
	return flapRate(n, windowMin), nil
}

// ── Alert channels (Tier 5 §11) ─────────────────────────────────────────────────

// AlertChannel is a notification destination. Alerts at or above MinSeverity route to
// it; realtime channels POST immediately, digest channels batch into the daily digest.
type AlertChannel struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	Kind          string    `json:"kind"` // webhook
	URL           string    `json:"url"`
	MinSeverity   string    `json:"min_severity"`  // info | warning | critical
	Mode          string    `json:"mode"`          // realtime | digest
	ActiveWindow  string    `json:"active_window"` // "" | service | overnight
	Enabled       bool      `json:"enabled"`
	NotifyResolve bool      `json:"notify_resolve"`
	// AlertTypes is the allowlist of alert type keys this channel delivers. Empty/nil
	// means all types (back-compat with channels created before per-type filtering).
	AlertTypes []string  `json:"alert_types,omitempty"`
	CreatedAt  time.Time `json:"created_at"`
}

// AllowsType reports whether this channel should deliver an alert of the given type.
// An empty allowlist means "all types".
func (c AlertChannel) AllowsType(t string) bool {
	if len(c.AlertTypes) == 0 {
		return true
	}
	for _, a := range c.AlertTypes {
		if a == t {
			return true
		}
	}
	return false
}

// SeverityRank maps a severity to a comparable level (info<warning<critical). Unknown
// severities rank as warning so they are never silently dropped below info channels.
func SeverityRank(sev string) int {
	switch sev {
	case "info":
		return 0
	case "critical":
		return 2
	default:
		return 1
	}
}

// ListAlertChannels returns channels, optionally only the enabled ones, newest first.
func (d *DB) ListAlertChannels(ctx context.Context, onlyEnabled bool) ([]AlertChannel, error) {
	q := `SELECT id, name, kind, url, min_severity, mode, active_window, enabled, notify_resolve, alert_types, created_at
	      FROM alert_channels`
	if onlyEnabled {
		q += ` WHERE enabled`
	}
	q += ` ORDER BY created_at`
	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AlertChannel
	for rows.Next() {
		var c AlertChannel
		if err := rows.Scan(&c.ID, &c.Name, &c.Kind, &c.URL, &c.MinSeverity, &c.Mode,
			&c.ActiveWindow, &c.Enabled, &c.NotifyResolve, &c.AlertTypes, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetAlertChannel returns a single channel by id.
func (d *DB) GetAlertChannel(ctx context.Context, id uuid.UUID) (AlertChannel, error) {
	var c AlertChannel
	err := d.pool.QueryRow(ctx, `
		SELECT id, name, kind, url, min_severity, mode, active_window, enabled, notify_resolve, alert_types, created_at
		FROM alert_channels WHERE id = $1`, id).Scan(
		&c.ID, &c.Name, &c.Kind, &c.URL, &c.MinSeverity, &c.Mode,
		&c.ActiveWindow, &c.Enabled, &c.NotifyResolve, &c.AlertTypes, &c.CreatedAt)
	return c, err
}

// CreateAlertChannel inserts a channel and returns its id.
func (d *DB) CreateAlertChannel(ctx context.Context, c AlertChannel) (uuid.UUID, error) {
	var id uuid.UUID
	err := d.pool.QueryRow(ctx, `
		INSERT INTO alert_channels (name, kind, url, min_severity, mode, active_window, enabled, notify_resolve, alert_types)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9) RETURNING id`,
		c.Name, nz(c.Kind, "webhook"), c.URL, nz(c.MinSeverity, "warning"), nz(c.Mode, "realtime"),
		c.ActiveWindow, c.Enabled, c.NotifyResolve, nilIfEmpty(c.AlertTypes)).Scan(&id)
	return id, err
}

// UpdateAlertChannel updates a channel's mutable fields.
func (d *DB) UpdateAlertChannel(ctx context.Context, c AlertChannel) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE alert_channels SET name=$2, url=$3, min_severity=$4, mode=$5,
			active_window=$6, enabled=$7, notify_resolve=$8, kind=$9, alert_types=$10 WHERE id=$1`,
		c.ID, c.Name, c.URL, nz(c.MinSeverity, "warning"), nz(c.Mode, "realtime"),
		c.ActiveWindow, c.Enabled, c.NotifyResolve, nz(c.Kind, "webhook"), nilIfEmpty(c.AlertTypes))
	return err
}

// nilIfEmpty returns nil for an empty slice so it stores as SQL NULL (= "all types").
func nilIfEmpty(s []string) []string {
	if len(s) == 0 {
		return nil
	}
	return s
}

// DeleteAlertChannel removes a channel.
func (d *DB) DeleteAlertChannel(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM alert_channels WHERE id=$1`, id)
	return err
}

// EnsureDefaultChannelFromWebhook migrates a legacy single AlertWebhookURL into a default
// channel the first time, so upgrades keep delivering. No-op if any channel already
// exists or the legacy URL is empty.
func (d *DB) EnsureDefaultChannelFromWebhook(ctx context.Context, legacyURL string) error {
	if legacyURL == "" {
		return nil
	}
	var n int
	if err := d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM alert_channels`).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO alert_channels (name, kind, url, min_severity, mode, active_window)
		VALUES ('Default webhook', 'webhook', $1, 'warning', 'realtime', '')`, legacyURL)
	return err
}

// nz returns s, or def when s is empty.
func nz(s, def string) string {
	if s == "" {
		return def
	}
	return s
}

func (d *DB) ListDeviceActiveAlerts(ctx context.Context, deviceID uuid.UUID, limit int) ([]Alert, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.pool.Query(ctx, `
		SELECT a.id, a.rule_id, a.type, a.device_id, COALESCE(d.serial_number, ''),
		       COALESCE(r.name, ''), a.severity, a.status, a.summary, a.detail,
		       a.occurrences, a.muted_until, a.fired_at, a.last_seen_at, a.resolved_at, a.updated_at,
		       a.assignee, a.note, COALESCE(d.device_class, ''), COALESCE(d.restaurant_id::text, '')
		FROM alerts a
		LEFT JOIN devices d ON d.id = a.device_id
		LEFT JOIN restaurants r ON r.id = d.restaurant_id
		WHERE a.device_id = $1 AND a.status <> 'resolved'
		ORDER BY CASE a.severity WHEN 'critical' THEN 0 WHEN 'warning' THEN 1 ELSE 2 END,
		         a.fired_at DESC
		LIMIT $2`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Alert
	for rows.Next() {
		var a Alert
		if err := rows.Scan(&a.ID, &a.RuleID, &a.Type, &a.DeviceID, &a.Serial,
			&a.RestaurantName, &a.Severity, &a.Status, &a.Summary, &a.Detail,
			&a.Occurrences, &a.MutedUntil, &a.FiredAt, &a.LastSeenAt, &a.ResolvedAt, &a.UpdatedAt,
			&a.Assignee, &a.Note, &a.DeviceClass, &a.RestaurantID); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}
