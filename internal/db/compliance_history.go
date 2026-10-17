package db

import (
	"context"
	"time"

	"github.com/google/uuid"
)

// ── Compliance history, for the venue reports ────────────────────────────────
//
// The venue's device-security report (dashboard/compliance_report.go) is rebuilt from
// what the engine already keeps: each device's current result per check and every state
// change as a compliance_event. Events are pruned after 400 days (RunCompliance), so any
// month the report can be opened for is covered.

// ComplianceEvent is one stored state change of one check on one device.
type ComplianceEvent struct {
	DeviceID uuid.UUID
	CheckID  string
	From, To string // states; From is '' for a device's first result
	Level    string
	At       time.Time
}

// ComplianceVenueDevice is a device the venue report covers.
type ComplianceVenueDevice struct {
	ID     uuid.UUID
	Serial string
}

// VenueComplianceDevices lists the devices at a venue now: not hidden, not retired,
// wiped or unenrolled. The report covers these devices for the whole period, so a device
// moved in mid-month is reported for its weeks here (it was scored only at a venue).
func (d *DB) VenueComplianceDevices(ctx context.Context, restaurantID uuid.UUID) ([]ComplianceVenueDevice, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, serial_number FROM devices
		WHERE restaurant_id = $1 AND NOT hidden
		  AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')
		ORDER BY serial_number`, restaurantID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ComplianceVenueDevice
	for rows.Next() {
		var v ComplianceVenueDevice
		if err := rows.Scan(&v.ID, &v.Serial); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ComplianceHistory returns, for the devices and checks given, the current results and
// the events needed to replay [from, now): the last event before from for each device and
// check (its state at from), then every event since. Events after the period are included
// so the report can say when a failure that outlived the period ended.
func (d *DB) ComplianceHistory(ctx context.Context, ids []uuid.UUID, checks []string, from time.Time) ([]ComplianceResult, []ComplianceEvent, error) {
	if len(ids) == 0 || len(checks) == 0 {
		return nil, nil, nil
	}
	var results []ComplianceResult
	rows, err := d.pool.Query(ctx, `
		SELECT device_id, check_id, state, level, detail, since, checked_at
		FROM compliance_results WHERE device_id = ANY($1) AND check_id = ANY($2)`, ids, checks)
	if err != nil {
		return nil, nil, err
	}
	for rows.Next() {
		var r ComplianceResult
		if err := rows.Scan(&r.DeviceID, &r.CheckID, &r.State, &r.Level, &r.Detail, &r.Since, &r.CheckedAt); err != nil {
			rows.Close()
			return nil, nil, err
		}
		results = append(results, r)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, nil, err
	}

	var events []ComplianceEvent
	rows, err = d.pool.Query(ctx, `
		(SELECT DISTINCT ON (device_id, check_id) device_id, check_id, from_state, to_state, level, at
		 FROM compliance_events
		 WHERE device_id = ANY($1) AND check_id = ANY($2) AND at < $3
		 ORDER BY device_id, check_id, at DESC)
		UNION ALL
		(SELECT device_id, check_id, from_state, to_state, level, at
		 FROM compliance_events
		 WHERE device_id = ANY($1) AND check_id = ANY($2) AND at >= $3)
		ORDER BY at`, ids, checks, from)
	if err != nil {
		return nil, nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var e ComplianceEvent
		if err := rows.Scan(&e.DeviceID, &e.CheckID, &e.From, &e.To, &e.Level, &e.At); err != nil {
			return nil, nil, err
		}
		events = append(events, e)
	}
	return results, events, rows.Err()
}

// SystemFix is a command the server sent by itself to fix something: the compliance
// auto-fix ("compliance") or the client auto-update ("auto-update").
type SystemFix struct {
	DeviceID uuid.UUID
	Type     string
	By       string
	At       time.Time
	Status   string // the device's delivery status for it, '' when it never answered
}

// SystemFixes lists the auto-fix and auto-update commands sent to the devices since
// from, with each device's delivery status. The report says a failure was fixed
// automatically only when one of these completed while it was open.
func (d *DB) SystemFixes(ctx context.Context, ids []uuid.UUID, from time.Time) ([]SystemFix, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT t.target_id, c.type, c.created_by, c.created_at, COALESCE(s.status, '')
		FROM commands c
		JOIN command_targets t ON t.command_id = c.id
		LEFT JOIN command_status s ON s.command_id = c.id AND s.device_id = t.target_id
		WHERE c.target_type = 'devices' AND t.target_id = ANY($1)
		  AND c.created_by IN ('compliance', 'auto-update') AND c.created_at >= $2
		ORDER BY c.created_at`, ids, from)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []SystemFix
	for rows.Next() {
		var f SystemFix
		if err := rows.Scan(&f.DeviceID, &f.Type, &f.By, &f.At, &f.Status); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// ClaimComplianceReportSend records that a venue's report for a month is being sent.
// False when it already was: the monthly job sends each venue's report once however
// often it runs, even with two servers on one database.
func (d *DB) ClaimComplianceReportSend(ctx context.Context, restaurantID uuid.UUID, month string) (bool, error) {
	tag, err := d.pool.Exec(ctx, `
		INSERT INTO compliance_report_sends (restaurant_id, month) VALUES ($1, $2)
		ON CONFLICT (restaurant_id, month) DO NOTHING`, restaurantID, month)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() == 1, nil
}

// SetComplianceReportRecipients notes who a claimed send went to.
func (d *DB) SetComplianceReportRecipients(ctx context.Context, restaurantID uuid.UUID, month, recipients string) error {
	_, err := d.pool.Exec(ctx, `UPDATE compliance_report_sends SET recipients = $3 WHERE restaurant_id = $1 AND month = $2`,
		restaurantID, month, recipients)
	return err
}

// ReleaseComplianceReportSend drops a claim whose send failed for every recipient, so the
// next run tries again.
func (d *DB) ReleaseComplianceReportSend(ctx context.Context, restaurantID uuid.UUID, month string) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM compliance_report_sends WHERE restaurant_id = $1 AND month = $2`, restaurantID, month)
	return err
}
