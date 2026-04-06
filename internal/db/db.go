package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
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
	ID            int       `json:"id"`
	Type          string    `json:"type"`            // "full" or "incremental"
	TargetBuildID string    `json:"target_build_id"` // build ID after update
	SourceBuildID string    `json:"source_build_id"` // build ID before update (incremental only)
	ReleaseDate   time.Time `json:"release_date"`
	UpdateURL     string    `json:"update_url"`
	CreatedAt     time.Time `json:"created_at"`
}

type Update struct {
	ID             int         `json:"id"`
	OtaPackageID   int         `json:"ota_package_id"`
	RebootBehavior string      `json:"reboot_behavior"`  // "immediate", "scheduled", "manual"
	ScheduledTime  *time.Time  `json:"scheduled_time"`   // when reboot_behavior is "scheduled"
	Status         string      `json:"status"`            // "pending", "active", "complete"
	CreatedAt      time.Time   `json:"created_at"`
	OtaPackage     *OTAPackage `json:"ota_package,omitempty"` // joined
	Targets        []UpdateTarget `json:"targets,omitempty"`   // joined
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
	SerialNumber string    `json:"serial_number"`
	Status       string    `json:"status"`
	UpdatedAt    time.Time `json:"updated_at"`
	Output       string    `json:"output"`
}

// DeviceFilter holds optional filter parameters for device listing.
type DeviceFilter struct {
	Search  string    // search by serial/build (ILIKE)
	GroupID uuid.UUID // filter by group membership (uuid.Nil = no filter)
	Online  string    // "online", "offline", or "" (no filter)
	BuildID string    // exact build_id match, or "" (no filter)
	Battery string    // "low" (<20%), "mid" (20-49%), "ok" (>=50%), or "" (no filter)
	Hidden  string    // "include" (show all), "only" (hidden only), or "" (active only)
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
		INSERT INTO devices (serial_number, build_id, last_seen_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (serial_number) DO UPDATE
			SET build_id     = EXCLUDED.build_id,
			    last_seen_at = NOW(),
			    hidden       = false
		RETURNING id, poll_interval_ms
	`, serial, buildID).Scan(&deviceID, &pollIntervalMs)
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
			COUNT(DISTINCT d.build_id),
			COUNT(*) FILTER (WHERE dc.kiosk_enabled = true)
		FROM devices d
		LEFT JOIN latest l ON l.device_id = d.id
		LEFT JOIN device_config dc ON dc.device_id = d.id
		WHERE NOT d.hidden
	`).Scan(&s.Total, &s.RecentlyActive, &s.LowBattery, &s.UniqueBuilds, &s.KioskCount)
	return s, err
}

func (d *DB) ListDevices(ctx context.Context, f DeviceFilter, offset, limit int, sort string) ([]Device, error) {
	query, args := d.buildDeviceQuery(f, sort, true, limit, offset)

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
	query, args := d.buildDeviceQuery(f, "", false, 0, 0)

	var count int
	err := d.pool.QueryRow(ctx, query, args...).Scan(&count)
	return count, err
}

func (d *DB) buildDeviceQuery(f DeviceFilter, sort string, selectRows bool, limit, offset int) (string, []interface{}) {
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
		wheres = append(wheres, fmt.Sprintf("(d.serial_number ILIKE $%d OR d.build_id ILIKE $%d)", argN, argN))
		args = append(args, "%"+f.Search+"%")
		argN++
	}

	if f.GroupID != uuid.Nil {
		joins = append(joins, fmt.Sprintf("JOIN device_groups dg ON dg.device_id = d.id AND dg.group_id = $%d", argN))
		args = append(args, f.GroupID)
		argN++
	}

	if f.Online == "online" {
		wheres = append(wheres, "d.last_seen_at > NOW() - INTERVAL '3 minutes'")
	} else if f.Online == "offline" {
		wheres = append(wheres, "d.last_seen_at <= NOW() - INTERVAL '3 minutes'")
	}

	if f.BuildID != "" {
		wheres = append(wheres, fmt.Sprintf("d.build_id = $%d", argN))
		args = append(args, f.BuildID)
		argN++
	}

	if selectRows {
		base := `SELECT
			d.id, d.serial_number, d.build_id, d.last_seen_at, d.created_at,
			COALESCE(c.battery_pct, 0) AS battery_pct,
			d.poll_interval_ms,
			COALESCE(dc.kiosk_enabled, false),
			COALESCE(dc.kiosk_package, ''),
			COALESCE(c.extra, '{}'::jsonb) AS latest_extra
		FROM devices d
		LEFT JOIN LATERAL (
			SELECT battery_pct, extra FROM checkins
			WHERE device_id = d.id
			ORDER BY created_at DESC
			LIMIT 1
		) c ON true
		LEFT JOIN device_config dc ON dc.device_id = d.id`

		for _, j := range joins {
			base += "\n" + j
		}

		base += "\nWHERE " + strings.Join(wheres, " AND ")

		// battery filter needs the lateral join, so it goes in HAVING-style via a wrapping CTE
		// or we simply add it to WHERE referencing c.battery_pct
		switch f.Battery {
		case "low":
			base += " AND COALESCE(c.battery_pct, 0) < 20"
		case "mid":
			base += " AND COALESCE(c.battery_pct, 0) BETWEEN 20 AND 49"
		case "ok":
			base += " AND COALESCE(c.battery_pct, 0) >= 50"
		}

		orderClause := "d.last_seen_at DESC"
		switch sort {
		case "serial":
			orderClause = "d.serial_number ASC"
		case "battery":
			orderClause = "COALESCE(c.battery_pct, 0) ASC"
		}
		base += "\nORDER BY " + orderClause

		base += fmt.Sprintf("\nLIMIT $%d OFFSET $%d", argN, argN+1)
		args = append(args, limit, offset)

		return base, args
	}

	// COUNT query
	base := "SELECT COUNT(*) FROM devices d"
	// For battery filter we need the lateral join even in count
	needLateral := f.Battery != ""
	if needLateral {
		base += `
		LEFT JOIN LATERAL (
			SELECT battery_pct FROM checkins
			WHERE device_id = d.id
			ORDER BY created_at DESC
			LIMIT 1
		) c ON true`
	}
	for _, j := range joins {
		base += "\n" + j
	}
	base += "\nWHERE " + strings.Join(wheres, " AND ")
	switch f.Battery {
	case "low":
		base += " AND COALESCE(c.battery_pct, 0) < 20"
	case "mid":
		base += " AND COALESCE(c.battery_pct, 0) BETWEEN 20 AND 49"
	case "ok":
		base += " AND COALESCE(c.battery_pct, 0) >= 50"
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

// ExportCheckins returns checkin data for multiple devices within a time range,
// sampled at the given interval in seconds (0 = all rows).
func (d *DB) ExportCheckins(ctx context.Context, deviceIDs []uuid.UUID, start, end time.Time, intervalSec int) ([]ExportRow, error) {
	var query string
	var args []interface{}

	if intervalSec > 0 {
		// Use ROW_NUMBER to pick one row per device per interval bucket
		query = `
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
		ORDER BY serial_number, created_at`
		args = []interface{}{deviceIDs, start, end, intervalSec}
	} else {
		query = `
		SELECT d.serial_number, c.battery_pct, c.build_id, c.extra, c.created_at, d.last_seen_at
		FROM checkins c
		JOIN devices d ON d.id = c.device_id
		WHERE c.device_id = ANY($1)
		  AND c.created_at >= $2
		  AND c.created_at <= $3
		ORDER BY d.serial_number, c.created_at`
		args = []interface{}{deviceIDs, start, end}
	}

	rows, err := d.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []ExportRow
	for rows.Next() {
		var r ExportRow
		var extra []byte
		if err := rows.Scan(&r.SerialNumber, &r.BatteryPct, &r.BuildID, &extra, &r.Timestamp, &r.LastSeenAt); err != nil {
			return nil, err
		}
		if len(extra) > 0 {
			r.Extra = json.RawMessage(extra)
		} else {
			r.Extra = json.RawMessage("{}")
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) GetDevice(ctx context.Context, serial string) (*Device, error) {
	var dev Device
	err := d.pool.QueryRow(ctx, `
		SELECT
			d.id, d.serial_number, d.build_id, d.last_seen_at, d.created_at,
			COALESCE(c.battery_pct, 0) AS battery_pct,
			d.poll_interval_ms,
			COALESCE(dc.kiosk_enabled, false),
			COALESCE(dc.kiosk_package, ''),
			COALESCE(c.extra, '{}'::jsonb) AS latest_extra
		FROM devices d
		LEFT JOIN LATERAL (
			SELECT battery_pct, extra FROM checkins
			WHERE device_id = d.id
			ORDER BY created_at DESC
			LIMIT 1
		) c ON true
		LEFT JOIN device_config dc ON dc.device_id = d.id
		WHERE d.serial_number = $1
	`, serial).Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct, &dev.PollIntervalMs, &dev.KioskEnabled, &dev.KioskPackage, &dev.LatestExtra)
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
			COALESCE(c.battery_pct, 0) AS battery_pct,
			d.poll_interval_ms,
			COALESCE(dc.kiosk_enabled, false),
			COALESCE(dc.kiosk_package, '')
		FROM devices d
		JOIN device_groups dg ON dg.device_id = d.id
		LEFT JOIN LATERAL (
			SELECT battery_pct FROM checkins
			WHERE device_id = d.id
			ORDER BY created_at DESC
			LIMIT 1
		) c ON true
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
func (d *DB) GetDeviceCommands(ctx context.Context, deviceID uuid.UUID) ([]DeviceCommand, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT c.id, c.type, c.apk_url, c.payload, c.target_type, c.created_at,
		       COALESCE(cs.status, 'pending') AS status,
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
	`, deviceID)
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
func (d *DB) GetCommandDeliveries(ctx context.Context, commandID uuid.UUID) ([]CommandDelivery, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.serial_number, cs.status, cs.updated_at, COALESCE(cr.output, '') AS output
		FROM command_status cs
		JOIN devices d ON d.id = cs.device_id
		LEFT JOIN command_results cr ON cr.command_id = cs.command_id AND cr.device_id = cs.device_id
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
		if err := rows.Scan(&cd.SerialNumber, &cd.Status, &cd.UpdatedAt, &cd.Output); err != nil {
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
`

// ── OTA Packages ──────────────────────────────────────────────────────────────

func (d *DB) CreateOTAPackage(ctx context.Context, typ, targetBuildID, sourceBuildID, updateURL string, releaseDate time.Time) (*OTAPackage, error) {
	var p OTAPackage
	err := d.pool.QueryRow(ctx, `
		INSERT INTO ota_packages (type, target_build_id, source_build_id, build_id, update_url, release_date)
		VALUES ($1, $2, $3, $2, $4, $5)
		RETURNING id, type, target_build_id, source_build_id, release_date, update_url, created_at
	`, typ, targetBuildID, sourceBuildID, updateURL, releaseDate).
		Scan(&p.ID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.CreatedAt)
	return &p, err
}

func (d *DB) ListOTAPackages(ctx context.Context) ([]OTAPackage, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, type, target_build_id, source_build_id, release_date, update_url, created_at
		FROM ota_packages
		ORDER BY release_date DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []OTAPackage
	for rows.Next() {
		var p OTAPackage
		if err := rows.Scan(&p.ID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *DB) DeleteOTAPackage(ctx context.Context, id int) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM ota_packages WHERE id = $1`, id)
	return err
}

// ── Updates ──────────────────────────────────────────────────────────────────

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

func (d *DB) ListUpdates(ctx context.Context) ([]Update, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT u.id, u.ota_package_id, u.reboot_behavior, u.scheduled_time, u.status, u.created_at,
		       p.id, p.type, p.target_build_id, p.source_build_id, p.release_date, p.update_url, p.created_at
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
			&p.ID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.CreatedAt); err != nil {
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
		       p.id, p.type, p.target_build_id, p.source_build_id, p.release_date, p.update_url, p.created_at
		FROM updates u
		JOIN ota_packages p ON p.id = u.ota_package_id
		WHERE u.id = $1
	`, id).Scan(&u.ID, &u.OtaPackageID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt,
		&p.ID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.CreatedAt)
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
		       p.id, p.type, p.target_build_id, p.source_build_id, p.release_date, p.update_url, p.created_at
		FROM update_devices ud
		JOIN updates u ON u.id = ud.update_id
		JOIN ota_packages p ON p.id = u.ota_package_id
		WHERE ud.device_id = $1 AND u.status = 'active' AND ud.status != 'installed'
		ORDER BY u.created_at DESC
		LIMIT 1
	`, deviceID).Scan(&u.ID, &u.OtaPackageID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt,
		&p.ID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.CreatedAt)
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
