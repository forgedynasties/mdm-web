package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Device struct {
	ID             uuid.UUID       `json:"id"`
	SerialNumber   string          `json:"serial_number"`
	BuildID        string          `json:"build_id"`
	BatteryPct     int             `json:"battery_pct"`
	LastSeenAt     time.Time       `json:"last_seen_at"`
	CreatedAt      time.Time       `json:"created_at"`
	PollIntervalMs int             `json:"poll_interval_ms"`
	KioskEnabled   bool            `json:"kiosk_enabled"`
	KioskPackage   string          `json:"kiosk_package"`
	LatestExtra    json.RawMessage `json:"latest_extra,omitempty"`
	Hidden         bool            `json:"hidden"`
}

// DefaultKioskFeatures shows system info (battery/wifi) but blocks home, recents,
// notifications, global actions and keyguard — matching Android LOCK_TASK_FEATURE_SYSTEM_INFO.
const DefaultKioskFeatures = 1

type DeviceConfig struct {
	DeviceID      uuid.UUID `json:"device_id"`
	KioskEnabled  bool      `json:"kiosk_enabled"`
	KioskPackage  string    `json:"kiosk_package"`
	KioskFeatures int       `json:"kiosk_features"`
	UpdatedAt     time.Time `json:"updated_at"`
}

type Summary struct {
	Total          int
	RecentlyActive int // checked in within last 3 minutes
	LowBattery     int // battery < 20%
	UniqueBuilds   int
	KioskCount     int // devices currently in kiosk mode
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

type OTAPackage struct {
	ID              int       `json:"id"`
	Type            string    `json:"type"`            // "full" or "incremental"
	TargetBuildID   string    `json:"target_build_id"`
	SourceBuildID   string    `json:"source_build_id"` // incremental only
	ReleaseDate     time.Time `json:"release_date"`
	UpdateURL       string    `json:"update_url"`
	Changelog       string    `json:"changelog"`
	Status          string    `json:"status"` // "active" or "yanked"
	CreatedAt       time.Time `json:"created_at"`
	DeploymentCount int       `json:"deployment_count,omitempty"` // populated by ListOTAPackages
}

type Update struct {
	ID              int            `json:"id"`
	OtaPackageID    int            `json:"ota_package_id"`
	RebootBehavior  string         `json:"reboot_behavior"` // "immediate", "scheduled", "manual"
	ScheduledTime   *time.Time     `json:"scheduled_time"`
	Status          string         `json:"status"` // "pending", "active", "complete"
	CreatedAt       time.Time      `json:"created_at"`
	OtaPackage      *OTAPackage    `json:"ota_package,omitempty"`
	Targets         []UpdateTarget `json:"targets,omitempty"`
	DeviceTotal     int            `json:"device_total,omitempty"`     // populated by ListDeploymentsByPackage
	DeviceInstalled int            `json:"device_installed,omitempty"` // populated by ListDeploymentsByPackage
}

type UpdateTarget struct {
	UpdateID     int       `json:"update_id"`
	DeviceID     uuid.UUID `json:"device_id"`
	SerialNumber string    `json:"serial_number"` // joined from devices
	BuildID      string    `json:"build_id"`      // current device build
	Status       string    `json:"status"`        // "pending", "downloading", "installing", "installed"
}

type Command struct {
	ID         uuid.UUID       `json:"id"`
	Type       string          `json:"type"`
	ApkURL     string          `json:"apk_url"`
	Payload    json.RawMessage `json:"payload"`
	TargetType string          `json:"target_type"`
	CreatedAt  time.Time       `json:"created_at"`
}

type ExportRow struct {
	SerialNumber string          `json:"serial_number"`
	BatteryPct   int             `json:"battery_pct"`
	BuildID      string          `json:"build_id"`
	Extra        json.RawMessage `json:"extra"`
	Timestamp    time.Time       `json:"timestamp"`
	LastSeenAt   time.Time       `json:"last_seen_at"`
}

type CommandDelivery struct {
	DeviceID     uuid.UUID `json:"device_id"`
	SerialNumber string    `json:"serial_number"`
	Status       string    `json:"status"`
	UpdatedAt    time.Time `json:"updated_at"`
	Output       string    `json:"output"`
	LastSeenAt   time.Time `json:"last_seen_at"`
	Online       bool      `json:"online"` // set by the handler from the ws.Hub, not the DB
}

// DeviceFilter holds optional filter parameters for device listing.
type DeviceFilter struct {
	Search                    string    // search by serial substring
	GroupID                   uuid.UUID // filter by group membership (uuid.Nil = no filter)
	ProductionID uuid.UUID // filter by production (uuid.Nil = no filter)
	Online                    string    // "online", "offline", or "" (no filter)
	BuildID                   string    // exact build_id match, or "" (no filter)
	Battery                   string    // "low" (<20%), "mid" (20-49%), "ok" (>=50%), or "" (no filter)
	Kiosk                     string    // "enabled" (kiosk on), "disabled" (kiosk off), or "" (no filter)
	Hidden                    string    // "include" (show all), "only" (hidden only), or "" (active only)
	ActiveThresholdSecs       int       // seconds before a device is considered offline (0 = default 180)
}

// ── Productions ───────────────────────────────────────────────────────────────

const base36Chars = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZ"

// EncodeBatch encodes a month (1-12) and 2-digit year into a 2-char base36 batch code.
// Formula: month*100 + year → base36(2 chars).
// e.g. April 2026 → 4*100+26 = 426 → "BU"
func EncodeBatch(month, year int) string {
	val := month*100 + (year % 100)
	return string([]byte{base36Chars[val/36], base36Chars[val%36]})
}

// DecodeBatch decodes a 2-char base36 batch code into month and 2-digit year.
func DecodeBatch(batch string) (month, year int) {
	hi := strings.IndexByte(base36Chars, batch[0])
	lo := strings.IndexByte(base36Chars, batch[1])
	val := hi*36 + lo
	return val / 100, val % 100
}

type Production struct {
	ID            uuid.UUID `json:"id"`
	Name          string    `json:"name"`
	ProductCode   string    `json:"product_code"`
	ModelCode     string    `json:"model_code"`
	Variant       string    `json:"variant"`
	SKU           string    `json:"sku"`
	Batch         string    `json:"batch"`
	BatchMonth    int       `json:"batch_month"`
	BatchYear     int       `json:"batch_year"`
	StartSequence int       `json:"start_sequence"`
	EndSequence   int       `json:"end_sequence"`
	Notes         string    `json:"notes"`
	CreatedAt     time.Time `json:"created_at"`
	// Computed stats
	Total         int `json:"total"`
	EverConnected int `json:"ever_connected"`
	Online        int `json:"online"`
}

func (p *Production) SerialPrefix() string {
	return p.ProductCode + p.ModelCode + p.Variant + p.SKU + p.Batch
}

func (p *Production) FirstSerial() string {
	return fmt.Sprintf("%s%05d", p.SerialPrefix(), p.StartSequence)
}

func (p *Production) LastSerial() string {
	return fmt.Sprintf("%s%05d", p.SerialPrefix(), p.EndSequence)
}

// ── Users ─────────────────────────────────────────────────────────────────────

type User struct {
	ID           uuid.UUID `json:"id"`
	Username     string    `json:"username"`
	Role         string    `json:"role"` // "viewer" | "operator"
	PasswordHash string    `json:"-"`
	CreatedAt    time.Time `json:"created_at"`
}

func (d *DB) CreateUser(ctx context.Context, username, passwordHash, role string) (*User, error) {
	var u User
	err := d.pool.QueryRow(ctx, `
		INSERT INTO users (username, password_hash, role)
		VALUES ($1, $2, $3)
		RETURNING id, username, role, password_hash, created_at
	`, username, passwordHash, role).Scan(&u.ID, &u.Username, &u.Role, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (d *DB) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	var u User
	err := d.pool.QueryRow(ctx, `
		SELECT id, username, role, password_hash, created_at FROM users WHERE username = $1
	`, username).Scan(&u.ID, &u.Username, &u.Role, &u.PasswordHash, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (d *DB) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, username, role, created_at FROM users ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.CreatedAt); err != nil {
			return nil, err
		}
		users = append(users, u)
	}
	return users, nil
}

func (d *DB) DeleteUser(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM users WHERE id = $1`, id)
	return err
}

// ProductionDevice is a device row augmented with connection status for production detail view.
type ProductionDevice struct {
	Serial           string    `json:"serial"`
	DeviceID         uuid.UUID `json:"device_id,omitempty"`
	BuildID          string    `json:"build_id,omitempty"`
	BatteryPct       int       `json:"battery_pct,omitempty"`
	LastSeenAt       time.Time `json:"last_seen_at,omitempty"`
	CreatedAt        time.Time `json:"created_at,omitempty"`
	ConnectionStatus string    `json:"connection_status"` // "online", "offline", "never"
}

type DB struct {
	pool *pgxpool.Pool
}

var ErrCommandNotTargeted = errors.New("command does not target device")

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
// the device UUID and poll interval so the caller can query pending commands.
func (d *DB) UpsertCheckin(ctx context.Context, serial, buildID string, batteryPct int, extra json.RawMessage) (uuid.UUID, int, error) {
	if len(extra) == 0 {
		extra = json.RawMessage("{}")
	}

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, 0, err
	}
	defer tx.Rollback(ctx)

	var deviceID uuid.UUID
	var pollIntervalMs int
	err = tx.QueryRow(ctx, `
		INSERT INTO devices (serial_number, build_id, last_seen_at, latest_battery_pct, latest_extra)
		VALUES ($1, $2, NOW(), $3, $4)
		ON CONFLICT (serial_number) DO UPDATE
			SET build_id           = EXCLUDED.build_id,
			    last_seen_at       = NOW(),
			    latest_battery_pct = EXCLUDED.latest_battery_pct,
			    latest_extra       = EXCLUDED.latest_extra,
			    hidden             = false
		RETURNING id, poll_interval_ms
	`, serial, buildID, batteryPct, extra).Scan(&deviceID, &pollIntervalMs)
	if err != nil {
		return uuid.Nil, 0, err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO checkins (device_id, battery_pct, build_id, extra)
		VALUES ($1, $2, $3, $4)
	`, deviceID, batteryPct, buildID, extra)
	if err != nil {
		return uuid.Nil, 0, err
	}

	return deviceID, pollIntervalMs, tx.Commit(ctx)
}

func (d *DB) GetSummary(ctx context.Context, activeSecs int) (Summary, error) {
	if activeSecs <= 0 {
		activeSecs = 180
	}
	var s Summary
	err := d.pool.QueryRow(ctx, `
		SELECT
			COUNT(d.id),
			COUNT(*) FILTER (WHERE d.last_seen_at > NOW() - ($1 * INTERVAL '1 second')),
			COUNT(*) FILTER (WHERE d.latest_battery_pct < 20),
			COUNT(DISTINCT d.build_id),
			COUNT(*) FILTER (WHERE dc.kiosk_enabled = true)
		FROM devices d
		LEFT JOIN device_config dc ON dc.device_id = d.id
		WHERE NOT d.hidden
	`, activeSecs).Scan(&s.Total, &s.RecentlyActive, &s.LowBattery, &s.UniqueBuilds, &s.KioskCount)
	return s, err
}

func (d *DB) ListDevices(ctx context.Context, f DeviceFilter, offset, limit int, sort, dir string) ([]Device, error) {
	query, args := d.buildDeviceQuery(f, sort, dir, true, limit, offset)

	rows, err := d.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []Device
	for rows.Next() {
		var dev Device
		if err := rows.Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct, &dev.PollIntervalMs, &dev.KioskEnabled, &dev.KioskPackage, &dev.LatestExtra); err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	return devices, rows.Err()
}

func (d *DB) CountDevices(ctx context.Context, f DeviceFilter) (int, error) {
	query, args := d.buildDeviceQuery(f, "", "", false, 0, 0)

	var count int
	err := d.pool.QueryRow(ctx, query, args...).Scan(&count)
	return count, err
}

func (d *DB) buildDeviceQuery(f DeviceFilter, sort, dir string, selectRows bool, limit, offset int) (string, []interface{}) {
	var args []interface{}
	argN := 1

	var joins []string
	wheres := []string{"true"}
	switch f.Hidden {
	case "include":
		// no filter on hidden
	case "only":
		wheres = append(wheres, "d.hidden")
	default:
		wheres = append(wheres, "NOT d.hidden")
	}

	if f.Search != "" {
		wheres = append(wheres, fmt.Sprintf("d.serial_number ILIKE $%d", argN))
		args = append(args, "%"+f.Search+"%")
		argN++
	}

	if f.GroupID != uuid.Nil {
		joins = append(joins, fmt.Sprintf("JOIN device_groups dg ON dg.device_id = d.id AND dg.group_id = $%d", argN))
		args = append(args, f.GroupID)
		argN++
	}

	if f.ProductionID != uuid.Nil {
		joins = append(joins, fmt.Sprintf("JOIN productions prod ON prod.id = $%d AND d.serial_number LIKE (prod.product_code || prod.model_code || prod.variant || prod.sku || prod.batch || '%%') AND LENGTH(d.serial_number) = 14 AND CAST(SUBSTRING(d.serial_number FROM 10 FOR 5) AS INT) BETWEEN prod.start_sequence AND prod.end_sequence", argN))
		args = append(args, f.ProductionID)
		argN++
	}

	if f.Online == "online" || f.Online == "offline" {
		threshold := f.ActiveThresholdSecs
		if threshold <= 0 {
			threshold = 180
		}
		args = append(args, threshold)
		if f.Online == "online" {
			wheres = append(wheres, fmt.Sprintf("d.last_seen_at > NOW() - ($%d * INTERVAL '1 second')", argN))
		} else {
			wheres = append(wheres, fmt.Sprintf("d.last_seen_at <= NOW() - ($%d * INTERVAL '1 second')", argN))
		}
		argN++
	}

	if f.BuildID != "" {
		wheres = append(wheres, fmt.Sprintf("d.build_id = $%d", argN))
		args = append(args, f.BuildID)
		argN++
	}

	if f.Kiosk == "enabled" {
		wheres = append(wheres, "EXISTS (SELECT 1 FROM device_config dck WHERE dck.device_id = d.id AND dck.kiosk_enabled = true)")
	} else if f.Kiosk == "disabled" {
		wheres = append(wheres, "NOT EXISTS (SELECT 1 FROM device_config dck WHERE dck.device_id = d.id AND dck.kiosk_enabled = true)")
	}

	if selectRows {
		base := `SELECT
			d.id, d.serial_number, d.build_id, d.last_seen_at, d.created_at,
			d.latest_battery_pct AS battery_pct,
			d.poll_interval_ms,
			COALESCE(dc.kiosk_enabled, false),
			COALESCE(dc.kiosk_package, ''),
			d.latest_extra AS latest_extra
		FROM devices d
		LEFT JOIN device_config dc ON dc.device_id = d.id`

		for _, j := range joins {
			base += "\n" + j
		}

		base += "\nWHERE " + strings.Join(wheres, " AND ")

		// Battery filters run against the latest checkin snapshot denormalized onto devices.
		switch f.Battery {
		case "low":
			base += " AND d.latest_battery_pct < 20"
		case "mid":
			base += " AND d.latest_battery_pct BETWEEN 20 AND 49"
		case "ok":
			base += " AND d.latest_battery_pct >= 50"
		}

		if dir != "asc" && dir != "desc" {
			dir = ""
		}

		orderClause := "d.last_seen_at DESC"
		switch sort {
		case "serial":
			if dir == "desc" {
				orderClause = "d.serial_number DESC"
			} else {
				orderClause = "d.serial_number ASC"
			}
		case "build":
			if dir == "desc" {
				orderClause = "d.build_id DESC"
			} else {
				orderClause = "d.build_id ASC"
			}
		case "battery":
			if dir == "desc" {
				orderClause = "d.latest_battery_pct DESC"
			} else {
				orderClause = "d.latest_battery_pct ASC"
			}
		case "ram":
			orderClause = `COALESCE(
				((d.latest_extra->'ram_usage_mb'->>'used')::int * 100) / NULLIF((d.latest_extra->'ram_usage_mb'->>'total')::int, 0),
				0
			) `
			if dir == "desc" {
				orderClause += "DESC"
			} else {
				orderClause += "ASC"
			}
		case "temp":
			if dir == "desc" {
				orderClause = "COALESCE((d.latest_extra->>'battery_temp_c')::numeric, 0) DESC"
			} else {
				orderClause = "COALESCE((d.latest_extra->>'battery_temp_c')::numeric, 0) ASC"
			}
		case "created_at":
			if dir == "asc" {
				orderClause = "d.created_at ASC"
			} else {
				orderClause = "d.created_at DESC"
			}
		case "last_seen":
			if dir == "asc" {
				orderClause = "d.last_seen_at ASC"
			} else {
				orderClause = "d.last_seen_at DESC"
			}
		}
		base += "\nORDER BY " + orderClause

		base += fmt.Sprintf("\nLIMIT $%d OFFSET $%d", argN, argN+1)
		args = append(args, limit, offset)

		return base, args
	}

	// COUNT query
	base := "SELECT COUNT(*) FROM devices d"
	for _, j := range joins {
		base += "\n" + j
	}
	base += "\nWHERE " + strings.Join(wheres, " AND ")
	switch f.Battery {
	case "low":
		base += " AND d.latest_battery_pct < 20"
	case "mid":
		base += " AND d.latest_battery_pct BETWEEN 20 AND 49"
	case "ok":
		base += " AND d.latest_battery_pct >= 50"
	}

	return base, args
}

// GetDistinctBuildIDs returns all distinct non-empty build IDs for non-hidden devices.
func (d *DB) GetDistinctBuildIDs(ctx context.Context) ([]string, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT build_id FROM devices
		WHERE NOT hidden AND build_id != ''
		ORDER BY build_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var builds []string
	for rows.Next() {
		var b string
		if err := rows.Scan(&b); err != nil {
			return nil, err
		}
		builds = append(builds, b)
	}
	return builds, rows.Err()
}

// StreamExportCheckins runs the same query as ExportCheckins but invokes
// fn for each row as it is read, so the caller can write directly to a
// response without buffering the whole result set. If fn returns an error,
// iteration stops and that error is returned.
func (d *DB) StreamExportCheckins(ctx context.Context, deviceIDs []uuid.UUID, start, end time.Time, intervalSec int, fn func(ExportRow) error) error {
	query, args := exportCheckinsQuery(deviceIDs, start, end, intervalSec)
	rows, err := d.pool.Query(ctx, query, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r ExportRow
		var extra []byte
		if err := rows.Scan(&r.SerialNumber, &r.BatteryPct, &r.BuildID, &extra, &r.Timestamp, &r.LastSeenAt); err != nil {
			return err
		}
		if len(extra) > 0 {
			r.Extra = json.RawMessage(extra)
		} else {
			r.Extra = json.RawMessage("{}")
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
}

func exportCheckinsQuery(deviceIDs []uuid.UUID, start, end time.Time, intervalSec int) (string, []interface{}) {
	if intervalSec > 0 {
		return `
			WITH numbered AS (
				SELECT
					d.serial_number,
					c.battery_pct,
					c.build_id,
					c.extra,
					c.created_at,
					d.last_seen_at,
					ROW_NUMBER() OVER (
						PARTITION BY c.device_id,
							floor(EXTRACT(EPOCH FROM c.created_at) / $4)
						ORDER BY c.created_at
					) AS rn
				FROM checkins c
				JOIN devices d ON d.id = c.device_id
				WHERE c.device_id = ANY($1)
				  AND c.created_at >= $2
				  AND c.created_at <= $3
			)
			SELECT serial_number, battery_pct, build_id, extra, created_at, last_seen_at
			FROM numbered WHERE rn = 1
			ORDER BY serial_number, created_at`,
			[]interface{}{deviceIDs, start, end, intervalSec}
	}
	return `
			SELECT d.serial_number, c.battery_pct, c.build_id, c.extra, c.created_at, d.last_seen_at
			FROM checkins c
			JOIN devices d ON d.id = c.device_id
			WHERE c.device_id = ANY($1)
			  AND c.created_at >= $2
			  AND c.created_at <= $3
			ORDER BY d.serial_number, c.created_at`,
		[]interface{}{deviceIDs, start, end}
}

// ExportCheckins returns checkin data for multiple devices within a time range,
// sampled at the given interval in seconds (0 = all rows). Prefer
// StreamExportCheckins for large windows so the rows aren't all buffered.
func (d *DB) ExportCheckins(ctx context.Context, deviceIDs []uuid.UUID, start, end time.Time, intervalSec int) ([]ExportRow, error) {
	var out []ExportRow
	err := d.StreamExportCheckins(ctx, deviceIDs, start, end, intervalSec, func(r ExportRow) error {
		out = append(out, r)
		return nil
	})
	return out, err
}

func (d *DB) GetDevice(ctx context.Context, serial string) (*Device, error) {
	var dev Device
	err := d.pool.QueryRow(ctx, `
		SELECT
			d.id, d.serial_number, d.build_id, d.last_seen_at, d.created_at,
			d.latest_battery_pct AS battery_pct,
			d.poll_interval_ms,
			COALESCE(dc.kiosk_enabled, false),
			COALESCE(dc.kiosk_package, ''),
			d.latest_extra AS latest_extra
		FROM devices d
		LEFT JOIN device_config dc ON dc.device_id = d.id
		WHERE d.serial_number = $1
	`, serial).Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct, &dev.PollIntervalMs, &dev.KioskEnabled, &dev.KioskPackage, &dev.LatestExtra)
	if err != nil {
		return nil, fmt.Errorf("device not found: %w", err)
	}
	return &dev, nil
}

// GetDeviceByID fetches a single device by its UUID.
func (d *DB) GetDeviceByID(ctx context.Context, id uuid.UUID) (*Device, error) {
	var dev Device
	err := d.pool.QueryRow(ctx, `
		SELECT
			d.id, d.serial_number, d.build_id, d.last_seen_at, d.created_at,
			d.latest_battery_pct AS battery_pct,
			d.poll_interval_ms,
			COALESCE(dc.kiosk_enabled, false),
			COALESCE(dc.kiosk_package, ''),
			d.latest_extra AS latest_extra
		FROM devices d
		LEFT JOIN device_config dc ON dc.device_id = d.id
		WHERE d.id = $1
	`, id).Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct, &dev.PollIntervalMs, &dev.KioskEnabled, &dev.KioskPackage, &dev.LatestExtra)
	if err != nil {
		return nil, fmt.Errorf("device not found: %w", err)
	}
	return &dev, nil
}

// HideDevice marks a device as hidden. It stays in the DB but is excluded from
// listings and summaries. The flag is cleared automatically on the next check-in.
func (d *DB) HideDevice(ctx context.Context, serial string) error {
	_, err := d.pool.Exec(ctx, `UPDATE devices SET hidden = true WHERE serial_number = $1`, serial)
	return err
}

func (d *DB) BulkHideDevices(ctx context.Context, serials []string) error {
	_, err := d.pool.Exec(ctx, `UPDATE devices SET hidden = true WHERE serial_number = ANY($1)`, serials)
	return err
}

func (d *DB) SetDevicePollInterval(ctx context.Context, serial string, intervalMs int) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE devices SET poll_interval_ms = $2 WHERE serial_number = $1
	`, serial, intervalMs)
	return err
}

func (d *DB) GetLatestCheckin(ctx context.Context, deviceID uuid.UUID) (*Checkin, error) {
	var c Checkin
	err := d.pool.QueryRow(ctx, `
		SELECT id, device_id, battery_pct, build_id, extra, created_at
		FROM checkins WHERE device_id = $1
		ORDER BY created_at DESC LIMIT 1
	`, deviceID).Scan(&c.ID, &c.DeviceID, &c.BatteryPct, &c.BuildID, &c.Extra, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
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

func (d *DB) GetCheckinsForDay(ctx context.Context, deviceID uuid.UUID, day time.Time) ([]Checkin, error) {
	start := time.Date(day.Year(), day.Month(), day.Day(), 0, 0, 0, 0, time.UTC)
	end := start.Add(24 * time.Hour)
	rows, err := d.pool.Query(ctx, `
		SELECT id, device_id, battery_pct, build_id, extra, created_at
		FROM checkins
		WHERE device_id = $1 AND created_at >= $2 AND created_at < $3
		ORDER BY created_at DESC
	`, deviceID, start, end)
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

func (d *DB) GetCheckinsForDuration(ctx context.Context, deviceID uuid.UUID, since time.Time) ([]Checkin, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, device_id, battery_pct, build_id, extra, created_at
		FROM checkins
		WHERE device_id = $1 AND created_at >= $2
		ORDER BY created_at DESC
	`, deviceID, since)
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

func (d *DB) AddDevicesToGroup(ctx context.Context, serials []string, groupID uuid.UUID) error {
	if len(serials) == 0 {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_groups (device_id, group_id)
		SELECT id, $2 FROM devices WHERE serial_number = ANY($1)
		ON CONFLICT DO NOTHING
	`, serials, groupID)
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
			d.latest_battery_pct AS battery_pct,
			d.poll_interval_ms,
			COALESCE(dc.kiosk_enabled, false),
			COALESCE(dc.kiosk_package, '')
		FROM devices d
		JOIN device_groups dg ON dg.device_id = d.id
		LEFT JOIN device_config dc ON dc.device_id = d.id
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
		if err := rows.Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct, &dev.PollIntervalMs, &dev.KioskEnabled, &dev.KioskPackage); err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	return devices, rows.Err()
}

// ── Productions ───────────────────────────────────────────────────────────────

type ProductionParams struct {
	Name          string
	ProductCode   string
	ModelCode     string
	Variant       string
	SKU           string
	Batch         string
	BatchMonth    int
	BatchYear     int
	StartSequence int
	EndSequence   int
	Notes         string
}

func (d *DB) CreateProduction(ctx context.Context, p ProductionParams) (*Production, error) {
	var prod Production
	err := d.pool.QueryRow(ctx, `
		INSERT INTO productions (name, product_code, model_code, variant, sku, batch, batch_month, batch_year, start_sequence, end_sequence, notes)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
		RETURNING id, name, product_code, model_code, variant, sku, batch, batch_month, batch_year, start_sequence, end_sequence, notes, created_at
	`, p.Name, p.ProductCode, p.ModelCode, p.Variant, p.SKU, p.Batch, p.BatchMonth, p.BatchYear, p.StartSequence, p.EndSequence, p.Notes).
		Scan(&prod.ID, &prod.Name, &prod.ProductCode, &prod.ModelCode, &prod.Variant, &prod.SKU,
			&prod.Batch, &prod.BatchMonth, &prod.BatchYear, &prod.StartSequence, &prod.EndSequence, &prod.Notes, &prod.CreatedAt)
	if err != nil {
		return nil, err
	}
	prod.Total = prod.EndSequence - prod.StartSequence + 1
	return &prod, nil
}

func (d *DB) ListProductions(ctx context.Context) ([]Production, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT
			p.id, p.name, p.product_code, p.model_code, p.variant, p.sku, p.batch,
			p.batch_month, p.batch_year, p.start_sequence, p.end_sequence, p.notes, p.created_at,
			p.end_sequence - p.start_sequence + 1 AS total,
			COUNT(d.id) AS ever_connected,
			COUNT(d.id) FILTER (WHERE d.last_seen_at > NOW() - INTERVAL '3 minutes') AS online
		FROM productions p
		LEFT JOIN devices d ON
			d.serial_number LIKE (p.product_code || p.model_code || p.variant || p.sku || p.batch || '%')
			AND LENGTH(d.serial_number) = 14
			AND SUBSTRING(d.serial_number FROM 10 FOR 5) ~ '^[0-9]+$'
			AND CAST(SUBSTRING(d.serial_number FROM 10 FOR 5) AS INT) BETWEEN p.start_sequence AND p.end_sequence
		GROUP BY p.id
		ORDER BY p.created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var productions []Production
	for rows.Next() {
		var p Production
		if err := rows.Scan(&p.ID, &p.Name, &p.ProductCode, &p.ModelCode, &p.Variant, &p.SKU, &p.Batch,
			&p.BatchMonth, &p.BatchYear, &p.StartSequence, &p.EndSequence, &p.Notes, &p.CreatedAt,
			&p.Total, &p.EverConnected, &p.Online); err != nil {
			return nil, err
		}
		productions = append(productions, p)
	}
	return productions, rows.Err()
}

func (d *DB) GetProduction(ctx context.Context, id uuid.UUID) (*Production, error) {
	var p Production
	err := d.pool.QueryRow(ctx, `
		SELECT
			p.id, p.name, p.product_code, p.model_code, p.variant, p.sku, p.batch,
			p.batch_month, p.batch_year, p.start_sequence, p.end_sequence, p.notes, p.created_at,
			p.end_sequence - p.start_sequence + 1 AS total,
			COUNT(d.id) AS ever_connected,
			COUNT(d.id) FILTER (WHERE d.last_seen_at > NOW() - INTERVAL '3 minutes') AS online
		FROM productions p
		LEFT JOIN devices d ON
			d.serial_number LIKE (p.product_code || p.model_code || p.variant || p.sku || p.batch || '%')
			AND LENGTH(d.serial_number) = 14
			AND SUBSTRING(d.serial_number FROM 10 FOR 5) ~ '^[0-9]+$'
			AND CAST(SUBSTRING(d.serial_number FROM 10 FOR 5) AS INT) BETWEEN p.start_sequence AND p.end_sequence
		WHERE p.id = $1
		GROUP BY p.id
	`, id).Scan(&p.ID, &p.Name, &p.ProductCode, &p.ModelCode, &p.Variant, &p.SKU, &p.Batch,
		&p.BatchMonth, &p.BatchYear, &p.StartSequence, &p.EndSequence, &p.Notes, &p.CreatedAt,
		&p.Total, &p.EverConnected, &p.Online)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (d *DB) DeleteProduction(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM productions WHERE id = $1`, id)
	return err
}

// GetProductionDevices returns connected devices for a production.
// Devices that have never connected are not in this list but can be inferred from total - ever_connected.
func (d *DB) GetProductionDevices(ctx context.Context, id uuid.UUID) ([]ProductionDevice, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT
			d.serial_number,
			d.id,
			d.build_id,
			d.latest_battery_pct,
			d.last_seen_at,
			d.created_at,
			CASE
				WHEN d.last_seen_at > NOW() - INTERVAL '3 minutes' THEN 'online'
				ELSE 'offline'
			END AS connection_status
		FROM productions p
		JOIN devices d ON
			d.serial_number LIKE (p.product_code || p.model_code || p.variant || p.sku || p.batch || '%')
			AND LENGTH(d.serial_number) = 14
			AND SUBSTRING(d.serial_number FROM 10 FOR 5) ~ '^[0-9]+$'
			AND CAST(SUBSTRING(d.serial_number FROM 10 FOR 5) AS INT) BETWEEN p.start_sequence AND p.end_sequence
		WHERE p.id = $1
		ORDER BY d.serial_number
	`, id)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []ProductionDevice
	for rows.Next() {
		var dev ProductionDevice
		if err := rows.Scan(&dev.Serial, &dev.DeviceID, &dev.BuildID, &dev.BatteryPct, &dev.LastSeenAt, &dev.CreatedAt, &dev.ConnectionStatus); err != nil {
			return nil, err
		}
		devices = append(devices, dev)
	}
	return devices, rows.Err()
}

func (d *DB) SearchDevicesBySerial(ctx context.Context, query string, limit int) ([]Device, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT
			d.id, d.serial_number, d.build_id, d.last_seen_at, d.created_at,
			d.latest_battery_pct AS battery_pct,
			d.poll_interval_ms,
			COALESCE(dc.kiosk_enabled, false),
			COALESCE(dc.kiosk_package, ''),
			d.latest_extra AS latest_extra
		FROM devices d
		LEFT JOIN device_config dc ON dc.device_id = d.id
		WHERE d.serial_number ILIKE $1
		ORDER BY
			CASE WHEN lower(d.serial_number) = lower($2) THEN 0 ELSE 1 END,
			d.serial_number
		LIMIT $3
	`, "%"+query+"%", query, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var devices []Device
	for rows.Next() {
		var dev Device
		if err := rows.Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct, &dev.PollIntervalMs, &dev.KioskEnabled, &dev.KioskPackage, &dev.LatestExtra); err != nil {
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

// GetAllDeviceIDs returns the IDs of every registered device.
func (d *DB) GetAllDeviceIDs(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `SELECT id FROM devices`)
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
func (d *DB) CreateCommand(ctx context.Context, cmdType, apkURL string, payload json.RawMessage, targetType string, targetIDs []uuid.UUID) (*Command, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	var cmd Command
	if len(payload) == 0 {
		payload = json.RawMessage("{}")
	}
	err = tx.QueryRow(ctx, `
		INSERT INTO commands (type, apk_url, payload, target_type)
		VALUES ($1, $2, $3, $4)
		RETURNING id, type, apk_url, payload, target_type, created_at
	`, cmdType, apkURL, payload, targetType).Scan(&cmd.ID, &cmd.Type, &cmd.ApkURL, &cmd.Payload, &cmd.TargetType, &cmd.CreatedAt)
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

func (d *DB) DeleteCommand(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM commands WHERE id = $1`, id)
	return err
}

func (d *DB) ListCommands(ctx context.Context) ([]Command, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, type, apk_url, payload, target_type, created_at
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
		if err := rows.Scan(&c.ID, &c.Type, &c.ApkURL, &c.Payload, &c.TargetType, &c.CreatedAt); err != nil {
			return nil, err
		}
		cmds = append(cmds, c)
	}
	return cmds, rows.Err()
}

func (d *DB) GetCommand(ctx context.Context, id uuid.UUID) (*Command, error) {
	var c Command
	err := d.pool.QueryRow(ctx, `
		SELECT id, type, apk_url, payload, target_type, created_at
		FROM commands WHERE id = $1
	`, id).Scan(&c.ID, &c.Type, &c.ApkURL, &c.Payload, &c.TargetType, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
}

type CommandDeliverySummary struct {
	CommandID uuid.UUID
	Pending   int
	Delivered int
	Completed int
	Failed    int
}

func (d *DB) GetCommandDeliverySummaries(ctx context.Context) (map[uuid.UUID]CommandDeliverySummary, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT command_id, status, COUNT(*) FROM command_deliveries GROUP BY command_id, status
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]CommandDeliverySummary)
	for rows.Next() {
		var cid uuid.UUID
		var status string
		var count int
		if err := rows.Scan(&cid, &status, &count); err != nil {
			return nil, err
		}
		s := out[cid]
		s.CommandID = cid
		switch status {
		case "pending":
			s.Pending = count
		case "delivered":
			s.Delivered = count
		case "completed":
			s.Completed = count
		case "failed":
			s.Failed = count
		}
		out[cid] = s
	}
	return out, rows.Err()
}

func (d *DB) GetCommandTargetSerials(ctx context.Context, commandID uuid.UUID) ([]string, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.serial_number FROM devices d
		JOIN command_targets ct ON ct.target_id = d.id
		WHERE ct.command_id = $1
	`, commandID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var s string
		if err := rows.Scan(&s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// GetCommandTargetIDs returns the target UUIDs stored in command_targets for a command.
func (d *DB) GetCommandTargetIDs(ctx context.Context, commandID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `SELECT target_id FROM command_targets WHERE command_id = $1`, commandID)
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

// GetPendingCommandsForDevice returns commands not yet delivered/acked for this device.
func (d *DB) GetPendingCommandsForDevice(ctx context.Context, deviceID uuid.UUID) ([]Command, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT c.id, c.type, c.apk_url, c.payload, c.target_type, c.created_at
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
			AND cs.status IN ('delivered', 'installed', 'failed', 'completed')
		)
		AND (
			c.type NOT IN ('shell', 'screenshot', 'reboot')
			OR c.created_at > NOW() - INTERVAL '5 minutes'
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
		if err := rows.Scan(&c.ID, &c.Type, &c.ApkURL, &c.Payload, &c.TargetType, &c.CreatedAt); err != nil {
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

func (d *DB) commandTargetsDevice(ctx context.Context, commandID, deviceID uuid.UUID) (bool, error) {
	var exists bool
	err := d.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1
			FROM commands c
			WHERE c.id = $1
			AND (
				c.target_type = 'all'
				OR (c.target_type = 'devices' AND EXISTS (
					SELECT 1 FROM command_targets ct
					WHERE ct.command_id = c.id AND ct.target_id = $2
				))
				OR (c.target_type = 'groups' AND EXISTS (
					SELECT 1 FROM command_targets ct
					JOIN device_groups dg ON dg.group_id = ct.target_id
					WHERE ct.command_id = c.id AND dg.device_id = $2
				))
			)
		)
	`, commandID, deviceID).Scan(&exists)
	return exists, err
}

// AckCommand lets a device report installed or failed for a command.
func (d *DB) AckCommand(ctx context.Context, commandID, deviceID uuid.UUID, status string) error {
	targeted, err := d.commandTargetsDevice(ctx, commandID, deviceID)
	if err != nil {
		return err
	}
	if !targeted {
		return ErrCommandNotTargeted
	}

	_, err = d.pool.Exec(ctx, `
		INSERT INTO command_status (command_id, device_id, status, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (command_id, device_id) DO UPDATE
			SET status = EXCLUDED.status, updated_at = NOW()
	`, commandID, deviceID, status)
	return err
}

type DeviceCommand struct {
	ID         uuid.UUID       `json:"id"`
	Type       string          `json:"type"`
	ApkURL     string          `json:"apk_url"`
	Payload    json.RawMessage `json:"payload"`
	TargetType string          `json:"target_type"`
	CreatedAt  time.Time       `json:"created_at"`
	Status     string          `json:"status"`
	UpdatedAt  time.Time       `json:"updated_at"`
	Output     string          `json:"output"`
}

// GetDeviceCommands returns all commands targeting a device with their status.
func (d *DB) GetDeviceCommands(ctx context.Context, deviceID uuid.UUID, expirySec int) ([]DeviceCommand, error) {
	if expirySec <= 0 {
		expirySec = 300
	}
	rows, err := d.pool.Query(ctx, fmt.Sprintf(`
		SELECT c.id, c.type, c.apk_url, c.payload, c.target_type, c.created_at,
		       CASE
		         WHEN cs.status IS NOT NULL THEN cs.status
		         WHEN c.type IN ('shell', 'screenshot', 'reboot')
		              AND c.created_at <= NOW() - INTERVAL '%d seconds' THEN 'expired'
		         ELSE 'pending'
		       END AS status,
		       COALESCE(cs.updated_at, c.created_at) AS updated_at,
		       COALESCE(cr.output, '') AS output
		FROM commands c
		LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = $1
		LEFT JOIN command_results cr ON cr.command_id = c.id AND cr.device_id = $1
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
	`, expirySec), deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DeviceCommand
	for rows.Next() {
		var dc DeviceCommand
		if err := rows.Scan(&dc.ID, &dc.Type, &dc.ApkURL, &dc.Payload, &dc.TargetType, &dc.CreatedAt, &dc.Status, &dc.UpdatedAt, &dc.Output); err != nil {
			return nil, err
		}
		out = append(out, dc)
	}
	return out, rows.Err()
}

// GetCommandDeliveries returns per-device status for a command.
// Shell/screenshot/reboot deliveries that are still 'delivered' or 'pending'
// after 5 minutes from command creation are reported as 'expired'.
func (d *DB) GetCommandDeliveries(ctx context.Context, commandID uuid.UUID, expirySec int) ([]CommandDelivery, error) {
	if expirySec <= 0 {
		expirySec = 300
	}
	rows, err := d.pool.Query(ctx, fmt.Sprintf(`
		SELECT cs.device_id,
		       d.serial_number,
		       CASE
		         WHEN c.type IN ('shell', 'screenshot', 'reboot')
		              AND cs.status IN ('pending', 'delivered')
		              AND c.created_at <= NOW() - INTERVAL '%d seconds'
		           THEN 'expired'
		         ELSE cs.status
		       END AS status,
		       cs.updated_at,
		       COALESCE(cr.output, '') AS output,
		       d.last_seen_at
		FROM command_status cs
		JOIN devices d ON d.id = cs.device_id
		JOIN commands c ON c.id = cs.command_id
		LEFT JOIN command_results cr ON cr.command_id = cs.command_id AND cr.device_id = cs.device_id
		WHERE cs.command_id = $1
		ORDER BY cs.updated_at DESC
	`, expirySec), commandID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CommandDelivery
	for rows.Next() {
		var cd CommandDelivery
		if err := rows.Scan(&cd.DeviceID, &cd.SerialNumber, &cd.Status, &cd.UpdatedAt, &cd.Output, &cd.LastSeenAt); err != nil {
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

// ── Device Packages ───────────────────────────────────────────────────────────

type DevicePackage struct {
	PackageName string    `json:"package_name"`
	AppName     string    `json:"app_name"`
	VersionName string    `json:"version_name"`
	UpdatedAt   time.Time `json:"updated_at"`
}

type FleetPackage struct {
	PackageName string `json:"package_name"`
	AppName     string `json:"app_name"`
	DeviceCount int    `json:"device_count"`
	Versions    string `json:"versions"`
}

// UpsertDevicePackages replaces all packages for a device atomically.
func (d *DB) UpsertDevicePackages(ctx context.Context, deviceID uuid.UUID, packages []DevicePackage) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, `DELETE FROM device_packages WHERE device_id = $1`, deviceID); err != nil {
		return err
	}

	if len(packages) > 0 {
		names := make([]string, len(packages))
		appNames := make([]string, len(packages))
		versions := make([]string, len(packages))
		for i, p := range packages {
			names[i] = p.PackageName
			appNames[i] = p.AppName
			versions[i] = p.VersionName
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO device_packages (device_id, package_name, app_name, version_name)
			SELECT $1, unnest($2::text[]), unnest($3::text[]), unnest($4::text[])
			ON CONFLICT (device_id, package_name) DO UPDATE SET app_name = EXCLUDED.app_name, version_name = EXCLUDED.version_name, updated_at = NOW()
		`, deviceID, names, appNames, versions); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func (d *DB) GetDevicePackages(ctx context.Context, deviceID uuid.UUID) ([]DevicePackage, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT package_name, app_name, version_name, updated_at
		FROM device_packages
		WHERE device_id = $1
		ORDER BY package_name
	`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DevicePackage
	for rows.Next() {
		var p DevicePackage
		if err := rows.Scan(&p.PackageName, &p.AppName, &p.VersionName, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *DB) SearchFleetPackages(ctx context.Context, query string) ([]FleetPackage, error) {
	var q string
	var rows pgx.Rows
	var err error
	if query != "" {
		q = "%" + query + "%"
		rows, err = d.pool.Query(ctx, `
			SELECT
				dp.package_name,
				COALESCE(MAX(dp.app_name), '') AS app_name,
				COUNT(DISTINCT dp.device_id) AS device_count,
				string_agg(DISTINCT dp.version_name, ', ' ORDER BY dp.version_name) AS versions
			FROM device_packages dp
			WHERE dp.package_name ILIKE $1 OR dp.app_name ILIKE $1
			GROUP BY dp.package_name
			ORDER BY device_count DESC, dp.package_name
			LIMIT 200
		`, q)
	} else {
		rows, err = d.pool.Query(ctx, `
			SELECT
				dp.package_name,
				COALESCE(MAX(dp.app_name), '') AS app_name,
				COUNT(DISTINCT dp.device_id) AS device_count,
				string_agg(DISTINCT dp.version_name, ', ' ORDER BY dp.version_name) AS versions
			FROM device_packages dp
			GROUP BY dp.package_name
			ORDER BY device_count DESC, dp.package_name
			LIMIT 200
		`)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []FleetPackage
	for rows.Next() {
		var p FleetPackage
		if err := rows.Scan(&p.PackageName, &p.AppName, &p.DeviceCount, &p.Versions); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ── Device Config / Kiosk ─────────────────────────────────────────────────────

func (d *DB) GetOrCreateDeviceConfig(ctx context.Context, deviceID uuid.UUID) (*DeviceConfig, error) {
	// Ensure a row exists, then read it.
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_config (device_id) VALUES ($1)
		ON CONFLICT (device_id) DO NOTHING
	`, deviceID)
	if err != nil {
		return nil, err
	}
	var cfg DeviceConfig
	err = d.pool.QueryRow(ctx, `
		SELECT device_id, kiosk_enabled, kiosk_package, kiosk_features, updated_at
		FROM device_config WHERE device_id = $1
	`, deviceID).Scan(&cfg.DeviceID, &cfg.KioskEnabled, &cfg.KioskPackage, &cfg.KioskFeatures, &cfg.UpdatedAt)
	if err != nil {
		return nil, err
	}
	return &cfg, nil
}

func (d *DB) SetKioskConfig(ctx context.Context, deviceID uuid.UUID, enabled bool, pkg string, features int) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_config (device_id, kiosk_enabled, kiosk_package, kiosk_features, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (device_id) DO UPDATE
			SET kiosk_enabled  = EXCLUDED.kiosk_enabled,
			    kiosk_package  = EXCLUDED.kiosk_package,
			    kiosk_features = EXCLUDED.kiosk_features,
			    updated_at     = NOW()
	`, deviceID, enabled, pkg, features)
	return err
}

func (d *DB) SetKioskConfigForDevices(ctx context.Context, deviceIDs []uuid.UUID, enabled bool, pkg string, features int) error {
	if len(deviceIDs) == 0 {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_config (device_id, kiosk_enabled, kiosk_package, kiosk_features, updated_at)
		SELECT d.id, $2, $3, $4, NOW()
		FROM devices d
		WHERE d.id = ANY($1)
		ON CONFLICT (device_id) DO UPDATE
			SET kiosk_enabled  = EXCLUDED.kiosk_enabled,
			    kiosk_package  = EXCLUDED.kiosk_package,
			    kiosk_features = EXCLUDED.kiosk_features,
			    updated_at     = NOW()
	`, deviceIDs, enabled, pkg, features)
	return err
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

type CommandResult struct {
	ID        uuid.UUID `json:"id"`
	CommandID uuid.UUID `json:"command_id"`
	DeviceID  uuid.UUID `json:"device_id"`
	Output    string    `json:"output"`
	CreatedAt time.Time `json:"created_at"`
}

func (d *DB) SaveCommandResult(ctx context.Context, commandID, deviceID uuid.UUID, output string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO command_results (command_id, device_id, output)
		VALUES ($1, $2, $3)
		ON CONFLICT (command_id, device_id) DO UPDATE SET output = EXCLUDED.output
	`, commandID, deviceID, output)
	return err
}

// GetDeviceIDsByGroupIDs returns the distinct device IDs that belong to any of the given groups.
func (d *DB) GetDeviceIDsByGroupIDs(ctx context.Context, groupIDs []uuid.UUID) ([]uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT device_id FROM device_groups WHERE group_id = ANY($1)
	`, groupIDs)
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

type AuditEntry struct {
	ID        int64     `json:"id"`
	CreatedAt time.Time `json:"created_at"`
	Actor     string    `json:"actor"`
	Action    string    `json:"action"`
	Target    string    `json:"target"`
	Detail    string    `json:"detail"`
}

func (d *DB) InsertAudit(ctx context.Context, actor, action, target, detail string) error {
	_, err := d.pool.Exec(ctx,
		`INSERT INTO audit_log (actor, action, target, detail) VALUES ($1, $2, $3, $4)`,
		actor, action, target, detail)
	return err
}

func (d *DB) ListAudit(ctx context.Context, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := d.pool.Query(ctx,
		`SELECT id, created_at, actor, action, target, detail FROM audit_log ORDER BY created_at DESC LIMIT $1`, limit)
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

// HideStaleDevices hides visible devices not seen within the last `days` days.
func (d *DB) HideStaleDevices(ctx context.Context, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	tag, err := d.pool.Exec(ctx, fmt.Sprintf(
		`UPDATE devices SET hidden = true WHERE NOT hidden AND last_seen_at < NOW() - INTERVAL '%d days'`, days))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// PruneCheckins deletes check-in rows older than `days` days.
func (d *DB) PruneCheckins(ctx context.Context, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	tag, err := d.pool.Exec(ctx, fmt.Sprintf(
		`DELETE FROM checkins WHERE created_at < NOW() - INTERVAL '%d days'`, days))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// RollupDailyStats aggregates one calendar day of checkins into device_daily_stats
// (one row per device for that day). Idempotent: re-running refreshes the day, so it
// is safe to call repeatedly for the current (still-accumulating) day. Returns the
// number of device-day rows written. `day` is interpreted at date granularity.
func (d *DB) RollupDailyStats(ctx context.Context, day time.Time) (int64, error) {
	dayStr := day.Format("2006-01-02")
	tag, err := d.pool.Exec(ctx, `
		INSERT INTO device_daily_stats AS s (
			device_id, day, checkin_count, battery_min, battery_max, battery_avg,
			temp_max, ram_pct_peak, charging_frac, online_minutes, build_id,
			first_seen_at, last_seen_at, computed_at)
		SELECT
			c.device_id,
			$1::date,
			COUNT(*),
			MIN(c.battery_pct),
			MAX(c.battery_pct),
			AVG(c.battery_pct)::real,
			MAX((c.extra->>'battery_temp_c')::numeric)::real,
			MAX(COALESCE(
				((c.extra->'ram_usage_mb'->>'used')::numeric * 100)
					/ NULLIF((c.extra->'ram_usage_mb'->>'total')::numeric, 0),
				0))::smallint,
			AVG(CASE WHEN (c.extra->>'charging')::boolean THEN 1 ELSE 0 END)::real,
			COUNT(DISTINCT date_trunc('minute', c.created_at)),
			(ARRAY_AGG(c.build_id ORDER BY c.created_at DESC))[1],
			MIN(c.created_at),
			MAX(c.created_at),
			NOW()
		FROM checkins c
		WHERE c.created_at >= $1::date AND c.created_at < ($1::date + INTERVAL '1 day')
		GROUP BY c.device_id
		ON CONFLICT (device_id, day) DO UPDATE SET
			checkin_count  = EXCLUDED.checkin_count,
			battery_min    = EXCLUDED.battery_min,
			battery_max    = EXCLUDED.battery_max,
			battery_avg    = EXCLUDED.battery_avg,
			temp_max       = EXCLUDED.temp_max,
			ram_pct_peak   = EXCLUDED.ram_pct_peak,
			charging_frac  = EXCLUDED.charging_frac,
			online_minutes = EXCLUDED.online_minutes,
			build_id       = EXCLUDED.build_id,
			first_seen_at  = EXCLUDED.first_seen_at,
			last_seen_at   = EXCLUDED.last_seen_at,
			computed_at    = EXCLUDED.computed_at
	`, dayStr)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// BackfillDailyStats rolls up every calendar day present in checkins that has no
// rows yet in device_daily_stats. Used once at startup so historical telemetry is
// captured before the hourly rollup takes over the current/recent days. Returns the
// number of days processed.
func (d *DB) BackfillDailyStats(ctx context.Context) (int, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT c.created_at::date AS day
		FROM checkins c
		WHERE NOT EXISTS (
			SELECT 1 FROM device_daily_stats s WHERE s.day = c.created_at::date)
		ORDER BY day`)
	if err != nil {
		return 0, err
	}
	var days []time.Time
	for rows.Next() {
		var day time.Time
		if err := rows.Scan(&day); err != nil {
			rows.Close()
			return 0, err
		}
		days = append(days, day)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return 0, err
	}
	for _, day := range days {
		if _, err := d.RollupDailyStats(ctx, day); err != nil {
			return 0, err
		}
	}
	return len(days), nil
}

// DeviceDailyStat is one rolled-up day of telemetry for a device. Aggregate columns
// are pointers so a day with no data for a metric serializes as null rather than 0.
type DeviceDailyStat struct {
	Day           time.Time `json:"day"`
	CheckinCount  int       `json:"checkin_count"`
	BatteryMin    *int      `json:"battery_min"`
	BatteryMax    *int      `json:"battery_max"`
	BatteryAvg    *float32  `json:"battery_avg"`
	TempMax       *float32  `json:"temp_max"`
	RAMPctPeak    *int      `json:"ram_pct_peak"`
	ChargingFrac  *float32  `json:"charging_frac"`
	OnlineMinutes int       `json:"online_minutes"`
	BuildID       string    `json:"build_id"`
}

// GetDeviceDailyStats returns the last `days` days of rolled-up stats for a device,
// oldest first. Defaults to 30 days.
func (d *DB) GetDeviceDailyStats(ctx context.Context, deviceID uuid.UUID, days int) ([]DeviceDailyStat, error) {
	if days <= 0 {
		days = 30
	}
	rows, err := d.pool.Query(ctx, `
		SELECT day, checkin_count, battery_min, battery_max, battery_avg,
		       temp_max, ram_pct_peak, charging_frac, online_minutes, build_id
		FROM device_daily_stats
		WHERE device_id = $1 AND day >= CURRENT_DATE - ($2::int - 1)
		ORDER BY day`, deviceID, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []DeviceDailyStat
	for rows.Next() {
		var s DeviceDailyStat
		if err := rows.Scan(&s.Day, &s.CheckinCount, &s.BatteryMin, &s.BatteryMax,
			&s.BatteryAvg, &s.TempMax, &s.RAMPctPeak, &s.ChargingFrac,
			&s.OnlineMinutes, &s.BuildID); err != nil {
			return nil, err
		}
		stats = append(stats, s)
	}
	return stats, rows.Err()
}

// GroupDailyStat is one day of stats aggregated across all devices in a group.
// Aggregates are pointers so a day/metric with no data serializes as null, not 0.
type GroupDailyStat struct {
	Day            time.Time `json:"day"`
	DeviceCount    int       `json:"device_count"`    // devices with data that day
	BatteryMin     *int      `json:"battery_min"`     // lowest daily min across the group
	BatteryMax     *int      `json:"battery_max"`     // highest daily max across the group
	BatteryAvg     *float32  `json:"battery_avg"`     // mean of per-device daily averages
	TempMax        *float32  `json:"temp_max"`        // hottest device that day
	ChargingFrac   *float32  `json:"charging_frac"`   // mean charging coverage
	OnlineMinAvg   *float32  `json:"online_min_avg"`  // mean online minutes per device
	DistinctBuilds int       `json:"distinct_builds"` // build_id spread (fleet consistency)
}

// GetGroupDailyStats returns the last `days` days of stats rolled up across every
// device in the group, oldest first. Defaults to 30 days. Groups are treated as
// generic device buckets — no assumption about what a group represents.
func (d *DB) GetGroupDailyStats(ctx context.Context, groupID uuid.UUID, days int) ([]GroupDailyStat, error) {
	if days <= 0 {
		days = 30
	}
	rows, err := d.pool.Query(ctx, `
		SELECT
			s.day,
			COUNT(DISTINCT s.device_id),
			MIN(s.battery_min),
			MAX(s.battery_max),
			AVG(s.battery_avg)::real,
			MAX(s.temp_max)::real,
			AVG(s.charging_frac)::real,
			AVG(s.online_minutes)::real,
			COUNT(DISTINCT NULLIF(s.build_id, ''))
		FROM device_daily_stats s
		JOIN device_groups dg ON dg.device_id = s.device_id
		WHERE dg.group_id = $1 AND s.day >= CURRENT_DATE - ($2::int - 1)
		GROUP BY s.day
		ORDER BY s.day`, groupID, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []GroupDailyStat
	for rows.Next() {
		var s GroupDailyStat
		if err := rows.Scan(&s.Day, &s.DeviceCount, &s.BatteryMin, &s.BatteryMax,
			&s.BatteryAvg, &s.TempMax, &s.ChargingFrac, &s.OnlineMinAvg,
			&s.DistinctBuilds); err != nil {
			return nil, err
		}
		stats = append(stats, s)
	}
	return stats, rows.Err()
}

// GroupHealth is a per-group health scorecard (Tier 3). Pointer fields are null when
// the group has no rolled-up data in the window. Score/ScoreClass are computed in Go.
type GroupHealth struct {
	GroupID        uuid.UUID `json:"group_id"`
	Name           string    `json:"name"`
	DeviceCount    int       `json:"device_count"`
	OfflineCount   int       `json:"offline_count"`
	OpenCritical   int       `json:"open_critical"`
	OpenWarning    int       `json:"open_warning"`
	BatteryAvg     *float64  `json:"battery_avg"`    // recent avg daily peak battery (overnight fullness)
	BatteryDelta   *float64  `json:"battery_delta"`  // recent minus prior week (negative = declining)
	ChargingAvg    *float64  `json:"charging_avg"`   // recent avg charging coverage (0-1)
	TempMax        *float64  `json:"temp_max"`       // hottest device in the window
	DistinctBuilds int       `json:"distinct_builds"`
	Score          int       `json:"score"`       // 0-100, higher is healthier
	ScoreClass     string    `json:"score_class"` // ok | warn | danger (for badge styling)
}

// healthScore derives a 0-100 score and class from a group's metrics. Heuristic and
// explainable: start at 100 and subtract penalties for offline devices, open alerts,
// poor charging, battery decline, overheating, and firmware fragmentation.
func (g *GroupHealth) computeScore() {
	score := 100
	if g.DeviceCount > 0 {
		score -= int(float64(g.OfflineCount) / float64(g.DeviceCount) * 40) // up to -40 if all offline
	}
	score -= g.OpenCritical * 15
	score -= g.OpenWarning * 4
	if g.ChargingAvg != nil && *g.ChargingAvg < 0.3 {
		score -= 15
	}
	if g.BatteryDelta != nil && *g.BatteryDelta < -10 {
		score -= 15
	}
	if g.TempMax != nil && *g.TempMax >= 45 {
		score -= 15
	}
	if g.DistinctBuilds > 1 {
		score -= (g.DistinctBuilds - 1) * 5
	}
	if score < 0 {
		score = 0
	}
	if score > 100 {
		score = 100
	}
	g.Score = score
	switch {
	case score >= 80:
		g.ScoreClass = "ok"
	case score >= 50:
		g.ScoreClass = "warn"
	default:
		g.ScoreClass = "danger"
	}
}

// GetGroupHealth returns a health scorecard per group, worst score first. activeSecs is
// the offline threshold (a device quieter than this counts as offline). Recent window is
// the last 7 days; battery delta compares it to the prior 7 days.
func (d *DB) GetGroupHealth(ctx context.Context, activeSecs int) ([]GroupHealth, error) {
	if activeSecs <= 0 {
		activeSecs = 180
	}
	rows, err := d.pool.Query(ctx, `
		WITH recent AS (
			SELECT dg.group_id,
				AVG(s.battery_max)   AS battery_avg,
				AVG(s.charging_frac) AS charging_avg,
				MAX(s.temp_max)      AS temp_max,
				COUNT(DISTINCT NULLIF(s.build_id, '')) AS builds
			FROM device_daily_stats s
			JOIN device_groups dg ON dg.device_id = s.device_id
			WHERE s.day > CURRENT_DATE - 7
			GROUP BY dg.group_id
		),
		prior AS (
			SELECT dg.group_id, AVG(s.battery_max) AS battery_avg
			FROM device_daily_stats s
			JOIN device_groups dg ON dg.device_id = s.device_id
			WHERE s.day <= CURRENT_DATE - 7 AND s.day > CURRENT_DATE - 14
			GROUP BY dg.group_id
		),
		devs AS (
			SELECT dg.group_id,
				COUNT(*) AS device_count,
				COUNT(*) FILTER (WHERE d.last_seen_at < NOW() - ($1 * INTERVAL '1 second')) AS offline_count
			FROM device_groups dg
			JOIN devices d ON d.id = dg.device_id AND NOT d.hidden
			GROUP BY dg.group_id
		),
		al AS (
			SELECT dg.group_id,
				COUNT(*) FILTER (WHERE a.severity = 'critical')  AS crit,
				COUNT(*) FILTER (WHERE a.severity <> 'critical') AS warn
			FROM alerts a
			JOIN device_groups dg ON dg.device_id = a.device_id
			WHERE a.status <> 'resolved'
			GROUP BY dg.group_id
		)
		SELECT g.id, g.name,
			COALESCE(devs.device_count, 0), COALESCE(devs.offline_count, 0),
			COALESCE(al.crit, 0), COALESCE(al.warn, 0),
			recent.battery_avg, (recent.battery_avg - prior.battery_avg),
			recent.charging_avg, recent.temp_max, COALESCE(recent.builds, 0)
		FROM groups g
		LEFT JOIN devs   ON devs.group_id   = g.id
		LEFT JOIN recent ON recent.group_id = g.id
		LEFT JOIN prior  ON prior.group_id  = g.id
		LEFT JOIN al     ON al.group_id     = g.id
		ORDER BY g.name`, activeSecs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []GroupHealth
	for rows.Next() {
		var g GroupHealth
		if err := rows.Scan(&g.GroupID, &g.Name, &g.DeviceCount, &g.OfflineCount,
			&g.OpenCritical, &g.OpenWarning, &g.BatteryAvg, &g.BatteryDelta,
			&g.ChargingAvg, &g.TempMax, &g.DistinctBuilds); err != nil {
			return nil, err
		}
		g.computeScore()
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Worst (lowest score) first so the page surfaces venues needing attention.
	sort.Slice(out, func(i, j int) bool { return out[i].Score < out[j].Score })
	return out, nil
}

// ── Alerts ────────────────────────────────────────────────────────────────────

// AlertRule is a rule definition the evaluator checks each housekeeping pass.
type AlertRule struct {
	ID        uuid.UUID       `json:"id"`
	Type      string          `json:"type"`
	Name      string          `json:"name"`
	Enabled   bool            `json:"enabled"`
	Params    json.RawMessage `json:"params"`
	ScopeType string          `json:"scope_type"`
	ScopeID   *uuid.UUID      `json:"scope_id"`
	CreatedAt time.Time       `json:"created_at"`
}

// Alert is a fired alert instance. Serial is joined from devices for display.
type Alert struct {
	ID         uuid.UUID       `json:"id"`
	RuleID     *uuid.UUID      `json:"rule_id"`
	Type       string          `json:"type"`
	DeviceID   *uuid.UUID      `json:"device_id"`
	Serial     string          `json:"serial"`
	Severity   string          `json:"severity"`
	Status     string          `json:"status"`
	Summary    string          `json:"summary"`
	Detail     json.RawMessage `json:"detail"`
	FiredAt    time.Time       `json:"fired_at"`
	ResolvedAt *time.Time      `json:"resolved_at"`
	UpdatedAt  time.Time       `json:"updated_at"`
}

// defaultAlertRules are seeded once (per type) by EnsureDefaultRules so alerting
// works out of the box; admins can edit/disable/delete them afterward.
var defaultAlertRules = []struct {
	Type, Name, Params string
}{
	{"offline", "Device offline", `{"offline_minutes":30,"quiet_start":0,"quiet_end":6}`},
	{"overheating", "Battery overheating", `{"temp_c":45}`},
	{"no_overnight_charge", "Did not charge overnight", `{"min_full_pct":90,"max_charge_frac":0.3}`},
	{"battery_health_decline", "Battery health declining", `{"drop_pct":15,"window_days":7}`},
}

// EnsureDefaultRules inserts each default rule only if no rule of that type exists.
func (d *DB) EnsureDefaultRules(ctx context.Context) error {
	for _, r := range defaultAlertRules {
		if _, err := d.pool.Exec(ctx, `
			INSERT INTO alert_rules (type, name, params)
			SELECT $1, $2, $3::jsonb
			WHERE NOT EXISTS (SELECT 1 FROM alert_rules WHERE type = $1)
		`, r.Type, r.Name, r.Params); err != nil {
			return err
		}
	}
	return nil
}

// ListAlertRules returns alert rules, optionally only the enabled ones.
func (d *DB) ListAlertRules(ctx context.Context, onlyEnabled bool) ([]AlertRule, error) {
	q := `SELECT id, type, name, enabled, params, scope_type, scope_id, created_at FROM alert_rules`
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
		if err := rows.Scan(&r.ID, &r.Type, &r.Name, &r.Enabled, &r.Params, &r.ScopeType, &r.ScopeID, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateAlertRule sets a rule's enabled flag and threshold params.
func (d *DB) UpdateAlertRule(ctx context.Context, id uuid.UUID, enabled bool, params json.RawMessage) error {
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE alert_rules SET enabled = $2, params = $3::jsonb WHERE id = $1
	`, id, enabled, params)
	return err
}

// CreateAlertIfAbsent inserts a new alert unless a non-resolved one already exists
// for (type, device). Returns true only when a row was actually created, so callers
// broadcast/notify exactly once per occurrence.
func (d *DB) CreateAlertIfAbsent(ctx context.Context, ruleID *uuid.UUID, typ string, deviceID uuid.UUID, severity, summary string, detail any) (bool, error) {
	detailJSON := []byte("{}")
	if detail != nil {
		b, err := json.Marshal(detail)
		if err != nil {
			return false, err
		}
		detailJSON = b
	}
	var id uuid.UUID
	err := d.pool.QueryRow(ctx, `
		INSERT INTO alerts (rule_id, type, device_id, severity, summary, detail)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)
		ON CONFLICT (type, device_id) WHERE status <> 'resolved' DO NOTHING
		RETURNING id
	`, ruleID, typ, deviceID, severity, summary, detailJSON).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
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

// ListAlerts returns alerts (newest first), optionally filtered by status, with the
// device serial joined. limit <= 0 means 200.
func (d *DB) ListAlerts(ctx context.Context, status string, limit int) ([]Alert, error) {
	if limit <= 0 {
		limit = 200
	}
	rows, err := d.pool.Query(ctx, `
		SELECT a.id, a.rule_id, a.type, a.device_id, COALESCE(d.serial_number, ''),
		       a.severity, a.status, a.summary, a.detail, a.fired_at, a.resolved_at, a.updated_at
		FROM alerts a
		LEFT JOIN devices d ON d.id = a.device_id
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
			&a.Severity, &a.Status, &a.Summary, &a.Detail, &a.FiredAt, &a.ResolvedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// CountOpenAlerts returns the number of alerts in the 'open' status (for nav badge).
func (d *DB) CountOpenAlerts(ctx context.Context) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM alerts WHERE status = 'open'`).Scan(&n)
	return n, err
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

// AIUsageDay is one day of AI token usage. Used for the Settings usage chart.
type AIUsageDay struct {
	Day          time.Time `json:"day"`
	Calls        int       `json:"calls"`
	InputTokens  int64     `json:"input_tokens"`
	OutputTokens int64     `json:"output_tokens"`
}

// AIUsageTotals is the all-time AI usage rollup for the Settings summary.
type AIUsageTotals struct {
	Calls        int   `json:"calls"`
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
}

// RecordAIUsage adds one call's token counts to today's ai_usage row (upsert).
func (d *DB) RecordAIUsage(ctx context.Context, inputTokens, outputTokens int64) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO ai_usage (day, calls, input_tokens, output_tokens)
		VALUES (CURRENT_DATE, 1, $1, $2)
		ON CONFLICT (day) DO UPDATE SET
			calls         = ai_usage.calls + 1,
			input_tokens  = ai_usage.input_tokens + EXCLUDED.input_tokens,
			output_tokens = ai_usage.output_tokens + EXCLUDED.output_tokens
	`, inputTokens, outputTokens)
	return err
}

// GetAIUsageDaily returns per-day usage for the last `days` days, oldest first.
func (d *DB) GetAIUsageDaily(ctx context.Context, days int) ([]AIUsageDay, error) {
	if days <= 0 {
		days = 30
	}
	rows, err := d.pool.Query(ctx, `
		SELECT day, calls, input_tokens, output_tokens
		FROM ai_usage
		WHERE day >= CURRENT_DATE - ($1::int - 1)
		ORDER BY day`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AIUsageDay
	for rows.Next() {
		var u AIUsageDay
		if err := rows.Scan(&u.Day, &u.Calls, &u.InputTokens, &u.OutputTokens); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetAIUsageTotals returns the all-time AI usage rollup.
func (d *DB) GetAIUsageTotals(ctx context.Context) (AIUsageTotals, error) {
	var t AIUsageTotals
	err := d.pool.QueryRow(ctx, `
		SELECT COALESCE(SUM(calls),0), COALESCE(SUM(input_tokens),0), COALESCE(SUM(output_tokens),0)
		FROM ai_usage`).Scan(&t.Calls, &t.InputTokens, &t.OutputTokens)
	return t, err
}

// AISummary is a cached AI summary for one scope.
type AISummary struct {
	Summary     string    `json:"summary"`
	Model       string    `json:"model"`
	GeneratedAt time.Time `json:"generated_at"`
}

// SetAISummary upserts the cached summary for a scope (e.g. "fleet").
func (d *DB) SetAISummary(ctx context.Context, scope, summary, model string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO ai_summary (scope, summary, model, generated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (scope) DO UPDATE SET
			summary = EXCLUDED.summary, model = EXCLUDED.model, generated_at = NOW()
	`, scope, summary, model)
	return err
}

// GetAISummary returns the cached summary for a scope. A missing row yields a zero
// AISummary and a nil error (no summary generated yet).
func (d *DB) GetAISummary(ctx context.Context, scope string) (AISummary, error) {
	var s AISummary
	err := d.pool.QueryRow(ctx, `
		SELECT summary, model, generated_at FROM ai_summary WHERE scope = $1
	`, scope).Scan(&s.Summary, &s.Model, &s.GeneratedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return AISummary{}, nil
	}
	return s, err
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
}

// param reads a numeric threshold from a rule's params JSON, falling back to def.
func param(p map[string]float64, key string, def float64) float64 {
	if v, ok := p[key]; ok {
		return v
	}
	return def
}

// gmtOffset parses a device-reported timezone like "GMT+5"/"GMT-3"/"GMT+0" to an
// hour offset. Anything unrecognized is treated as UTC (0).
func gmtOffset(tz string) int {
	if !strings.HasPrefix(tz, "GMT") {
		return 0
	}
	n, err := strconv.Atoi(strings.TrimPrefix(tz, "GMT"))
	if err != nil {
		return 0
	}
	return n
}

// inQuiet reports whether local hour h falls in the [start,end) quiet window,
// supporting windows that wrap past midnight (e.g. 22→6). start==end means "never".
func inQuiet(h, start, end int) bool {
	if start == end {
		return false
	}
	if start < end {
		return h >= start && h < end
	}
	return h >= start || h < end
}

// EvaluateAlerts runs every enabled fleet-scoped rule against device_daily_stats,
// creating alerts for violators (deduped) and resolving alerts whose condition has
// cleared. Returns counts of created and resolved alerts. Called from housekeeping.
func (d *DB) EvaluateAlerts(ctx context.Context) (created []AlertNotification, resolved int, err error) {
	rules, err := d.ListAlertRules(ctx, true)
	if err != nil {
		return nil, 0, err
	}
	for _, r := range rules {
		if r.ScopeType != "fleet" {
			continue // group/device scoping not implemented yet
		}
		var p map[string]float64
		if len(r.Params) > 0 {
			_ = json.Unmarshal(r.Params, &p)
		}
		hits, severity, e := d.detectRule(ctx, r.Type, p)
		if e != nil {
			return created, resolved, e
		}

		ids := make([]uuid.UUID, 0, len(hits))
		ruleID := r.ID
		for _, h := range hits {
			ids = append(ids, h.DeviceID)
			ok, e := d.CreateAlertIfAbsent(ctx, &ruleID, r.Type, h.DeviceID, severity, h.Summary, h.Detail)
			if e != nil {
				return created, resolved, e
			}
			if ok {
				created = append(created, AlertNotification{r.Type, severity, h.Summary, h.Serial})
			}
		}
		// Resolve any open alert of this type whose device is no longer violating.
		tag, e := d.pool.Exec(ctx, `
			UPDATE alerts SET status = 'resolved', resolved_at = NOW(), updated_at = NOW()
			WHERE type = $1 AND status <> 'resolved' AND device_id <> ALL($2::uuid[])
		`, r.Type, ids)
		if e != nil {
			return created, resolved, e
		}
		resolved += int(tag.RowsAffected())
	}
	return created, resolved, nil
}

// detectRule returns the devices currently violating a rule type plus the severity
// to record. Unknown rule types return no hits.
func (d *DB) detectRule(ctx context.Context, typ string, p map[string]float64) ([]alertHit, string, error) {
	switch typ {
	case "offline":
		mins := int(param(p, "offline_minutes", 30))
		qs := int(param(p, "quiet_start", 0))
		qe := int(param(p, "quiet_end", 6))
		rows, err := d.pool.Query(ctx, `
			SELECT id, serial_number, last_seen_at, COALESCE(latest_extra->>'timezone', '')
			FROM devices
			WHERE NOT hidden AND last_seen_at < NOW() - ($1 * INTERVAL '1 minute')`, mins)
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		now := time.Now().UTC()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial, tz string
			var lastSeen time.Time
			if err := rows.Scan(&id, &serial, &lastSeen, &tz); err != nil {
				return nil, "critical", err
			}
			// Skip devices in their local quiet/overnight window (expected offline).
			localHour := ((now.Hour()+gmtOffset(tz))%24 + 24) % 24
			if inQuiet(localHour, qs, qe) {
				continue
			}
			down := int(now.Sub(lastSeen).Minutes())
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Offline — last check-in %dm ago", down),
				map[string]any{"offline_minutes": down, "last_seen": lastSeen, "timezone": tz}})
		}
		return hits, "critical", rows.Err()

	case "overheating":
		limit := param(p, "temp_c", 45)
		rows, err := d.pool.Query(ctx, `
			SELECT s.device_id, dv.serial_number, s.temp_max
			FROM device_daily_stats s JOIN devices dv ON dv.id = s.device_id
			WHERE s.day = CURRENT_DATE AND s.temp_max >= $1`, limit)
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var temp float32
			if err := rows.Scan(&id, &serial, &temp); err != nil {
				return nil, "critical", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Battery reached %.0f°C today (limit %.0f°C)", temp, limit),
				map[string]any{"temp_c": temp, "limit_c": limit}})
		}
		return hits, "critical", rows.Err()

	case "no_overnight_charge":
		minFull := param(p, "min_full_pct", 90)
		maxFrac := param(p, "max_charge_frac", 0.3)
		rows, err := d.pool.Query(ctx, `
			SELECT s.device_id, dv.serial_number, s.battery_max, s.charging_frac
			FROM device_daily_stats s JOIN devices dv ON dv.id = s.device_id
			WHERE s.day = CURRENT_DATE - 1 AND s.battery_max < $1 AND COALESCE(s.charging_frac, 0) < $2`,
			minFull, maxFrac)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var bmax int
			var frac float32
			if err := rows.Scan(&id, &serial, &bmax, &frac); err != nil {
				return nil, "warning", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Only reached %d%% and charged %.0f%% of yesterday", bmax, frac*100),
				map[string]any{"battery_max": bmax, "charging_frac": frac}})
		}
		return hits, "warning", rows.Err()

	case "battery_health_decline":
		win := int(param(p, "window_days", 7))
		drop := param(p, "drop_pct", 15)
		rows, err := d.pool.Query(ctx, `
			SELECT t.device_id, dv.serial_number, t.recent, t.prior FROM (
				SELECT device_id,
					AVG(battery_max) FILTER (WHERE day > CURRENT_DATE - ($1::int))                                       AS recent,
					AVG(battery_max) FILTER (WHERE day <= CURRENT_DATE - ($1::int) AND day > CURRENT_DATE - (2 * $1::int)) AS prior
				FROM device_daily_stats
				WHERE day > CURRENT_DATE - (2 * $1::int)
				GROUP BY device_id
			) t
			JOIN devices dv ON dv.id = t.device_id
			WHERE t.recent IS NOT NULL AND t.prior IS NOT NULL AND (t.prior - t.recent) >= $2`,
			win, drop)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var recent, prior float64
			if err := rows.Scan(&id, &serial, &recent, &prior); err != nil {
				return nil, "warning", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Daily peak battery fell %.0f points vs the prior %dd", prior-recent, win),
				map[string]any{"recent_avg": recent, "prior_avg": prior, "window_days": win}})
		}
		return hits, "warning", rows.Err()
	}
	return nil, "warning", nil
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

type DBStats struct {
	Devices       int64 `json:"devices"`
	Checkins      int64 `json:"checkins"`
	Commands      int64 `json:"commands"`
	LogcatResults int64 `json:"logcat_results"`
}

func (d *DB) TableStats(ctx context.Context) (DBStats, error) {
	var s DBStats
	err := d.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM devices),
		       (SELECT count(*) FROM checkins),
		       (SELECT count(*) FROM commands),
		       (SELECT count(*) FROM logcat_results)
	`).Scan(&s.Devices, &s.Checkins, &s.Commands, &s.LogcatResults)
	return s, err
}

const migrationSQL = `
CREATE TABLE IF NOT EXISTS devices (
	id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	serial_number TEXT        NOT NULL UNIQUE,
	build_id      TEXT        NOT NULL DEFAULT '',
	latest_battery_pct SMALLINT    NOT NULL DEFAULT 0,
	latest_extra  JSONB       NOT NULL DEFAULT '{}',
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

CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE INDEX IF NOT EXISTS idx_checkins_device_id  ON checkins(device_id);
CREATE INDEX IF NOT EXISTS idx_checkins_device_created_at ON checkins(device_id, created_at DESC);
CREATE INDEX IF NOT EXISTS idx_checkins_created_at ON checkins(created_at DESC);
CREATE INDEX IF NOT EXISTS idx_devices_last_seen   ON devices(last_seen_at DESC);
CREATE INDEX IF NOT EXISTS idx_devices_serial_trgm ON devices USING GIN (serial_number gin_trgm_ops);
CREATE INDEX IF NOT EXISTS idx_device_groups_group_id_device_id ON device_groups(group_id, device_id);

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

ALTER TABLE commands ADD COLUMN IF NOT EXISTS payload JSONB NOT NULL DEFAULT '{}';

CREATE TABLE IF NOT EXISTS command_results (
	id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	command_id UUID NOT NULL REFERENCES commands(id) ON DELETE CASCADE,
	device_id  UUID NOT NULL REFERENCES devices(id)  ON DELETE CASCADE,
	output     TEXT NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	UNIQUE(command_id, device_id)
);

CREATE INDEX IF NOT EXISTS idx_command_results_command_id ON command_results(command_id);

CREATE TABLE IF NOT EXISTS device_packages (
	device_id    UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	package_name TEXT NOT NULL,
	app_name     TEXT NOT NULL DEFAULT '',
	version_name TEXT NOT NULL DEFAULT '',
	updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (device_id, package_name)
);

CREATE INDEX IF NOT EXISTS idx_device_packages_device_id   ON device_packages(device_id);
CREATE INDEX IF NOT EXISTS idx_device_packages_package_name ON device_packages(package_name);

ALTER TABLE device_packages ADD COLUMN IF NOT EXISTS app_name TEXT NOT NULL DEFAULT '';

ALTER TABLE devices ADD COLUMN IF NOT EXISTS poll_interval_ms INTEGER NOT NULL DEFAULT 30000;
ALTER TABLE devices ADD COLUMN IF NOT EXISTS latest_battery_pct SMALLINT NOT NULL DEFAULT 0;
ALTER TABLE devices ADD COLUMN IF NOT EXISTS latest_extra JSONB NOT NULL DEFAULT '{}';

-- NOTE: A one-time backfill of devices.latest_battery_pct / latest_extra from the
-- newest checkin per device used to live here (a DISTINCT ON over the whole
-- checkins table). It ran on EVERY startup inside this migration batch, doing a
-- full scan + sort of checkins — instant on a fresh DB but ~30 min and IO-bound
-- on production where checkins has millions of rows, blocking the server from
-- ever binding its port. It is also redundant: UpsertCheckin keeps both columns
-- current on every checkin. Removed. If a fresh backfill is ever needed again,
-- run it once manually, not from the recurring startup migration.

CREATE TABLE IF NOT EXISTS device_config (
	device_id      UUID    PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
	kiosk_enabled  BOOLEAN NOT NULL DEFAULT false,
	kiosk_package  TEXT    NOT NULL DEFAULT '',
	kiosk_features INTEGER NOT NULL DEFAULT 1,
	updated_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS ota_packages (
	id              SERIAL      PRIMARY KEY,
	build_id        TEXT        NOT NULL UNIQUE,
	release_date    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	update_url      TEXT        NOT NULL,
	payload_offset  BIGINT      NOT NULL DEFAULT 0,
	payload_size    BIGINT      NOT NULL DEFAULT 0,
	payload_headers TEXT[]      NOT NULL DEFAULT '{}',
	created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE ota_packages ADD COLUMN IF NOT EXISTS type TEXT NOT NULL DEFAULT 'full';
ALTER TABLE ota_packages ADD COLUMN IF NOT EXISTS target_build_id TEXT NOT NULL DEFAULT '';
ALTER TABLE ota_packages ADD COLUMN IF NOT EXISTS source_build_id TEXT NOT NULL DEFAULT '';

-- Migrate existing build_id to target_build_id
UPDATE ota_packages SET target_build_id = build_id WHERE target_build_id = '';

-- Allow multiple packages with the same build_id
ALTER TABLE ota_packages DROP CONSTRAINT IF EXISTS ota_packages_build_id_key;

CREATE TABLE IF NOT EXISTS updates (
	id              SERIAL      PRIMARY KEY,
	ota_package_id  INTEGER     NOT NULL REFERENCES ota_packages(id) ON DELETE CASCADE,
	reboot_behavior TEXT        NOT NULL DEFAULT 'immediate',
	scheduled_time  TIMESTAMPTZ,
	created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE updates ADD COLUMN IF NOT EXISTS scheduled_time TIMESTAMPTZ;

ALTER TABLE groups  ADD COLUMN IF NOT EXISTS ota_package_id INTEGER REFERENCES ota_packages(id) ON DELETE SET NULL;
ALTER TABLE devices ADD COLUMN IF NOT EXISTS ota_package_id INTEGER REFERENCES ota_packages(id) ON DELETE SET NULL;

ALTER TABLE updates ADD COLUMN IF NOT EXISTS status TEXT NOT NULL DEFAULT 'pending';

CREATE TABLE IF NOT EXISTS update_devices (
	update_id  INTEGER   NOT NULL REFERENCES updates(id) ON DELETE CASCADE,
	device_id  UUID      NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	status     TEXT      NOT NULL DEFAULT 'pending',
	PRIMARY KEY (update_id, device_id)
);

ALTER TABLE devices ADD COLUMN IF NOT EXISTS hidden BOOLEAN NOT NULL DEFAULT false;

CREATE TABLE IF NOT EXISTS productions (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name           TEXT NOT NULL,
    product_code   TEXT NOT NULL,
    model_code     TEXT NOT NULL,
    variant        CHAR(1) NOT NULL DEFAULT '0',
    sku            CHAR(2) NOT NULL DEFAULT 'AA',
    batch          CHAR(2) NOT NULL,
    batch_month    INT NOT NULL,
    batch_year     INT NOT NULL,
    start_sequence INT NOT NULL,
    end_sequence   INT NOT NULL,
    notes          TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_productions_batch ON productions(batch);
CREATE INDEX IF NOT EXISTS idx_productions_prefix ON productions(product_code, model_code, variant, sku, batch);

ALTER TABLE ota_packages ADD COLUMN IF NOT EXISTS changelog TEXT NOT NULL DEFAULT '';
ALTER TABLE ota_packages ADD COLUMN IF NOT EXISTS status    TEXT NOT NULL DEFAULT 'active';

CREATE TABLE IF NOT EXISTS users (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    role          TEXT NOT NULL CHECK (role IN ('viewer', 'operator')),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

CREATE TABLE IF NOT EXISTS audit_log (
    id         BIGSERIAL PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    actor      TEXT NOT NULL DEFAULT '',
    action     TEXT NOT NULL,
    target     TEXT NOT NULL DEFAULT '',
    detail     TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_audit_created ON audit_log(created_at DESC);

-- Per-device per-day rollup of checkin telemetry (Tier 1 descriptive analytics).
-- Populated by RollupDailyStats; queried for trends so we never scan raw checkins.
CREATE TABLE IF NOT EXISTS device_daily_stats (
    device_id      UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    day            DATE NOT NULL,
    checkin_count  INTEGER NOT NULL DEFAULT 0,
    battery_min    SMALLINT,
    battery_max    SMALLINT,
    battery_avg    REAL,
    temp_max       REAL,
    ram_pct_peak   SMALLINT,
    charging_frac  REAL,          -- fraction of checkins reporting charging=true
    online_minutes INTEGER NOT NULL DEFAULT 0, -- distinct minute buckets with a checkin
    build_id       TEXT NOT NULL DEFAULT '',    -- last build_id seen that day
    first_seen_at  TIMESTAMPTZ,
    last_seen_at   TIMESTAMPTZ,
    computed_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (device_id, day)
);
CREATE INDEX IF NOT EXISTS idx_device_daily_stats_day ON device_daily_stats(day DESC);

-- Alert rules: a rule type + JSONB params (thresholds), optionally scoped. The
-- evaluator (RunHousekeeping) reads enabled rules and fires alerts against them.
CREATE TABLE IF NOT EXISTS alert_rules (
    id         UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    type       TEXT NOT NULL,                 -- overheating | no_overnight_charge | battery_health_decline | ...
    name       TEXT NOT NULL DEFAULT '',
    enabled    BOOLEAN NOT NULL DEFAULT true,
    params     JSONB NOT NULL DEFAULT '{}',   -- thresholds, e.g. {"temp_c":45}
    scope_type TEXT NOT NULL DEFAULT 'fleet', -- fleet | group | device
    scope_id   UUID,                          -- group_id or device_id when scoped
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Fired alert instances. At most one OPEN alert per (type, device) via the partial
-- unique index below, so re-evaluating a still-true condition does not duplicate.
CREATE TABLE IF NOT EXISTS alerts (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    rule_id     UUID REFERENCES alert_rules(id) ON DELETE SET NULL,
    type        TEXT NOT NULL,
    device_id   UUID REFERENCES devices(id) ON DELETE CASCADE,
    severity    TEXT NOT NULL DEFAULT 'warning', -- info | warning | critical
    status      TEXT NOT NULL DEFAULT 'open',    -- open | acknowledged | resolved
    summary     TEXT NOT NULL DEFAULT '',
    detail      JSONB NOT NULL DEFAULT '{}',
    fired_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    resolved_at TIMESTAMPTZ,
    updated_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_alerts_open_unique ON alerts(type, device_id) WHERE status <> 'resolved';
CREATE INDEX IF NOT EXISTS idx_alerts_status ON alerts(status, fired_at DESC);
CREATE INDEX IF NOT EXISTS idx_alerts_device ON alerts(device_id);

-- AI analysis token usage, rolled up per day (one row per calendar day). Each AI
-- call increments calls and adds its input/output token counts.
CREATE TABLE IF NOT EXISTS ai_usage (
    day           DATE PRIMARY KEY,
    calls         INTEGER NOT NULL DEFAULT 0,
    input_tokens  BIGINT  NOT NULL DEFAULT 0,
    output_tokens BIGINT  NOT NULL DEFAULT 0
);

-- Cached AI summaries, one row per scope (e.g. 'fleet'). Regenerated periodically
-- by housekeeping so the dashboard can show it without an on-demand API call.
CREATE TABLE IF NOT EXISTS ai_summary (
    scope        TEXT PRIMARY KEY,
    summary      TEXT NOT NULL DEFAULT '',
    model        TEXT NOT NULL DEFAULT '',
    generated_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
`

// ── OTA Packages ──────────────────────────────────────────────────────────────

func (d *DB) CreateOTAPackage(ctx context.Context, typ, targetBuildID, sourceBuildID, updateURL, changelog string, releaseDate time.Time) (*OTAPackage, error) {
	var p OTAPackage
	err := d.pool.QueryRow(ctx, `
		INSERT INTO ota_packages (type, target_build_id, source_build_id, build_id, update_url, changelog, release_date)
		VALUES ($1, $2, $3, $2, $4, $5, $6)
		RETURNING id, type, target_build_id, source_build_id, release_date, update_url, changelog, status, created_at
	`, typ, targetBuildID, sourceBuildID, updateURL, changelog, releaseDate).
		Scan(&p.ID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.Changelog, &p.Status, &p.CreatedAt)
	return &p, err
}

func (d *DB) ListOTAPackages(ctx context.Context) ([]OTAPackage, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT p.id, p.type, p.target_build_id, p.source_build_id, p.release_date, p.update_url, p.changelog, p.status, p.created_at,
		       COUNT(u.id) AS deployment_count
		FROM ota_packages p
		LEFT JOIN updates u ON u.ota_package_id = p.id
		GROUP BY p.id
		ORDER BY p.created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OTAPackage
	for rows.Next() {
		var p OTAPackage
		if err := rows.Scan(&p.ID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.Changelog, &p.Status, &p.CreatedAt, &p.DeploymentCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *DB) GetOTAPackage(ctx context.Context, id int) (*OTAPackage, error) {
	var p OTAPackage
	err := d.pool.QueryRow(ctx, `
		SELECT p.id, p.type, p.target_build_id, p.source_build_id, p.release_date, p.update_url, p.changelog, p.status, p.created_at,
		       COUNT(u.id) AS deployment_count
		FROM ota_packages p
		LEFT JOIN updates u ON u.ota_package_id = p.id
		WHERE p.id = $1
		GROUP BY p.id
	`, id).Scan(&p.ID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.Changelog, &p.Status, &p.CreatedAt, &p.DeploymentCount)
	if err != nil {
		return nil, err
	}
	return &p, nil
}

func (d *DB) SetOTAPackageStatus(ctx context.Context, id int, status string) error {
	_, err := d.pool.Exec(ctx, `UPDATE ota_packages SET status = $2 WHERE id = $1`, id, status)
	return err
}

func (d *DB) DeleteOTAPackage(ctx context.Context, id int) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM ota_packages WHERE id = $1`, id)
	return err
}

// ── Updates (Deployments) ─────────────────────────────────────────────────────

func (d *DB) CreateUpdate(ctx context.Context, otaPackageID int, rebootBehavior string, scheduledTime *time.Time) (*Update, error) {
	var u Update
	err := d.pool.QueryRow(ctx, `
		INSERT INTO updates (ota_package_id, reboot_behavior, scheduled_time, status)
		VALUES ($1, $2, $3, 'pending')
		RETURNING id, ota_package_id, reboot_behavior, scheduled_time, status, created_at
	`, otaPackageID, rebootBehavior, scheduledTime).
		Scan(&u.ID, &u.OtaPackageID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt)
	return &u, err
}

func (d *DB) ListDeploymentsByPackage(ctx context.Context, pkgID int) ([]Update, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT u.id, u.ota_package_id, u.reboot_behavior, u.scheduled_time, u.status, u.created_at,
		       COUNT(ud.device_id) AS device_total,
		       COUNT(CASE WHEN ud.status = 'installed' THEN 1 END) AS device_installed
		FROM updates u
		LEFT JOIN update_devices ud ON ud.update_id = u.id
		WHERE u.ota_package_id = $1
		GROUP BY u.id
		ORDER BY u.created_at DESC
	`, pkgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Update
	for rows.Next() {
		var u Update
		if err := rows.Scan(&u.ID, &u.OtaPackageID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt,
			&u.DeviceTotal, &u.DeviceInstalled); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

func (d *DB) ListUpdates(ctx context.Context) ([]Update, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT u.id, u.ota_package_id, u.reboot_behavior, u.scheduled_time, u.status, u.created_at,
		       p.id, p.type, p.target_build_id, p.source_build_id, p.release_date, p.update_url, p.changelog, p.status, p.created_at
		FROM updates u
		JOIN ota_packages p ON p.id = u.ota_package_id
		ORDER BY u.created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Update
	for rows.Next() {
		var u Update
		var p OTAPackage
		if err := rows.Scan(&u.ID, &u.OtaPackageID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt,
			&p.ID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.Changelog, &p.Status, &p.CreatedAt); err != nil {
			return nil, err
		}
		u.OtaPackage = &p
		out = append(out, u)
	}
	return out, rows.Err()
}

func (d *DB) GetUpdate(ctx context.Context, id int) (*Update, error) {
	var u Update
	var p OTAPackage
	err := d.pool.QueryRow(ctx, `
		SELECT u.id, u.ota_package_id, u.reboot_behavior, u.scheduled_time, u.status, u.created_at,
		       p.id, p.type, p.target_build_id, p.source_build_id, p.release_date, p.update_url, p.changelog, p.status, p.created_at
		FROM updates u
		JOIN ota_packages p ON p.id = u.ota_package_id
		WHERE u.id = $1
	`, id).Scan(&u.ID, &u.OtaPackageID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt,
		&p.ID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.Changelog, &p.Status, &p.CreatedAt)
	if err != nil {
		return nil, err
	}
	u.OtaPackage = &p
	return &u, nil
}

func (d *DB) DeleteUpdate(ctx context.Context, id int) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM updates WHERE id = $1`, id)
	return err
}

func (d *DB) UpdateDeploymentRebootSettings(ctx context.Context, id int, rebootBehavior string, scheduledTime *time.Time) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE updates
		SET reboot_behavior = $2, scheduled_time = $3
		WHERE id = $1
	`, id, rebootBehavior, scheduledTime)
	return err
}

// SendUpdateToDevices adds devices as targets of an update. Skips devices that
// already have an active (non-complete) update. Sets the update status to "active".
func (d *DB) SendUpdateToDevices(ctx context.Context, updateID int, deviceIDs []uuid.UUID) error {
	for _, did := range deviceIDs {
		_, _ = d.pool.Exec(ctx, `
			INSERT INTO update_devices (update_id, device_id, status)
			VALUES ($1, $2, 'pending')
			ON CONFLICT DO NOTHING
		`, updateID, did)
	}
	_, err := d.pool.Exec(ctx, `UPDATE updates SET status = 'active' WHERE id = $1 AND status = 'pending'`, updateID)
	return err
}

// DeviceHasActiveUpdate returns true if the device is a target of any non-complete update.
func (d *DB) DeviceHasActiveUpdate(ctx context.Context, deviceID uuid.UUID) (bool, error) {
	var exists bool
	err := d.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM update_devices ud
			JOIN updates u ON u.id = ud.update_id
			WHERE ud.device_id = $1 AND u.status = 'active' AND ud.status != 'installed'
		)
	`, deviceID).Scan(&exists)
	return exists, err
}

// ResolveUpdateForDevice returns the active Update (with joined OTAPackage) for a device
// from the update_devices table. Returns nil if no active update is assigned.
func (d *DB) ResolveUpdateForDevice(ctx context.Context, deviceID uuid.UUID) (*Update, error) {
	var u Update
	var p OTAPackage
	err := d.pool.QueryRow(ctx, `
		SELECT u.id, u.ota_package_id, u.reboot_behavior, u.scheduled_time, u.status, u.created_at,
		       p.id, p.type, p.target_build_id, p.source_build_id, p.release_date, p.update_url, p.changelog, p.status, p.created_at
		FROM update_devices ud
		JOIN updates u ON u.id = ud.update_id
		JOIN ota_packages p ON p.id = u.ota_package_id
		WHERE ud.device_id = $1 AND u.status = 'active' AND ud.status != 'installed' AND p.status = 'active'
		ORDER BY u.created_at DESC
		LIMIT 1
	`, deviceID).Scan(&u.ID, &u.OtaPackageID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt,
		&p.ID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.Changelog, &p.Status, &p.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	u.OtaPackage = &p
	return &u, nil
}

// SetUpdateDeviceStatus updates the status of a device within an update.
func (d *DB) SetUpdateDeviceStatus(ctx context.Context, updateID int, deviceID uuid.UUID, status string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE update_devices SET status = $3 WHERE update_id = $1 AND device_id = $2
	`, updateID, deviceID, status)
	return err
}

// CheckAndCompleteUpdate marks an update as "complete" if all its targets are "installed".
func (d *DB) CheckAndCompleteUpdate(ctx context.Context, updateID int) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE updates SET status = 'complete'
		WHERE id = $1 AND status = 'active'
		AND NOT EXISTS (
			SELECT 1 FROM update_devices WHERE update_id = $1 AND status != 'installed'
		)
	`, updateID)
	return err
}

// GetUpdateTargets returns the device targets for an update.
func (d *DB) GetUpdateTargets(ctx context.Context, updateID int) ([]UpdateTarget, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT ud.update_id, ud.device_id, d.serial_number, d.build_id, ud.status
		FROM update_devices ud
		JOIN devices d ON d.id = ud.device_id
		WHERE ud.update_id = $1
		ORDER BY d.serial_number
	`, updateID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []UpdateTarget
	for rows.Next() {
		var t UpdateTarget
		if err := rows.Scan(&t.UpdateID, &t.DeviceID, &t.SerialNumber, &t.BuildID, &t.Status); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// HasPendingOTACommand returns true if the device already has an OTA command
// that hasn't been completed or failed yet.
// HasPendingOTACommand returns true if the device already has an OTA command
// that is in-progress (pending/delivered/downloaded) OR that failed within the
// last hour. This prevents the server from re-sending OTA commands on every
// check-in after a failure.
func (d *DB) HasPendingOTACommand(ctx context.Context, deviceID uuid.UUID) (bool, error) {
	var exists bool
	err := d.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM commands c
			JOIN command_targets ct ON ct.command_id = c.id AND ct.target_id = $1
			LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = $1
			WHERE c.type = 'ota'
			AND (
				cs.status IS NULL
				OR cs.status NOT IN ('installed', 'failed', 'completed')
				OR (cs.status = 'failed' AND cs.updated_at > NOW() - INTERVAL '1 hour')
			)
		)
	`, deviceID).Scan(&exists)
	return exists, err
}

	// ClearPendingOTACommands marks in-progress OTA commands for a device as
	// failed so HasPendingOTACommand returns false, allowing immediate retry.
	func (d *DB) ClearPendingOTACommands(ctx context.Context, deviceID uuid.UUID) error {
		_, err := d.pool.Exec(ctx, `
			INSERT INTO command_status (command_id, device_id, status, updated_at)
			SELECT c.id, ct.target_id, 'failed', NOW() - INTERVAL '2 hours'
			FROM commands c
			JOIN command_targets ct ON ct.command_id = c.id AND ct.target_id = $1
			LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = $1
			WHERE c.type = 'ota'
			AND (
				cs.status IS NULL
				OR cs.status NOT IN ('installed', 'failed', 'completed')
				OR (cs.status = 'failed' AND cs.updated_at > NOW() - INTERVAL '2 hours')
			)
			ON CONFLICT (command_id, device_id) DO UPDATE
				SET status = EXCLUDED.status, updated_at = EXCLUDED.updated_at
		`, deviceID)
		return err
	}
