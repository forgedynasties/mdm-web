package db

import (
	"context"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

type AuditEntry struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Target    string    `json:"target"`
	Detail    string    `json:"detail"`
	// DeviceID is set when the action was performed on one device; nil otherwise.
	DeviceID *uuid.UUID `json:"device_id,omitempty"`
	// DeviceSerial is resolved for display and is empty when DeviceID is nil.
	DeviceSerial string `json:"device_serial,omitempty"`
}

// InsertAuditDevice records an action against a specific device, so "what was done to
// this device, and by whom" is an indexed lookup rather than a guess at what `target`
// happens to mean for that action.
func (d *DB) InsertAuditDevice(ctx context.Context, actor, action, target, detail string, deviceID uuid.UUID) error {
	_, err := d.pool.Exec(ctx,
		`INSERT INTO audit_log (actor, action, target, detail, device_id) VALUES ($1, $2, $3, $4, $5)`,
		actor, action, target, detail, deviceID)
	return err
}

// ListAuditForDevice returns the actions taken on one device, newest first.
func (d *DB) ListAuditForDevice(ctx context.Context, deviceID uuid.UUID, limit int) ([]AuditEntry, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.pool.Query(ctx, `
		SELECT id, created_at, actor, action, target, detail
		FROM audit_log
		WHERE device_id = $1
		ORDER BY created_at DESC
		LIMIT $2`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.CreatedAt, &e.Actor, &e.Action, &e.Target, &e.Detail); err != nil {
			return nil, err
		}
		e.DeviceID = &deviceID
		out = append(out, e)
	}
	return out, rows.Err()
}

func (d *DB) InsertAudit(ctx context.Context, actor, action, target, detail string) error {
	_, err := d.pool.Exec(ctx,
		`INSERT INTO audit_log (actor, action, target, detail) VALUES ($1, $2, $3, $4)`,
		actor, action, target, detail)
	return err
}

func (d *DB) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	return d.ListAuditFiltered(ctx, "", limit)
}

// ListAuditFiltered is ListAudit with an optional actor filter (exact match) — backs
// the Activity page's per-user view. actor == "" returns everyone.
func (d *DB) ListAuditFiltered(ctx context.Context, actor string, limit int) ([]AuditEntry, error) {
	return d.ListAuditFilteredEx(ctx, actor, "", limit)
}

// ListAuditFilteredEx is ListAuditFiltered plus excludeActor (exact match, e.g.
// "admin") — ignored when actor is set, since an explicit actor filter overrides
// it. Backs the Activity page's default-hide-admin toggle.
func (d *DB) ListAuditFilteredEx(ctx context.Context, actor, excludeActor string, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	var rows pgx.Rows
	var err error
	switch {
	case actor != "":
		rows, err = d.pool.Query(ctx,
			`SELECT id, created_at, actor, action, target, detail FROM audit_log WHERE actor = $2 AND action <> '`+PageViewAction+`' ORDER BY created_at DESC LIMIT $1`, limit, actor)
	case excludeActor != "":
		rows, err = d.pool.Query(ctx,
			`SELECT id, created_at, actor, action, target, detail FROM audit_log WHERE actor != $2 ORDER BY created_at DESC LIMIT $1`, limit, excludeActor)
	default:
		rows, err = d.pool.Query(ctx,
			`SELECT id, created_at, actor, action, target, detail FROM audit_log ORDER BY created_at DESC LIMIT $1`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var a AuditEntry
		if err := rows.Scan(&a.ID, &a.CreatedAt, &a.Actor, &a.Action, &a.Target, &a.Detail); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// PageViewAction is the audit action recorded for a dashboard page navigation.
// Views are excluded from "actions" counts and hidden in Activity unless asked
// for; they exist so viewer accounts leave a trace too.
const PageViewAction = "page.view"

// ListAuditForActivity is the Activity page's feed: newest first, optionally
// without one actor (the env admin) and optionally including page views.
// AuditCountsByActor is how many log entries each actor has (page views excluded),
// for ranking the Activity page's user filter by who does the most.
func (d *DB) AuditCountsByActor(ctx context.Context) (map[string]int, error) {
	rows, err := d.pool.Query(ctx, `SELECT actor, COUNT(*) FROM audit_log WHERE action <> '`+PageViewAction+`' GROUP BY actor`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]int{}
	for rows.Next() {
		var a string
		var n int
		if err := rows.Scan(&a, &n); err != nil {
			return nil, err
		}
		out[a] = n
	}
	return out, rows.Err()
}

func (d *DB) ListAuditForActivity(ctx context.Context, excludeActor string, includeViews bool, limit int, serial string) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := d.pool.Query(ctx, `
		SELECT a.id, a.created_at, a.actor, a.action, a.target, a.detail,
		       a.device_id, COALESCE(dv.serial_number, '')
		FROM audit_log a
		LEFT JOIN devices dv ON dv.id = a.device_id
		WHERE ($2 = '' OR a.actor <> $2) AND ($3 OR a.action <> '`+PageViewAction+`')
		  -- $4 empty means every device. Matched on the device, not on target: target
		  -- holds a serial for some actions, a comma-joined list for bulk ones and a
		  -- rollout id for others, so a text match there answers a different question
		  -- per action.
		  AND ($4 = '' OR dv.serial_number = $4)
		ORDER BY a.created_at DESC LIMIT $1`, limit, excludeActor, includeViews, serial)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var a AuditEntry
		if err := rows.Scan(&a.ID, &a.CreatedAt, &a.Actor, &a.Action, &a.Target, &a.Detail, &a.DeviceID, &a.DeviceSerial); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ListAuditDeviceSerials returns the devices that have any recorded activity, most
// recently active first — the Activity page's device filter.
func (d *DB) ListAuditDeviceSerials(ctx context.Context) ([]string, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT dv.serial_number
		FROM audit_log a JOIN devices dv ON dv.id = a.device_id
		WHERE a.device_id IS NOT NULL
		GROUP BY dv.serial_number
		ORDER BY MAX(a.created_at) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var sn string
		if err := rows.Scan(&sn); err != nil {
			return nil, err
		}
		out = append(out, sn)
	}
	return out, rows.Err()
}

// ListAuditActors returns the distinct actor names that have logged activity,
// newest-active first — backs the Activity page's per-user filter dropdown.
func (d *DB) ListAuditActors(ctx context.Context) ([]string, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT actor FROM audit_log GROUP BY actor ORDER BY MAX(created_at) DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var a string
		if err := rows.Scan(&a); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}


