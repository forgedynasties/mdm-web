package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Device struct {
	ID           uuid.UUID `json:"id"`
	SerialNumber string    `json:"serial_number"`
	BuildID      string    `json:"build_id"`
	BatteryPct   int       `json:"battery_pct"`
	LastSeenAt   time.Time `json:"last_seen_at"`
	CreatedAt    time.Time `json:"created_at"`
}

type Summary struct {
	Total          int
	RecentlyActive int // checked in within last 3 minutes
	LowBattery     int // battery < 20%
	AvgBattery     int
	UniqueBuilds   int
}

type Checkin struct {
	ID         uuid.UUID       `json:"id"`
	DeviceID   uuid.UUID       `json:"device_id"`
	BatteryPct int             `json:"battery_pct"`
	BuildID    string          `json:"build_id"`
	Extra      json.RawMessage `json:"extra"`
	CreatedAt  time.Time       `json:"created_at"`
}

type Group struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	DeviceCount int       `json:"device_count"`
	CreatedAt   time.Time `json:"created_at"`
}

type Command struct {
	ID         uuid.UUID `json:"id"`
	Type       string    `json:"type"`
	ApkURL     string    `json:"apk_url"`
	TargetType string    `json:"target_type"` // all | devices | groups
	CreatedAt  time.Time `json:"created_at"`
}

type CommandDelivery struct {
	SerialNumber string    `json:"serial_number"`
	Status       string    `json:"status"` // delivered | installed | failed
	UpdatedAt    time.Time `json:"updated_at"`
}

type DB struct {
	pool *pgxpool.Pool
}

func New(ctx context.Context, connStr string) (*DB, error) {
	pool, err := pgxpool.New(ctx, connStr)
	if err != nil {
		return nil, err
	}
	return &DB{pool: pool}, nil
}

func (d *DB) Close() {
	d.pool.Close()
}

func (d *DB) Ping(ctx context.Context) error {
	return d.pool.Ping(ctx)
}

func (d *DB) RunMigrations(ctx context.Context) error {
	_, err := d.pool.Exec(ctx, migrationSQL)
	return err
}

// UpsertCheckin upserts the device record, inserts a checkin row, and returns
// the device UUID so the caller can query pending commands.
func (d *DB) UpsertCheckin(ctx context.Context, serial, buildID string, batteryPct int, extra json.RawMessage) (uuid.UUID, error) {
	if len(extra) == 0 {
		extra = json.RawMessage("{}")
	}

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, err
	}
	defer tx.Rollback(ctx)

	var deviceID uuid.UUID
	err = tx.QueryRow(ctx, `
		INSERT INTO devices (serial_number, build_id, last_seen_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (serial_number) DO UPDATE
			SET build_id     = EXCLUDED.build_id,
			    last_seen_at = NOW()
		RETURNING id
	`, serial, buildID).Scan(&deviceID)
	if err != nil {
		return uuid.Nil, err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO checkins (device_id, battery_pct, build_id, extra)
		VALUES ($1, $2, $3, $4)
	`, deviceID, batteryPct, buildID, extra)
	if err != nil {
		return uuid.Nil, err
	}

	return deviceID, tx.Commit(ctx)
}

func (d *DB) GetSummary(ctx context.Context) (Summary, error) {
	var s Summary
	err := d.pool.QueryRow(ctx, `
		WITH latest AS (
			SELECT DISTINCT ON (device_id)
				device_id, battery_pct
			FROM checkins
			ORDER BY device_id, created_at DESC
		)
		SELECT
			COUNT(d.id),
			COUNT(*) FILTER (WHERE d.last_seen_at > NOW() - INTERVAL '3 minutes'),
			COUNT(*) FILTER (WHERE l.battery_pct < 20),
			COALESCE(ROUND(AVG(l.battery_pct)), 0),
			COUNT(DISTINCT d.build_id)
		FROM devices d
		LEFT JOIN latest l ON l.device_id = d.id
	`).Scan(&s.Total, &s.RecentlyActive, &s.LowBattery, &s.AvgBattery, &s.UniqueBuilds)
	return s, err
}

func (d *DB) ListDevices(ctx context.Context, search string, offset, limit int) ([]Device, error) {
	const base = `
		SELECT
			d.id, d.serial_number, d.build_id, d.last_seen_at, d.created_at,
			COALESCE(c.battery_pct, 0) AS battery_pct
		FROM devices d
		LEFT JOIN LATERAL (
			SELECT battery_pct FROM checkins
			WHERE device_id = d.id
			ORDER BY created_at DESC
			LIMIT 1
		) c ON true`

	var rows pgx.Rows
	var err error
	if search != "" {
		rows, err = d.pool.Query(ctx, base+`
		WHERE d.serial_number ILIKE $1 OR d.build_id ILIKE $1
		ORDER BY d.last_seen_at DESC
		LIMIT $2 OFFSET $3`, "%"+search+"%", limit, offset)
	} else {
		rows, err = d.pool.Query(ctx, base+`
		ORDER BY d.last_seen_at DESC
		LIMIT $1 OFFSET $2`, limit, offset)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []Device
	for rows.Next() {
		var dev Device
		if err := rows.Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct); err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	return devices, rows.Err()
}

func (d *DB) CountDevices(ctx context.Context, search string) (int, error) {
	var count int
	if search != "" {
		err := d.pool.QueryRow(ctx, `
			SELECT COUNT(*) FROM devices
			WHERE serial_number ILIKE $1 OR build_id ILIKE $1`,
			"%"+search+"%").Scan(&count)
		return count, err
	}
	err := d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM devices`).Scan(&count)
	return count, err
}

func (d *DB) GetDevice(ctx context.Context, serial string) (*Device, error) {
	var dev Device
	err := d.pool.QueryRow(ctx, `
		SELECT
			d.id, d.serial_number, d.build_id, d.last_seen_at, d.created_at,
			COALESCE(c.battery_pct, 0) AS battery_pct
		FROM devices d
		LEFT JOIN LATERAL (
			SELECT battery_pct FROM checkins
			WHERE device_id = d.id
			ORDER BY created_at DESC
			LIMIT 1
		) c ON true
		WHERE d.serial_number = $1
	`, serial).Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct)
	if err != nil {
		return nil, fmt.Errorf("device not found: %w", err)
	}
	return &dev, nil
}

func (d *DB) GetCheckins(ctx context.Context, deviceID uuid.UUID, limit int) ([]Checkin, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, device_id, battery_pct, build_id, extra, created_at
		FROM checkins
		WHERE device_id = $1
		ORDER BY created_at DESC
		LIMIT $2
	`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var checkins []Checkin
	for rows.Next() {
		var c Checkin
		var extra []byte
		if err := rows.Scan(&c.ID, &c.DeviceID, &c.BatteryPct, &c.BuildID, &extra, &c.CreatedAt); err != nil {
			return nil, err
		}
		if len(extra) > 0 {
			c.Extra = json.RawMessage(extra)
		} else {
			c.Extra = json.RawMessage("{}")
		}
		checkins = append(checkins, c)
	}
	return checkins, rows.Err()
}

func (d *DB) GetCheckinsCount(ctx context.Context, deviceID uuid.UUID) (int, error) {
	var count int
	err := d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM checkins WHERE device_id = $1`, deviceID).Scan(&count)
	return count, err
}

func (d *DB) GetCheckinsPaged(ctx context.Context, deviceID uuid.UUID, limit, offset int) ([]Checkin, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, device_id, battery_pct, build_id, extra, created_at
		FROM checkins
		WHERE device_id = $1
		ORDER BY created_at DESC
		LIMIT $2 OFFSET $3
	`, deviceID, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var checkins []Checkin
	for rows.Next() {
		var c Checkin
		var extra []byte
		if err := rows.Scan(&c.ID, &c.DeviceID, &c.BatteryPct, &c.BuildID, &extra, &c.CreatedAt); err != nil {
			return nil, err
		}
		if len(extra) > 0 {
			c.Extra = json.RawMessage(extra)
		} else {
			c.Extra = json.RawMessage("{}")
		}
		checkins = append(checkins, c)
	}
	return checkins, rows.Err()
}

// ── Groups ────────────────────────────────────────────────────────────────────

func (d *DB) CreateGroup(ctx context.Context, name string) (*Group, error) {
	var g Group
	err := d.pool.QueryRow(ctx, `
		INSERT INTO groups (name) VALUES ($1)
		RETURNING id, name, created_at
	`, name).Scan(&g.ID, &g.Name, &g.CreatedAt)
	return &g, err
}

func (d *DB) ListGroups(ctx context.Context) ([]Group, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT g.id, g.name, g.created_at, COUNT(dg.device_id) AS device_count
		FROM groups g
		LEFT JOIN device_groups dg ON dg.group_id = g.id
		GROUP BY g.id, g.name, g.created_at
		ORDER BY g.name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var groups []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.CreatedAt, &g.DeviceCount); err != nil {
			return nil, err
		}
		groups = append(groups, g)
	}
	return groups, rows.Err()
}

func (d *DB) GetGroup(ctx context.Context, id uuid.UUID) (*Group, error) {
	var g Group
	err := d.pool.QueryRow(ctx, `
		SELECT g.id, g.name, g.created_at, COUNT(dg.device_id) AS device_count
		FROM groups g
		LEFT JOIN device_groups dg ON dg.group_id = g.id
		WHERE g.id = $1
		GROUP BY g.id, g.name, g.created_at
	`, id).Scan(&g.ID, &g.Name, &g.CreatedAt, &g.DeviceCount)
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

func (d *DB) RemoveDeviceFromGroup(ctx context.Context, serial string, groupID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `
		DELETE FROM device_groups
		WHERE device_id = (SELECT id FROM devices WHERE serial_number = $1)
		AND group_id = $2
	`, serial, groupID)
	return err
}

func (d *DB) ListGroupDevices(ctx context.Context, groupID uuid.UUID) ([]Device, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT
			d.id, d.serial_number, d.build_id, d.last_seen_at, d.created_at,
			COALESCE(c.battery_pct, 0) AS battery_pct
		FROM devices d
		JOIN device_groups dg ON dg.device_id = d.id
		LEFT JOIN LATERAL (
			SELECT battery_pct FROM checkins
			WHERE device_id = d.id
			ORDER BY created_at DESC
			LIMIT 1
		) c ON true
		WHERE dg.group_id = $1
		ORDER BY d.serial_number
	`, groupID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []Device
	for rows.Next() {
		var dev Device
		if err := rows.Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct); err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	return devices, rows.Err()
}

// GetDeviceIDsBySerials resolves serial numbers to device UUIDs.
func (d *DB) GetDeviceIDsBySerials(ctx context.Context, serials []string) ([]uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id FROM devices WHERE serial_number = ANY($1)
	`, serials)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// ── Commands ──────────────────────────────────────────────────────────────────

// CreateCommand creates a command. For target_type "devices", targetIDs are device UUIDs.
// For "groups", they are group UUIDs. For "all", targetIDs is empty.
func (d *DB) CreateCommand(ctx context.Context, apkURL, targetType string, targetIDs []uuid.UUID) (*Command, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var cmd Command
	err = tx.QueryRow(ctx, `
		INSERT INTO commands (type, apk_url, target_type)
		VALUES ('install_apk', $1, $2)
		RETURNING id, type, apk_url, target_type, created_at
	`, apkURL, targetType).Scan(&cmd.ID, &cmd.Type, &cmd.ApkURL, &cmd.TargetType, &cmd.CreatedAt)
	if err != nil {
		return nil, err
	}

	for _, tid := range targetIDs {
		if _, err := tx.Exec(ctx, `
			INSERT INTO command_targets (command_id, target_id) VALUES ($1, $2)
		`, cmd.ID, tid); err != nil {
			return nil, err
		}
	}

	return &cmd, tx.Commit(ctx)
}

func (d *DB) ListCommands(ctx context.Context) ([]Command, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, type, apk_url, target_type, created_at
		FROM commands
		ORDER BY created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cmds []Command
	for rows.Next() {
		var c Command
		if err := rows.Scan(&c.ID, &c.Type, &c.ApkURL, &c.TargetType, &c.CreatedAt); err != nil {
			return nil, err
		}
		cmds = append(cmds, c)
	}
	return cmds, rows.Err()
}

func (d *DB) GetCommand(ctx context.Context, id uuid.UUID) (*Command, error) {
	var c Command
	err := d.pool.QueryRow(ctx, `
		SELECT id, type, apk_url, target_type, created_at
		FROM commands WHERE id = $1
	`, id).Scan(&c.ID, &c.Type, &c.ApkURL, &c.TargetType, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// GetPendingCommandsForDevice returns commands not yet delivered/acked for this device.
func (d *DB) GetPendingCommandsForDevice(ctx context.Context, deviceID uuid.UUID) ([]Command, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT c.id, c.type, c.apk_url, c.target_type, c.created_at
		FROM commands c
		WHERE (
			c.target_type = 'all'
			OR (c.target_type = 'devices' AND EXISTS (
				SELECT 1 FROM command_targets ct
				WHERE ct.command_id = c.id AND ct.target_id = $1
			))
			OR (c.target_type = 'groups' AND EXISTS (
				SELECT 1 FROM command_targets ct
				JOIN device_groups dg ON dg.group_id = ct.target_id
				WHERE ct.command_id = c.id AND dg.device_id = $1
			))
		)
		AND NOT EXISTS (
			SELECT 1 FROM command_status cs
			WHERE cs.command_id = c.id AND cs.device_id = $1
			AND cs.status IN ('delivered', 'installed', 'failed')
		)
		ORDER BY c.created_at ASC
	`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cmds []Command
	for rows.Next() {
		var c Command
		if err := rows.Scan(&c.ID, &c.Type, &c.ApkURL, &c.TargetType, &c.CreatedAt); err != nil {
			return nil, err
		}
		cmds = append(cmds, c)
	}
	return cmds, rows.Err()
}

// MarkCommandsDelivered records that these commands were sent to the device.
func (d *DB) MarkCommandsDelivered(ctx context.Context, deviceID uuid.UUID, commandIDs []uuid.UUID) error {
	for _, cid := range commandIDs {
		_, err := d.pool.Exec(ctx, `
			INSERT INTO command_status (command_id, device_id, status, updated_at)
			VALUES ($1, $2, 'delivered', NOW())
			ON CONFLICT (command_id, device_id) DO NOTHING
		`, cid, deviceID)
		if err != nil {
			return err
		}
	}
	return nil
}

// AckCommand lets a device report installed or failed for a command.
func (d *DB) AckCommand(ctx context.Context, commandID, deviceID uuid.UUID, status string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO command_status (command_id, device_id, status, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (command_id, device_id) DO UPDATE
			SET status = EXCLUDED.status, updated_at = NOW()
	`, commandID, deviceID, status)
	return err
}

type DeviceCommand struct {
	ID         uuid.UUID `json:"id"`
	ApkURL     string    `json:"apk_url"`
	TargetType string    `json:"target_type"`
	CreatedAt  time.Time `json:"created_at"`
	Status     string    `json:"status"`
	UpdatedAt  time.Time `json:"updated_at"`
}

// GetDeviceCommands returns all commands targeting a device with their status.
func (d *DB) GetDeviceCommands(ctx context.Context, deviceID uuid.UUID) ([]DeviceCommand, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT c.id, c.apk_url, c.target_type, c.created_at,
		       COALESCE(cs.status, 'pending') AS status,
		       COALESCE(cs.updated_at, c.created_at) AS updated_at
		FROM commands c
		LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = $1
		WHERE (
			c.target_type = 'all'
			OR (c.target_type = 'devices' AND EXISTS (
				SELECT 1 FROM command_targets ct WHERE ct.command_id = c.id AND ct.target_id = $1
			))
			OR (c.target_type = 'groups' AND EXISTS (
				SELECT 1 FROM command_targets ct
				JOIN device_groups dg ON dg.group_id = ct.target_id
				WHERE ct.command_id = c.id AND dg.device_id = $1
			))
		)
		ORDER BY c.created_at DESC
	`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DeviceCommand
	for rows.Next() {
		var dc DeviceCommand
		if err := rows.Scan(&dc.ID, &dc.ApkURL, &dc.TargetType, &dc.CreatedAt, &dc.Status, &dc.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, dc)
	}
	return out, rows.Err()
}

// GetCommandDeliveries returns per-device status for a command.
func (d *DB) GetCommandDeliveries(ctx context.Context, commandID uuid.UUID) ([]CommandDelivery, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.serial_number, cs.status, cs.updated_at
		FROM command_status cs
		JOIN devices d ON d.id = cs.device_id
		WHERE cs.command_id = $1
		ORDER BY cs.updated_at DESC
	`, commandID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CommandDelivery
	for rows.Next() {
		var cd CommandDelivery
		if err := rows.Scan(&cd.SerialNumber, &cd.Status, &cd.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, cd)
	}
	return out, rows.Err()
}

// ── Apps ──────────────────────────────────────────────────────────────────────

type App struct {
	ID        uuid.UUID `json:"id"`
	Name      string    `json:"name"`
	ApkURL    string    `json:"apk_url"`
	CreatedAt time.Time `json:"created_at"`
}

func (d *DB) ListApps(ctx context.Context) ([]App, error) {
	rows, err := d.pool.Query(ctx, `SELECT id, name, apk_url, created_at FROM apps ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		var a App
		if err := rows.Scan(&a.ID, &a.Name, &a.ApkURL, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (d *DB) CreateApp(ctx context.Context, name, apkURL string) (*App, error) {
	var a App
	err := d.pool.QueryRow(ctx,
		`INSERT INTO apps (name, apk_url) VALUES ($1, $2) RETURNING id, name, apk_url, created_at`,
		name, apkURL,
	).Scan(&a.ID, &a.Name, &a.ApkURL, &a.CreatedAt)
	return &a, err
}

func (d *DB) DeleteApp(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM apps WHERE id = $1`, id)
	return err
}

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

func (d *DB) CreateLogcatRequest(ctx context.Context, deviceID uuid.UUID, level string, lines int, tag string) (*LogcatRequest, error) {
	var r LogcatRequest
	err := d.pool.QueryRow(ctx, `
		INSERT INTO logcat_requests (device_id, level, lines, tag)
		VALUES ($1, $2, $3, $4)
		RETURNING id, device_id, level, lines, tag, status, created_at, updated_at
	`, deviceID, level, lines, tag).Scan(&r.ID, &r.DeviceID, &r.Level, &r.Lines, &r.Tag, &r.Status, &r.CreatedAt, &r.UpdatedAt)
	return &r, err
}

// GetPendingLogcatRequestsForDevice returns undelivered logcat requests for a device (oldest first).
func (d *DB) GetPendingLogcatRequestsForDevice(ctx context.Context, deviceID uuid.UUID) ([]LogcatRequest, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, device_id, level, lines, tag, status, created_at, updated_at
		FROM logcat_requests
		WHERE device_id = $1 AND status = 'pending'
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
			lr.id, lr.device_id, lr.level, lr.lines, lr.tag, lr.status, lr.created_at, lr.updated_at,
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

// ParseSerials splits a newline/comma separated string into a trimmed slice.
func ParseSerials(raw string) []string {
	raw = strings.ReplaceAll(raw, ",", "\n")
	parts := strings.Split(raw, "\n")
	var out []string
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

const migrationSQL = `
CREATE TABLE IF NOT EXISTS devices (
	id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	serial_number TEXT        NOT NULL UNIQUE,
	build_id      TEXT        NOT NULL DEFAULT '',
	last_seen_at  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS checkins (
	id          UUID     PRIMARY KEY DEFAULT gen_random_uuid(),
	device_id   UUID     NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	battery_pct SMALLINT NOT NULL,
	build_id    TEXT     NOT NULL DEFAULT '',
	extra       JSONB    NOT NULL DEFAULT '{}',
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS groups (
	id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	name       TEXT NOT NULL UNIQUE,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS device_groups (
	device_id UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	group_id  UUID NOT NULL REFERENCES groups(id)  ON DELETE CASCADE,
	PRIMARY KEY (device_id, group_id)
);

CREATE TABLE IF NOT EXISTS commands (
	id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	type        TEXT NOT NULL DEFAULT 'install_apk',
	apk_url     TEXT NOT NULL,
	target_type TEXT NOT NULL,
	created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS command_targets (
	command_id UUID NOT NULL REFERENCES commands(id) ON DELETE CASCADE,
	target_id  UUID NOT NULL,
	PRIMARY KEY (command_id, target_id)
);

CREATE TABLE IF NOT EXISTS command_status (
	command_id UUID NOT NULL REFERENCES commands(id) ON DELETE CASCADE,
	device_id  UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	status     TEXT NOT NULL DEFAULT 'delivered',
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (command_id, device_id)
);

CREATE TABLE IF NOT EXISTS apps (
	id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	name       TEXT NOT NULL,
	apk_url    TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_checkins_device_id  ON checkins(device_id);
CREATE INDEX IF NOT EXISTS idx_checkins_created_at ON checkins(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_devices_last_seen   ON devices(last_seen_at DESC);

CREATE TABLE IF NOT EXISTS logcat_requests (
	id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	device_id  UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	level      TEXT NOT NULL DEFAULT 'W',
	lines      INT  NOT NULL DEFAULT 500,
	tag        TEXT NOT NULL DEFAULT '',
	status     TEXT NOT NULL DEFAULT 'pending',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS logcat_results (
	id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	request_id UUID NOT NULL REFERENCES logcat_requests(id) ON DELETE CASCADE,
	device_id  UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	content    TEXT NOT NULL,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE INDEX IF NOT EXISTS idx_logcat_requests_device_id ON logcat_requests(device_id);
CREATE INDEX IF NOT EXISTS idx_logcat_results_request_id ON logcat_results(request_id);
`
