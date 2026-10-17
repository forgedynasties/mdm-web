package db

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ── Logcat ────────────────────────────────────────────────────────────────────

type LogcatRequest struct {
	ID        uuid.UUID `json:"id"`
	DeviceID  uuid.UUID `json:"device_id"`
	Level     string    `json:"level"`
	Lines     int       `json:"lines"`
	Tag       string    `json:"tag"`
	Status    string    `json:"status"` // pending | delivered | fulfilled
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// LogcatPreset is a distinct (level, lines, tag) combination previously requested,
// for the "recent"/"frequent" quick-rerun chips on the logcat page.
type LogcatPreset struct {
	Level string `json:"level"`
	Lines int    `json:"lines"`
	Tag   string `json:"tag"`
}

type LogcatResult struct {
	ID        uuid.UUID `json:"id"`
	RequestID uuid.UUID `json:"request_id"`
	DeviceID  uuid.UUID `json:"device_id"`
	Content   string    `json:"content"`
	CreatedAt time.Time `json:"created_at"`
}

type LogcatEntry struct {
	Request LogcatRequest
	Result  *LogcatResult
}

// CreateLogcatRequests inserts one logcat request per device in a single
// statement and returns the created rows (with IDs) so the caller can push each
// over the WebSocket. Replaces the per-device N+1 in captureLogsForTargets.
func (d *DB) CreateLogcatRequests(ctx context.Context, deviceIDs []uuid.UUID, level string, lines int, tag string) ([]LogcatRequest, error) {
	if len(deviceIDs) == 0 {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, `
		INSERT INTO logcat_requests (device_id, level, lines, tag)
		SELECT dev, $2, $3, $4 FROM UNNEST($1::uuid[]) AS dev
		RETURNING id, device_id, level, lines, tag, status, created_at, updated_at
	`, deviceIDs, level, lines, tag)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogcatRequest
	for rows.Next() {
		var r LogcatRequest
		if err := rows.Scan(&r.ID, &r.DeviceID, &r.Level, &r.Lines, &r.Tag, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) CreateLogcatRequest(ctx context.Context, deviceID uuid.UUID, level string, lines int, tag string) (*LogcatRequest, error) {
	var r LogcatRequest
	err := d.pool.QueryRow(ctx, `
		INSERT INTO logcat_requests (device_id, level, lines, tag)
		VALUES ($1, $2, $3, $4)
		RETURNING id, device_id, level, lines, tag, status, created_at, updated_at
	`, deviceID, level, lines, tag).Scan(&r.ID, &r.DeviceID, &r.Level, &r.Lines, &r.Tag, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	return &r, err
}

// CreateLogcatRequestForAlert inserts a logcat request linked to an alert (an
// auto-capture triggered by e.g. a device_crash alert). Same as CreateLogcatRequest
// but stamps alert_id so the result can be shown on the alert.
func (d *DB) CreateLogcatRequestForAlert(ctx context.Context, deviceID uuid.UUID, level string, lines int, tag string, alertID uuid.UUID) (*LogcatRequest, error) {
	var r LogcatRequest
	err := d.pool.QueryRow(ctx, `
		INSERT INTO logcat_requests (device_id, level, lines, tag, alert_id)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, device_id, level, lines, tag, status, created_at, updated_at
	`, deviceID, level, lines, tag, alertID).Scan(&r.ID, &r.DeviceID, &r.Level, &r.Lines, &r.Tag, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	return &r, err
}

// GetOpenAlertID returns the id of the current non-resolved alert for (type, device),
// so a follow-up action (e.g. attaching logs) can reference it.
func (d *DB) GetOpenAlertID(ctx context.Context, typ string, deviceID uuid.UUID) (uuid.UUID, bool, error) {
	var id uuid.UUID
	err := d.pool.QueryRow(ctx, `
		SELECT id FROM alerts WHERE type = $1 AND device_id = $2 AND status <> 'resolved'
		ORDER BY fired_at DESC LIMIT 1`, typ, deviceID).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	if err != nil {
		return uuid.Nil, false, err
	}
	return id, true, nil
}

// AlertHasLogcat reports whether a logcat capture has already been requested for an
// alert — the rate limit so a repeatedly-firing crash grabs logs only once.
func (d *DB) AlertHasLogcat(ctx context.Context, alertID uuid.UUID) (bool, error) {
	var exists bool
	err := d.pool.QueryRow(ctx, `SELECT EXISTS(SELECT 1 FROM logcat_requests WHERE alert_id = $1)`, alertID).Scan(&exists)
	return exists, err
}

// AlertLogcat is the auto-captured log attached to an alert, for display.
type AlertLogcat struct {
	Status     string // pending | delivered | fulfilled
	Content    string // set when fulfilled
	Level      string
	Lines      int
	RequestAt  time.Time
	CapturedAt time.Time // when the result came back (zero if not yet)
}

// GetAlertLogcat returns the auto-captured logcat linked to an alert (its request
// status and, once returned, the content). ok is false when no capture was made.
func (d *DB) GetAlertLogcat(ctx context.Context, alertID uuid.UUID) (*AlertLogcat, bool, error) {
	var a AlertLogcat
	var content *string
	var capturedAt *time.Time
	err := d.pool.QueryRow(ctx, `
		SELECT lr.status, lr.level, lr.lines, lr.created_at, res.content, res.created_at
		FROM logcat_requests lr
		LEFT JOIN logcat_results res ON res.request_id = lr.id
		WHERE lr.alert_id = $1
		ORDER BY lr.created_at DESC LIMIT 1`, alertID).Scan(
		&a.Status, &a.Level, &a.Lines, &a.RequestAt, &content, &capturedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, false, nil
	}
	if err != nil {
		return nil, false, err
	}
	if content != nil {
		a.Content = *content
	}
	if capturedAt != nil {
		a.CapturedAt = *capturedAt
	}
	return &a, true, nil
}

// GetPendingLogcatRequestsForDevice returns undelivered logcat requests for a device (oldest first).
func (d *DB) GetPendingLogcatRequestsForDevice(ctx context.Context, deviceID uuid.UUID) ([]LogcatRequest, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, device_id, level, lines, tag, status, created_at, updated_at
		FROM logcat_requests
		WHERE device_id = $1 AND status = 'pending'
		AND created_at > NOW() - INTERVAL '5 minutes'
		ORDER BY created_at ASC
	`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LogcatRequest
	for rows.Next() {
		var r LogcatRequest
		if err := rows.Scan(&r.ID, &r.DeviceID, &r.Level, &r.Lines, &r.Tag, &r.Status, &r.CreatedAt, &r.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) MarkLogcatRequestsDelivered(ctx context.Context, ids []uuid.UUID) error {
	for _, id := range ids {
		if _, err := d.pool.Exec(ctx, `
			UPDATE logcat_requests SET status = 'delivered', updated_at = NOW()
			WHERE id = $1 AND status = 'pending'
		`, id); err != nil {
			return err
		}
	}
	return nil
}

func (d *DB) SaveLogcatResult(ctx context.Context, requestID, deviceID uuid.UUID, content string) (*LogcatResult, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	// A logcat request is issued to a specific device. Since every device shares the
	// same DEVICE_API_KEY, the submitted request_id is an unauthenticated identity
	// claim — verify the request was actually issued to THIS device before storing a
	// result and marking it fulfilled, so a device can't forge/deny another's logs.
	var reqDevice uuid.UUID
	if err := tx.QueryRow(ctx, `SELECT device_id FROM logcat_requests WHERE id = $1`, requestID).Scan(&reqDevice); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, ErrLogcatNotTargeted
		}
		return nil, err
	}
	if reqDevice != deviceID {
		return nil, ErrLogcatNotTargeted
	}

	var result LogcatResult
	err = tx.QueryRow(ctx, `
		INSERT INTO logcat_results (request_id, device_id, content)
		VALUES ($1, $2, $3)
		RETURNING id, request_id, device_id, content, created_at
	`, requestID, deviceID, content).Scan(&result.ID, &result.RequestID, &result.DeviceID, &result.Content, &result.CreatedAt)
	if err != nil {
		return nil, err
	}

	if _, err := tx.Exec(ctx, `
		UPDATE logcat_requests SET status = 'fulfilled', updated_at = NOW() WHERE id = $1
	`, requestID); err != nil {
		return nil, err
	}

	return &result, tx.Commit(ctx)
}

func (d *DB) GetLogcatEntriesForDevice(ctx context.Context, deviceID uuid.UUID, limit int) ([]LogcatEntry, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT
			lr.id, lr.device_id, lr.level, lr.lines, lr.tag,
			CASE
			  WHEN lr.status = 'pending' AND lr.created_at <= NOW() - INTERVAL '5 minutes' THEN 'expired'
			  ELSE lr.status
			END AS status,
			lr.created_at, lr.updated_at,
			lres.id, lres.content, lres.created_at
		FROM logcat_requests lr
		LEFT JOIN logcat_results lres ON lres.request_id = lr.id
		WHERE lr.device_id = $1
		ORDER BY lr.created_at DESC
		LIMIT $2
	`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []LogcatEntry
	for rows.Next() {
		var e LogcatEntry
		var resID *uuid.UUID
		var resContent *string
		var resCreatedAt *time.Time
		if err := rows.Scan(
			&e.Request.ID, &e.Request.DeviceID, &e.Request.Level, &e.Request.Lines,
			&e.Request.Tag, &e.Request.Status, &e.Request.CreatedAt, &e.Request.UpdatedAt,
			&resID, &resContent, &resCreatedAt,
		); err != nil {
			return nil, err
		}
		if resID != nil {
			e.Result = &LogcatResult{
				ID:        *resID,
				RequestID: e.Request.ID,
				DeviceID:  deviceID,
				Content:   *resContent,
				CreatedAt: *resCreatedAt,
			}
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// LogcatSuggestions returns a device's previously-requested logcat presets for quick
// re-run: the most recently used (distinct level/lines/tag, newest first) and the most
// frequently used. Mirrors ShellCommandSuggestions but keyed by the request parameters.
func (d *DB) LogcatSuggestions(ctx context.Context, deviceID uuid.UUID, limit int) (recent, frequent []LogcatPreset, err error) {
	if limit <= 0 {
		limit = 6
	}
	collect := func(q string) ([]LogcatPreset, error) {
		rows, err := d.pool.Query(ctx, q, deviceID, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []LogcatPreset
		for rows.Next() {
			var p LogcatPreset
			if err := rows.Scan(&p.Level, &p.Lines, &p.Tag); err != nil {
				return nil, err
			}
			out = append(out, p)
		}
		return out, rows.Err()
	}
	recent, err = collect(`
		SELECT level, lines, tag FROM (
			SELECT level, lines, tag, MAX(created_at) AS last
			FROM logcat_requests
			WHERE device_id = $1
			GROUP BY level, lines, tag
			ORDER BY last DESC
			LIMIT $2
		) t`)
	if err != nil {
		return nil, nil, err
	}
	frequent, err = collect(`
		SELECT level, lines, tag
		FROM logcat_requests
		WHERE device_id = $1
		GROUP BY level, lines, tag
		ORDER BY COUNT(*) DESC, MAX(created_at) DESC
		LIMIT $2`)
	if err != nil {
		return nil, nil, err
	}
	return recent, frequent, nil
}

// FleetLogcatCapture is one recent logcat request across the whole fleet, with the
// owning device's serial — powers the fleet-wide Logs page.
type FleetLogcatCapture struct {
	RequestID    uuid.UUID `json:"request_id"`
	SerialNumber string    `json:"serial_number"`
	Level        string    `json:"level"`
	Lines        int       `json:"lines"`
	Tag          string    `json:"tag"`
	Status       string    `json:"status"`
	HasResult    bool      `json:"has_result"`
	CreatedAt    time.Time `json:"created_at"`
}

// LogcatPresetStat is a fleet-wide capture preset (level/lines/tag) with how often
// it has been requested across all devices.
type LogcatPresetStat struct {
	Level    string    `json:"level"`
	Lines    int       `json:"lines"`
	Tag      string    `json:"tag"`
	Count    int       `json:"count"`
	LastUsed time.Time `json:"last_used"`
}

// ListRecentLogcatCaptures returns the most recent logcat requests across the whole
// fleet (newest first), each joined to its device serial, for the Logs page.
func (d *DB) ListRecentLogcatCaptures(ctx context.Context, limit int) ([]FleetLogcatCapture, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.pool.Query(ctx, `
		SELECT lr.id, dev.serial_number, lr.level, lr.lines, lr.tag,
		       CASE
		         WHEN lr.status = 'pending' AND lr.created_at <= NOW() - INTERVAL '5 minutes' THEN 'expired'
		         ELSE lr.status
		       END AS status,
		       (lres.id IS NOT NULL) AS has_result,
		       lr.created_at
		FROM logcat_requests lr
		JOIN devices dev ON dev.id = lr.device_id
		LEFT JOIN logcat_results lres ON lres.request_id = lr.id
		ORDER BY lr.created_at DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FleetLogcatCapture
	for rows.Next() {
		var c FleetLogcatCapture
		if err := rows.Scan(&c.RequestID, &c.SerialNumber, &c.Level, &c.Lines, &c.Tag, &c.Status, &c.HasResult, &c.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// FleetLogcatFrequent returns the most frequently requested capture presets across
// the whole fleet, with usage counts, for the Logs page.
func (d *DB) FleetLogcatFrequent(ctx context.Context, limit int) ([]LogcatPresetStat, error) {
	if limit <= 0 {
		limit = 10
	}
	rows, err := d.pool.Query(ctx, `
		SELECT level, lines, tag, COUNT(*) AS cnt, MAX(created_at) AS last
		FROM logcat_requests
		GROUP BY level, lines, tag
		ORDER BY cnt DESC, last DESC
		LIMIT $1
	`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LogcatPresetStat
	for rows.Next() {
		var p LogcatPresetStat
		if err := rows.Scan(&p.Level, &p.Lines, &p.Tag, &p.Count, &p.LastUsed); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FleetLogcatSuggestions returns capture presets across the whole fleet for the
// command-builder quick-fill chips: the most recently used (distinct level/lines/tag,
// newest first) and the most frequently used. Mirrors LogcatSuggestions but not
// scoped to a single device.
func (d *DB) FleetLogcatSuggestions(ctx context.Context, limit int) (recent, frequent []LogcatPreset, err error) {
	if limit <= 0 {
		limit = 6
	}
	collect := func(q string) ([]LogcatPreset, error) {
		rows, err := d.pool.Query(ctx, q, limit)
		if err != nil {
			return nil, err
		}
		defer rows.Close()
		var out []LogcatPreset
		for rows.Next() {
			var p LogcatPreset
			if err := rows.Scan(&p.Level, &p.Lines, &p.Tag); err != nil {
				return nil, err
			}
			out = append(out, p)
		}
		return out, rows.Err()
	}
	recent, err = collect(`
		SELECT level, lines, tag FROM (
			SELECT level, lines, tag, MAX(created_at) AS last
			FROM logcat_requests
			GROUP BY level, lines, tag
			ORDER BY last DESC
			LIMIT $1
		) t`)
	if err != nil {
		return nil, nil, err
	}
	frequent, err = collect(`
		SELECT level, lines, tag
		FROM logcat_requests
		GROUP BY level, lines, tag
		ORDER BY COUNT(*) DESC, MAX(created_at) DESC
		LIMIT $1`)
	if err != nil {
		return nil, nil, err
	}
	return recent, frequent, nil
}

// PruneLogcat deletes logcat results and requests older than `days` days.
func (d *DB) PruneLogcat(ctx context.Context, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	var total int64
	for _, tbl := range []string{"logcat_results", "logcat_requests"} {
		tag, err := d.pool.Exec(ctx, fmt.Sprintf(
			`DELETE FROM %s WHERE created_at < NOW() - INTERVAL '%d days'`, tbl, days))
		if err != nil {
			return total, err
		}
		total += tag.RowsAffected()
	}
	return total, nil
}
