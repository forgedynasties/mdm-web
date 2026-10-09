package db

import (
	"context"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Problems: the alerts open on one device, worked and notified as one. The 30 Sep alert
// review found 80% of alerts fired while the same device already had two or more other
// alert types open, so a list of alerts was a list of the same few faults said four or
// five ways. The grouping itself is done where the list is shown (dashboard, dispatch);
// this file holds the state it needs: severity by opening hours, the workflow fields,
// which problems a channel was already told about, and the morning digest.

// hitSeverity is the severity to record for one hit. Crashes are graded per device, and
// a critical outside the device's opening hours is recorded as a warning with
// after_hours set: nobody can act on it until the restaurant opens, so it waits for the
// morning digest instead of paging. If the condition still holds at opening the next pass
// records it as critical, which is an escalation and pages then.
func hitSeverity(now time.Time, typ, ruleSev string, h alertHit, w ServiceWindow) string {
	sev := ruleSev
	if typ == "device_crash" {
		n, _ := h.Detail["count"].(int)
		sum, _ := h.Detail["summary"].(string)
		sev = crashSeverity(n, sum)
	}
	if sev == "critical" && !inActiveWindow(now, "", w, "service") {
		if h.Detail != nil {
			h.Detail["after_hours"] = true
		}
		return "warning"
	}
	return sev
}

// crashLoopCount is how many crashes inside the rule's window make a crash loop rather
// than a one-off.
const crashLoopCount = 3

// crashSeverity: a crash is critical for a device only in a crash loop or when the app
// is ours. A one-off crash in a system app (permissioncontroller crashed 75 times on 40
// devices in the review) is a release problem, listed once by signature, not a page per
// device.
func crashSeverity(n int, summary string) string {
	if n >= crashLoopCount || isOurPackage(crashPackage(summary)) {
		return "critical"
	}
	return "warning"
}

// crashPackage is the package leading a DropBox crash summary ("pkg:proc — error").
func crashPackage(summary string) string {
	i := strings.Index(summary, " — ")
	if i <= 0 {
		return ""
	}
	p := summary[:i]
	if j := strings.IndexByte(p, ':'); j > 0 {
		p = p[:j]
	}
	return strings.TrimSpace(p)
}

// isOurPackage reports whether a package is one of AIO's own apps.
func isOurPackage(p string) bool {
	return strings.HasPrefix(p, "aio.") || strings.HasPrefix(p, "com.aio")
}

// ProblemReasons are the reasons a problem can be resolved with by hand. "false_alarm"
// is counted per rule on the Alert rules page, so the next noisy rule shows up as a
// number. A known issue and a false alarm are also snoozed, so the same condition does
// not come straight back.
var ProblemReasons = []struct{ Key, Label string }{
	{"fixed", "Fixed"},
	{"false_alarm", "False alarm"},
	{"known_issue", "Known issue"},
	{"hardware_replaced", "Hardware replaced"},
}

// ProblemAction applies one workflow action to the open alerts of a problem: assign
// (value = owner, "" to clear), ack (value = optional note), snooze (value = hours, 0
// to wake it), resolve (value = reason). Returns the number of alerts changed.
func (d *DB) ProblemAction(ctx context.Context, ids []uuid.UUID, op, value string) (int64, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var q string
	var arg any = value
	switch op {
	case "assign":
		q = `UPDATE alerts SET assignee = $2, updated_at = NOW()
		     WHERE id = ANY($1) AND status <> 'resolved'`
	case "ack":
		q = `UPDATE alerts SET status = 'acknowledged',
		            note = CASE WHEN $2 = '' THEN note ELSE $2 END, updated_at = NOW()
		     WHERE id = ANY($1) AND status <> 'resolved'`
	case "snooze":
		h, err := strconv.Atoi(value)
		if err != nil || h < 0 || h > 24*30 {
			return 0, errBadProblemValue
		}
		arg = h
		q = `UPDATE alerts SET muted_until = CASE WHEN $2::int = 0 THEN NULL
		                                         ELSE NOW() + make_interval(hours => $2::int) END,
		            updated_at = NOW()
		     WHERE id = ANY($1) AND status <> 'resolved'`
	case "resolve":
		mute := 0
		switch value {
		case "fixed", "hardware_replaced":
		case "false_alarm":
			mute = 24
		case "known_issue":
			mute = 24 * 7
		default:
			return 0, errBadProblemValue
		}
		tag, err := d.pool.Exec(ctx, `
			UPDATE alerts SET status = 'resolved', resolved_at = NOW(), resolve_reason = $2,
			       muted_until = CASE WHEN $3::int = 0 THEN muted_until
			                          ELSE NOW() + make_interval(hours => $3::int) END,
			       updated_at = NOW()
			WHERE id = ANY($1) AND status <> 'resolved'`, ids, value, mute)
		if err != nil {
			return 0, err
		}
		return tag.RowsAffected(), nil
	default:
		return 0, errBadProblemValue
	}
	tag, err := d.pool.Exec(ctx, q, ids, arg)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

type problemValueError struct{}

func (problemValueError) Error() string { return "invalid problem action" }

var errBadProblemValue error = problemValueError{}

// PagedDevices returns which of devices already have an open alert a channel was told
// about, other than the alerts in exclude: those devices' problems were already sent,
// and a new symptom on them is added to the problem quietly.
func (d *DB) PagedDevices(ctx context.Context, devices, exclude []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(devices) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT device_id FROM alerts
		WHERE device_id = ANY($1) AND id <> ALL($2::uuid[])
		  AND status <> 'resolved' AND notified_at IS NOT NULL`, devices, exclude)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return out, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// MarkAlertsNotified records that a channel was told about these alerts.
func (d *DB) MarkAlertsNotified(ctx context.Context, ids []uuid.UUID) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := d.pool.Exec(ctx, `UPDATE alerts SET notified_at = NOW() WHERE id = ANY($1) AND notified_at IS NULL`, ids)
	return err
}

// DeviceNotifyInfo is what a problem message says about its device.
type DeviceNotifyInfo struct {
	Restaurant string
	Class      string
}

// DeviceNotifyInfos looks up restaurant and device class for the given devices.
func (d *DB) DeviceNotifyInfos(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]DeviceNotifyInfo, error) {
	out := map[uuid.UUID]DeviceNotifyInfo{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT d.id, COALESCE(r.name, ''), COALESCE(d.device_class, '')
		FROM devices d LEFT JOIN restaurants r ON r.id = d.restaurant_id
		WHERE d.id = ANY($1)`, ids)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var i DeviceNotifyInfo
		if err := rows.Scan(&id, &i.Restaurant, &i.Class); err != nil {
			return out, err
		}
		out[id] = i
	}
	return out, rows.Err()
}

// ClosedProblem is a problem a channel was told about that has now fully cleared: no
// alert is open on the device any more.
type ClosedProblem struct {
	DeviceID   uuid.UUID
	Serial     string
	Restaurant string
	Types      []string
	OpenedAt   time.Time
	ClosedAt   time.Time
	Reason     string // the by-hand resolve reason, "" when it cleared by itself
}

// TakeClosedPagedProblems returns, once, each device whose paged problem has cleared,
// and marks it so the "resolved" message goes out a single time.
func (d *DB) TakeClosedPagedProblems(ctx context.Context) ([]ClosedProblem, error) {
	rows, err := d.pool.Query(ctx, `
		WITH closed AS (
			SELECT a.device_id FROM alerts a
			WHERE a.notified_at IS NOT NULL AND NOT a.resolve_notified AND a.status = 'resolved'
			  AND NOT EXISTS (SELECT 1 FROM alerts o WHERE o.device_id = a.device_id AND o.status <> 'resolved')
			GROUP BY a.device_id
		), marked AS (
			UPDATE alerts SET resolve_notified = true
			FROM closed
			WHERE alerts.device_id = closed.device_id AND alerts.notified_at IS NOT NULL
			  AND NOT alerts.resolve_notified AND alerts.status = 'resolved'
			RETURNING alerts.device_id, alerts.type, alerts.fired_at, alerts.resolved_at, alerts.resolve_reason
		)
		SELECT m.device_id, COALESCE(dv.serial_number, ''), COALESCE(r.name, ''),
		       array_agg(DISTINCT m.type), MIN(m.fired_at), MAX(m.resolved_at),
		       COALESCE(MAX(NULLIF(m.resolve_reason, '')), '')
		FROM marked m
		LEFT JOIN devices dv ON dv.id = m.device_id
		LEFT JOIN restaurants r ON r.id = dv.restaurant_id
		GROUP BY m.device_id, dv.serial_number, r.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ClosedProblem
	for rows.Next() {
		var c ClosedProblem
		var closed *time.Time
		if err := rows.Scan(&c.DeviceID, &c.Serial, &c.Restaurant, &c.Types, &c.OpenedAt, &closed, &c.Reason); err != nil {
			return out, err
		}
		if closed != nil {
			c.ClosedAt = *closed
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// NightAlert is one alert raised overnight, for the morning digest.
type NightAlert struct {
	DeviceID   uuid.UUID
	Serial     string
	Restaurant string
	Type       string
	Summary    string
	FiredAt    time.Time
	Open       bool
	AfterHours bool // recorded as a warning only because the restaurant was closed
}

// AlertsFiredSince returns the warning and critical alerts raised since from, on
// devices still in the fleet, oldest first.
func (d *DB) AlertsFiredSince(ctx context.Context, from time.Time) ([]NightAlert, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT a.device_id, dv.serial_number, COALESCE(r.name, ''), a.type, a.summary, a.fired_at,
		       a.status <> 'resolved', COALESCE((a.detail->>'after_hours')::boolean, false)
		FROM alerts a
		JOIN devices dv ON dv.id = a.device_id
		LEFT JOIN restaurants r ON r.id = dv.restaurant_id
		WHERE a.fired_at >= $1 AND a.severity IN ('warning', 'critical')
		  AND NOT dv.hidden AND dv.enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')
		ORDER BY a.fired_at`, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NightAlert
	for rows.Next() {
		var n NightAlert
		if err := rows.Scan(&n.DeviceID, &n.Serial, &n.Restaurant, &n.Type, &n.Summary, &n.FiredAt, &n.Open, &n.AfterHours); err != nil {
			return out, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

// OfflineAtOpen counts, per restaurant, placed devices that have reported in the last
// week but are not reporting now: the "is everything up for opening" line of the digest.
func (d *DB) OfflineAtOpen(ctx context.Context, connected []uuid.UUID) (map[string]int, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT r.name, COUNT(*)
		FROM devices dv JOIN restaurants r ON r.id = dv.restaurant_id
		WHERE NOT dv.hidden AND dv.enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')
		  AND dv.custody_server = ''
		  AND dv.last_seen_at > NOW() - INTERVAL '7 days'
		  AND dv.last_seen_at < NOW() - INTERVAL '15 minutes'
		  AND dv.id <> ALL($1::uuid[])
		GROUP BY r.name`, connected)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var name string
		var n int
		if err := rows.Scan(&name, &n); err != nil {
			return out, err
		}
		out[name] = n
	}
	return out, rows.Err()
}

// ClaimDigest records the morning digest for day and reports whether this call claimed
// it: false means it was already sent, by this process or before a restart.
func (d *DB) ClaimDigest(ctx context.Context, day time.Time, lines int) (bool, error) {
	tag, err := d.pool.Exec(ctx, `INSERT INTO alert_digests (day, lines) VALUES ($1::date, $2) ON CONFLICT (day) DO NOTHING`,
		day.Format("2006-01-02"), lines)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// ReleaseDigest forgets a claimed digest whose send failed, so the next pass retries.
func (d *DB) ReleaseDigest(ctx context.Context, day time.Time) {
	_, _ = d.pool.Exec(ctx, `DELETE FROM alert_digests WHERE day = $1::date`, day.Format("2006-01-02"))
}

// CrashIssue is one crash signature on one build, across devices: a release problem.
type CrashIssue struct {
	Kind      string
	Summary   string
	BuildID   string
	Events    int
	Devices   int
	FirstAt   time.Time
	LastAt    time.Time
	ReleaseID int // the release whose version is this build (see CrashGroupsOnBuild), 0 when none
}

// releaseIssueMinDevices is how many devices a crash signature must reach to be a
// release issue rather than one device's problem.
const releaseIssueMinDevices = 3

// crashSignatureSQL is a crash's signature: its summary up to the first comma. The same
// fault can be worded several ways — permissioncontroller's crash names one of four
// "safety sources" after the comma — and counting each wording apart split one crash on
// 34 devices into four smaller ones.
const crashSignatureSQL = `split_part(e.summary, ',', 1)`

// ListCrashIssues groups the last days of crashes by kind, signature and build, keeping
// those seen on at least releaseIssueMinDevices devices, widest first.
func (d *DB) ListCrashIssues(ctx context.Context, days, limit int) ([]CrashIssue, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.pool.Query(ctx, `
		SELECT e.kind, `+crashSignatureSQL+`, e.build_id, COUNT(*), COUNT(DISTINCT e.device_id),
		       MIN(e.occurred_at), MAX(e.occurred_at),
		       COALESCE((SELECT rl.id FROM releases rl WHERE rl.version = e.build_id AND e.build_id <> ''
		                 ORDER BY rl.id DESC LIMIT 1), 0)
		FROM device_events e JOIN devices dv ON dv.id = e.device_id
		WHERE e.kind NOT IN ('reboot', 'kiosk_exit_offline', 'offline_dropped') AND NOT dv.hidden
		  AND e.occurred_at > NOW() - make_interval(days => $1)
		GROUP BY e.kind, `+crashSignatureSQL+`, e.build_id
		HAVING COUNT(DISTINCT e.device_id) >= $2
		ORDER BY COUNT(DISTINCT e.device_id) DESC, MAX(e.occurred_at) DESC
		LIMIT $3`, days, releaseIssueMinDevices, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CrashIssue
	for rows.Next() {
		var c CrashIssue
		if err := rows.Scan(&c.Kind, &c.Summary, &c.BuildID, &c.Events, &c.Devices, &c.FirstAt, &c.LastAt, &c.ReleaseID); err != nil {
			return out, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// IsReleaseCrash reports whether a crash signature has been seen on enough devices in
// the last two weeks to be a release issue, which is not paged per device.
func (d *DB) IsReleaseCrash(ctx context.Context, summary string) bool {
	if summary == "" {
		return false
	}
	var n int
	if err := d.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT e.device_id) FROM device_events e
		WHERE `+crashSignatureSQL+` = split_part($1, ',', 1) AND e.occurred_at > NOW() - INTERVAL '14 days'`, summary).Scan(&n); err != nil {
		return false
	}
	return n >= releaseIssueMinDevices
}

// FalseAlarmCounts returns, per rule type, how many alerts were resolved as a false
// alarm in the last days.
func (d *DB) FalseAlarmCounts(ctx context.Context, days int) (map[string]int, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT type, COUNT(*) FROM alerts
		WHERE resolve_reason = 'false_alarm' AND resolved_at > NOW() - make_interval(days => $1)
		GROUP BY type`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var t string
		var n int
		if err := rows.Scan(&t, &n); err != nil {
			return out, err
		}
		out[t] = n
	}
	return out, rows.Err()
}
