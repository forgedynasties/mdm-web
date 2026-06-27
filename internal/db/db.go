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
	// RestaurantID is the venue the device physically lives in (nil = lab/bench unit).
	// RestaurantName is joined for display. A device is "deployed" iff it has a restaurant.
	RestaurantID   *uuid.UUID `json:"restaurant_id,omitempty"`
	RestaurantName string     `json:"restaurant_name,omitempty"`
	// DeployedEffective is true when the device is live in a restaurant (RestaurantID set);
	// false = lab/bench unit. There is no separate deployed flag — assignment is the signal.
	DeployedEffective bool `json:"deployed_effective"`
}

// Restaurant is a real venue a device physically lives in. It owns the venue semantics
// (service window, health, AI bucketing). A device assigned to a restaurant is "deployed";
// unassigned = lab/bench. See docs/release-versioning-and-restaurants.md.
type Restaurant struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	Address     string    `json:"address"`
	Latitude    *float64  `json:"latitude"`
	Longitude   *float64  `json:"longitude"`
	Timezone    string    `json:"timezone"`
	Notes       string    `json:"notes"`
	CreatedAt   time.Time `json:"created_at"`
	DeviceCount int       `json:"device_count"` // populated by List/Get
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
	ReleaseID       int       `json:"release_id"`
	Type            string    `json:"type"` // "full" or "incremental"
	TargetBuildID   string    `json:"target_build_id"`
	SourceBuildID   string    `json:"source_build_id"` // incremental only
	ReleaseDate     time.Time `json:"release_date"`
	UpdateURL       string    `json:"update_url"`
	Changelog       string    `json:"changelog"`
	Status          string    `json:"status"` // "active" or "yanked"
	CreatedAt       time.Time `json:"created_at"`
	DeploymentCount int       `json:"deployment_count,omitempty"` // populated by ListOTAPackages
}

// Release groups the packages for one target build version under a single
// lifecycle. Version equals the target build id.
type Release struct {
	ID            int        `json:"id"`
	Version       string     `json:"version"`
	Name          string     `json:"name"`
	Changelog     string     `json:"changelog"`
	Status        string     `json:"status"`          // "draft" | "published" | "yanked"
	Hidden        bool       `json:"hidden"`          // hidden from the main releases list (irrelevant)
	SkipBaseTests bool       `json:"skip_base_tests"` // QA tests only release-specific cases, not base
	CreatedAt     time.Time  `json:"created_at"`
	PublishedAt   *time.Time `json:"published_at"`
	SignedOffBy   string     `json:"signed_off_by"` // dev who smoke-tested; "" when not signed off
	SignedOffAt   *time.Time `json:"signed_off_at"`
	PackageCount  int        `json:"package_count,omitempty"` // populated by ListReleases
	DeployCount   int        `json:"deploy_count,omitempty"`  // populated by ListReleases
}

// FleetVersion is one release version actually reported by devices in the field, with the
// devices on it and a link to the managed release (if any). Powers release tracking.
type FleetVersion struct {
	Version       string   `json:"version"`
	DeviceCount   int      `json:"device_count"`
	Serials       []string `json:"serials"`
	ReleaseID     *int     `json:"release_id"`     // non-nil when this version is a managed release
	ReleaseStatus string   `json:"release_status"` // "" when unmanaged
	ReleaseHidden bool     `json:"release_hidden"`
}

type Update struct {
	ID              int            `json:"id"`
	OtaPackageID    int            `json:"ota_package_id"`
	ReleaseID       int            `json:"release_id"`
	RebootBehavior  string         `json:"reboot_behavior"` // "immediate", "scheduled", "manual"
	ScheduledTime   *time.Time     `json:"scheduled_time"`
	Status          string         `json:"status"` // "pending", "active", "complete"
	CreatedAt       time.Time      `json:"created_at"`
	OtaPackage      *OTAPackage    `json:"ota_package,omitempty"`
	Release         *Release       `json:"release,omitempty"`
	Targets         []UpdateTarget `json:"targets,omitempty"`
	DeviceTotal     int            `json:"device_total,omitempty"`     // populated by ListDeploymentsByPackage
	DeviceInstalled int            `json:"device_installed,omitempty"` // populated by ListDeploymentsByPackage
	DeviceStatus    string         `json:"device_status,omitempty"`    // populated by ResolveUpdateForDevice
}

type UpdateTarget struct {
	UpdateID     int       `json:"update_id"`
	DeviceID     uuid.UUID `json:"device_id"`
	SerialNumber string    `json:"serial_number"` // joined from devices
	BuildID      string    `json:"build_id"`      // current device build
	Status       string    `json:"status"`        // "pending", "downloading", "installing", "installed"
	ErrorCode    string    `json:"error_code"`    // device-reported code when status == "failed"
	UpdatedAt    time.Time `json:"updated_at"`    // when this row last changed state

	StartedAt   *time.Time `json:"started_at"`   // when the device began downloading (nil if not yet)
	CompletedAt *time.Time `json:"completed_at"` // when the device reported installed (nil if not yet)
}

// DurationSeconds returns how long this device took to update (download → installed),
// or -1 if it hasn't both started and completed.
func (t UpdateTarget) DurationSeconds() int {
	if t.StartedAt == nil || t.CompletedAt == nil {
		return -1
	}
	return int(t.CompletedAt.Sub(*t.StartedAt).Seconds())
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
	// Empty marks a cycles-mode grid row with no check-in within one interval
	// of the mark — data fields are zero values and should export as blanks.
	Empty bool `json:"empty,omitempty"`
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
	Search              string    // search by serial substring
	GroupID             uuid.UUID // filter by group membership (uuid.Nil = no filter)
	ExcludeGroupID      uuid.UUID // exclude devices already in this group (uuid.Nil = no filter)
	RestaurantID        uuid.UUID // filter by restaurant/venue (uuid.Nil = no filter)
	ProductionID        uuid.UUID // filter by production (uuid.Nil = no filter)
	Online              string    // "online", "offline", or "" (no filter)
	BuildID             string    // exact build_id match, or "" (no filter)
	Battery             string    // "low" (<20%), "mid" (20-49%), "ok" (>=50%), or "" (no filter)
	Kiosk               string    // "enabled" (kiosk on), "disabled" (kiosk off), or "" (no filter)
	Charging            string    // "yes" (charging), "no" (not charging), or "" (no filter)
	Timezone            string    // exact timezone match (latest_extra->>'timezone'), or "" (no filter)
	Hidden              string    // "include" (show all), "only" (hidden only), or "" (active only)
	ActiveThresholdSecs int       // seconds before a device is considered offline (0 = default 180)
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

// UpdateUserRole changes a user's role. The caller validates the role value.
func (d *DB) UpdateUserRole(ctx context.Context, id uuid.UUID, role string) error {
	_, err := d.pool.Exec(ctx, `UPDATE users SET role = $2 WHERE id = $1`, id, role)
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
	// Run on a single connection inside one transaction so SET LOCAL lock_timeout
	// scopes to the migration only. The lock_timeout makes a schema statement
	// fail fast rather than block indefinitely when another session holds a
	// conflicting lock — typically the previous container still draining during a
	// deploy. A fast failure exits the process, Docker (restart: unless-stopped)
	// retries, and by then the lock is gone; without it the migration hangs before
	// the server ever listens, which the proxy surfaces as a 502.
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()

	tx, err := conn.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if _, err := tx.Exec(ctx, "SET LOCAL lock_timeout = '15s'"); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, migrationSQL); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// UpsertCheckin records a check-in, creating the device row on first contact. The returned
// isNew is true only when the device row was inserted by this call (xmax = 0 on a freshly
// inserted tuple), so callers can fire onboarding side-effects exactly once.
// mergeExtra controls how the incoming extra combines
// with the stored snapshot: false (HTTP keyframe / full snapshot) REPLACES latest_extra,
// clearing any stale keys; true (WS delta frame) shallow-merges (JSONB ||) so only the
// changed fields are overwritten and unchanged ones are preserved. battery_pct is a pointer:
// when nil (omitted from a delta) the prior value is carried forward. RETURNING the resolved
// extra + battery makes the checkins history row a full snapshot regardless of frame type
// (windowed alerts + daily rollups read checkins.extra).
func (d *DB) UpsertCheckin(ctx context.Context, serial, buildID string, batteryPct *int, extra json.RawMessage, mergeExtra bool) (deviceID uuid.UUID, pollIntervalMs int, isNew bool, err error) {
	if len(extra) == 0 {
		extra = json.RawMessage("{}")
	}

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, 0, false, err
	}
	defer tx.Rollback(ctx)

	extraExpr := "EXCLUDED.latest_extra"
	if mergeExtra {
		extraExpr = "COALESCE(devices.latest_extra, '{}'::jsonb) || EXCLUDED.latest_extra"
	}
	var merged json.RawMessage
	var battery int
	err = tx.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO devices (serial_number, build_id, last_seen_at, latest_battery_pct, latest_extra)
		VALUES ($1, $2, NOW(), COALESCE($3, 0), $4)
		ON CONFLICT (serial_number) DO UPDATE
			SET build_id           = EXCLUDED.build_id,
			    last_seen_at       = NOW(),
			    latest_battery_pct = COALESCE($3, devices.latest_battery_pct),
			    latest_extra       = %s,
			    hidden             = false
		RETURNING id, poll_interval_ms, (xmax = 0) AS is_new, latest_battery_pct, latest_extra
	`, extraExpr), serial, buildID, batteryPct, extra).Scan(&deviceID, &pollIntervalMs, &isNew, &battery, &merged)
	if err != nil {
		return uuid.Nil, 0, false, err
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO checkins (device_id, battery_pct, build_id, extra)
		VALUES ($1, $2, $3, $4)
	`, deviceID, battery, buildID, merged)
	if err != nil {
		return uuid.Nil, 0, false, err
	}

	return deviceID, pollIntervalMs, isNew, tx.Commit(ctx)
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
		if err := rows.Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct, &dev.PollIntervalMs, &dev.KioskEnabled, &dev.KioskPackage, &dev.LatestExtra, &dev.Hidden, &dev.RestaurantName); err != nil {
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

	if f.ExcludeGroupID != uuid.Nil {
		wheres = append(wheres, fmt.Sprintf("NOT EXISTS (SELECT 1 FROM device_groups dgx WHERE dgx.device_id = d.id AND dgx.group_id = $%d)", argN))
		args = append(args, f.ExcludeGroupID)
		argN++
	}

	if f.RestaurantID != uuid.Nil {
		wheres = append(wheres, fmt.Sprintf("d.restaurant_id = $%d", argN))
		args = append(args, f.RestaurantID)
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

	if f.Timezone != "" {
		wheres = append(wheres, fmt.Sprintf("d.latest_extra->>'timezone' = $%d", argN))
		args = append(args, f.Timezone)
		argN++
	}

	if f.Charging == "yes" {
		wheres = append(wheres, "COALESCE((d.latest_extra->>'charging')::boolean, false) = true")
	} else if f.Charging == "no" {
		wheres = append(wheres, "COALESCE((d.latest_extra->>'charging')::boolean, false) = false")
	}

	if selectRows {
		base := `SELECT
			d.id, d.serial_number, d.build_id, d.last_seen_at, d.created_at,
			d.latest_battery_pct AS battery_pct,
			d.poll_interval_ms,
			COALESCE(dc.kiosk_enabled, false),
			COALESCE(dc.kiosk_package, ''),
			d.latest_extra AS latest_extra,
			d.hidden,
			COALESCE(r.name, '')
		FROM devices d
		LEFT JOIN device_config dc ON dc.device_id = d.id
		LEFT JOIN restaurants r ON r.id = d.restaurant_id`

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

// GetDistinctTimezones returns all distinct non-empty timezone values
// (from latest_extra->>'timezone') for non-hidden devices.
func (d *DB) GetDistinctTimezones(ctx context.Context) ([]string, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT latest_extra->>'timezone' AS tz FROM devices
		WHERE NOT hidden AND COALESCE(latest_extra->>'timezone', '') != ''
		ORDER BY tz
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var tzs []string
	for rows.Next() {
		var tz string
		if err := rows.Scan(&tz); err != nil {
			return nil, err
		}
		tzs = append(tzs, tz)
	}
	return tzs, rows.Err()
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

// StreamExportCycles streams one row per device per grid mark: start, start+interval,
// …, end (inclusive). Each mark carries the latest check-in at or before it, but only
// if that check-in is within one interval of the mark — otherwise the row comes back
// with Empty=true so the CSV shows a visible gap instead of stale carried-forward data.
func (d *DB) StreamExportCycles(ctx context.Context, deviceIDs []uuid.UUID, start, end time.Time, intervalSec int, fn func(ExportRow) error) error {
	rows, err := d.pool.Query(ctx, `
		SELECT d.serial_number, c.battery_pct, c.build_id, c.extra, g.ts, d.last_seen_at
		FROM devices d
		CROSS JOIN generate_series($2::timestamptz, $3::timestamptz, make_interval(secs => $4)) AS g(ts)
		LEFT JOIN LATERAL (
			SELECT battery_pct, build_id, extra
			FROM checkins c
			WHERE c.device_id = d.id
			  AND c.created_at <= g.ts
			  AND c.created_at > g.ts - make_interval(secs => $4)
			ORDER BY c.created_at DESC
			LIMIT 1
		) c ON true
		WHERE d.id = ANY($1)
		ORDER BY d.serial_number, g.ts`,
		deviceIDs, start, end, intervalSec)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var r ExportRow
		var batteryPct *int
		var buildID *string
		var extra []byte
		if err := rows.Scan(&r.SerialNumber, &batteryPct, &buildID, &extra, &r.Timestamp, &r.LastSeenAt); err != nil {
			return err
		}
		if batteryPct == nil {
			r.Empty = true
			r.Extra = json.RawMessage("{}")
		} else {
			r.BatteryPct = *batteryPct
			if buildID != nil {
				r.BuildID = *buildID
			}
			if len(extra) > 0 {
				r.Extra = json.RawMessage(extra)
			} else {
				r.Extra = json.RawMessage("{}")
			}
		}
		if err := fn(r); err != nil {
			return err
		}
	}
	return rows.Err()
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

// ListAllSerials returns every device serial (visible and hidden), so callers can
// detect and linkify serials named in free text (e.g. the AI report prose).
func (d *DB) ListAllSerials(ctx context.Context) ([]string, error) {
	rows, err := d.pool.Query(ctx, `SELECT serial_number FROM devices ORDER BY serial_number`)
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

func (d *DB) GetDevice(ctx context.Context, serial string) (*Device, error) {
	var dev Device
	err := d.pool.QueryRow(ctx, `
		SELECT
			d.id, d.serial_number, d.build_id, d.last_seen_at, d.created_at,
			d.latest_battery_pct AS battery_pct,
			d.poll_interval_ms,
			COALESCE(dc.kiosk_enabled, false),
			COALESCE(dc.kiosk_package, ''),
			d.latest_extra AS latest_extra,
			d.restaurant_id, COALESCE(r.name, ''),
			(d.restaurant_id IS NOT NULL) AS deployed_effective
		FROM devices d
		LEFT JOIN device_config dc ON dc.device_id = d.id
		LEFT JOIN restaurants r ON r.id = d.restaurant_id
		WHERE d.serial_number = $1
	`, serial).Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct, &dev.PollIntervalMs, &dev.KioskEnabled, &dev.KioskPackage, &dev.LatestExtra, &dev.RestaurantID, &dev.RestaurantName, &dev.DeployedEffective)
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
			d.latest_extra AS latest_extra,
			d.hidden
		FROM devices d
		LEFT JOIN device_config dc ON dc.device_id = d.id
		WHERE d.id = $1
	`, id).Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct, &dev.PollIntervalMs, &dev.KioskEnabled, &dev.KioskPackage, &dev.LatestExtra, &dev.Hidden)
	if err != nil {
		return nil, fmt.Errorf("device not found: %w", err)
	}
	return &dev, nil
}

// DeploymentCounts returns how many non-hidden devices are deployed (assigned to a
// restaurant) vs. in the lab (unassigned).
func (d *DB) DeploymentCounts(ctx context.Context) (deployed, lab int, err error) {
	err = d.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE d.restaurant_id IS NOT NULL),
			COUNT(*) FILTER (WHERE d.restaurant_id IS NULL)
		FROM devices d
		WHERE NOT d.hidden
	`).Scan(&deployed, &lab)
	return deployed, lab, err
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

func (d *DB) UnhideDevice(ctx context.Context, serial string) error {
	_, err := d.pool.Exec(ctx, `UPDATE devices SET hidden = false WHERE serial_number = $1`, serial)
	return err
}

func (d *DB) BulkUnhideDevices(ctx context.Context, serials []string) error {
	_, err := d.pool.Exec(ctx, `UPDATE devices SET hidden = false WHERE serial_number = ANY($1)`, serials)
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

	return scanCheckins(rows)
}

// GetCheckinsBetween returns a device's check-ins within [from, until]. Used to load a
// bounded window around a past incident (e.g. a heat spike days ago) without pulling
// every check-in since then — these devices can check in every few seconds.
func (d *DB) GetCheckinsBetween(ctx context.Context, deviceID uuid.UUID, from, until time.Time) ([]Checkin, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, device_id, battery_pct, build_id, extra, created_at
		FROM checkins
		WHERE device_id = $1 AND created_at >= $2 AND created_at <= $3
		ORDER BY created_at DESC
	`, deviceID, from, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	return scanCheckins(rows)
}

// scanCheckins materializes check-in rows, defaulting empty extra to "{}".
func scanCheckins(rows pgx.Rows) ([]Checkin, error) {
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

// FleetCounts holds the headline totals shown in the unified Fleet tab strip
// (Devices / Restaurants / Groups), fetched in one round-trip.
type FleetCounts struct {
	Devices     int `json:"devices"`
	Restaurants int `json:"restaurants"`
	Groups      int `json:"groups"`
}

// FleetCounts returns visible-device, restaurant, and group totals in one query.
func (d *DB) FleetCounts(ctx context.Context) (FleetCounts, error) {
	var c FleetCounts
	err := d.pool.QueryRow(ctx, `
		SELECT (SELECT COUNT(*) FROM devices WHERE NOT hidden),
		       (SELECT COUNT(*) FROM restaurants),
		       (SELECT COUNT(*) FROM groups)
	`).Scan(&c.Devices, &c.Restaurants, &c.Groups)
	return c, err
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

// ListDeviceGroups returns the groups a single device belongs to (for the device
// detail page's placement block).
func (d *DB) ListDeviceGroups(ctx context.Context, deviceID uuid.UUID) ([]Group, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT g.id, g.name, g.created_at
		FROM groups g JOIN device_groups dg ON dg.group_id = g.id
		WHERE dg.device_id = $1
		ORDER BY g.name`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Group
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
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

// ── Restaurants ─────────────────────────────────────────────────────────────────
//
// A restaurant is the venue a device physically lives in (1 device → at most 1
// restaurant). Restaurants own the venue semantics (deployed flag, service window,
// health, AI bucketing); groups remain free-form tags. See
// docs/release-versioning-and-restaurants.md.

func (d *DB) CreateRestaurant(ctx context.Context, r Restaurant) (*Restaurant, error) {
	var out Restaurant
	err := d.pool.QueryRow(ctx, `
		INSERT INTO restaurants (name, address, latitude, longitude, timezone, notes)
		VALUES ($1, $2, $3, $4, $5, $6)
		RETURNING id, name, address, latitude, longitude, timezone, notes, created_at
	`, r.Name, r.Address, r.Latitude, r.Longitude, r.Timezone, r.Notes).
		Scan(&out.ID, &out.Name, &out.Address, &out.Latitude, &out.Longitude, &out.Timezone, &out.Notes, &out.CreatedAt)
	return &out, err
}

func (d *DB) UpdateRestaurant(ctx context.Context, r Restaurant) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE restaurants SET name = $2, address = $3, latitude = $4, longitude = $5,
			timezone = $6, notes = $7
		WHERE id = $1
	`, r.ID, r.Name, r.Address, r.Latitude, r.Longitude, r.Timezone, r.Notes)
	return err
}

func (d *DB) DeleteRestaurant(ctx context.Context, id uuid.UUID) error {
	// devices.restaurant_id ON DELETE SET NULL unassigns members automatically.
	_, err := d.pool.Exec(ctx, `DELETE FROM restaurants WHERE id = $1`, id)
	return err
}

func (d *DB) ListRestaurants(ctx context.Context) ([]Restaurant, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT r.id, r.name, r.address, r.latitude, r.longitude, r.timezone,
		       r.notes, r.created_at, COUNT(d.id) AS device_count
		FROM restaurants r
		LEFT JOIN devices d ON d.restaurant_id = r.id AND NOT d.hidden
		GROUP BY r.id
		ORDER BY r.name
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Restaurant
	for rows.Next() {
		var r Restaurant
		if err := rows.Scan(&r.ID, &r.Name, &r.Address, &r.Latitude, &r.Longitude, &r.Timezone,
			&r.Notes, &r.CreatedAt, &r.DeviceCount); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (d *DB) GetRestaurant(ctx context.Context, id uuid.UUID) (*Restaurant, error) {
	var r Restaurant
	err := d.pool.QueryRow(ctx, `
		SELECT r.id, r.name, r.address, r.latitude, r.longitude, r.timezone,
		       r.notes, r.created_at, COUNT(d.id) AS device_count
		FROM restaurants r
		LEFT JOIN devices d ON d.restaurant_id = r.id AND NOT d.hidden
		WHERE r.id = $1
		GROUP BY r.id
	`, id).Scan(&r.ID, &r.Name, &r.Address, &r.Latitude, &r.Longitude, &r.Timezone,
		&r.Notes, &r.CreatedAt, &r.DeviceCount)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// AssignDeviceToRestaurant sets (or with restaurantID=nil clears) a device's venue.
func (d *DB) AssignDeviceToRestaurant(ctx context.Context, serial string, restaurantID *uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `UPDATE devices SET restaurant_id = $2 WHERE serial_number = $1`, serial, restaurantID)
	return err
}

// AssignDevicesToRestaurant assigns many devices (by serial) to a restaurant at once.
func (d *DB) AssignDevicesToRestaurant(ctx context.Context, serials []string, restaurantID uuid.UUID) error {
	if len(serials) == 0 {
		return nil
	}
	_, err := d.pool.Exec(ctx, `UPDATE devices SET restaurant_id = $2 WHERE serial_number = ANY($1)`, serials, restaurantID)
	return err
}

func (d *DB) ListRestaurantDevices(ctx context.Context, restaurantID uuid.UUID) ([]Device, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT
			d.id, d.serial_number, d.build_id, d.last_seen_at, d.created_at,
			d.latest_battery_pct AS battery_pct,
			d.poll_interval_ms,
			COALESCE(dc.kiosk_enabled, false),
			COALESCE(dc.kiosk_package, '')
		FROM devices d
		LEFT JOIN device_config dc ON dc.device_id = d.id
		WHERE d.restaurant_id = $1 AND NOT d.hidden
		ORDER BY d.serial_number
	`, restaurantID)
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

// ListAssignableDevices returns candidate devices to assign to a restaurant: every
// non-hidden device NOT already in this restaurant, optionally filtered by serial, with
// its current restaurant name. Unassigned (lab) devices sort first, then most-recently
// seen. Powers the restaurant device picker.
func (d *DB) ListAssignableDevices(ctx context.Context, restaurantID uuid.UUID, query, status, battery string, limit, activeThresholdSecs int) ([]Device, error) {
	if limit <= 0 {
		limit = 20
	}
	if activeThresholdSecs <= 0 {
		activeThresholdSecs = 180
	}
	args := []any{restaurantID, query}
	q := `
		SELECT d.serial_number, d.build_id, d.latest_battery_pct, d.last_seen_at, COALESCE(r.name, '')
		FROM devices d
		LEFT JOIN restaurants r ON r.id = d.restaurant_id
		WHERE NOT d.hidden
		  AND d.restaurant_id IS DISTINCT FROM $1
		  AND ($2 = '' OR d.serial_number ILIKE '%' || $2 || '%')`
	if status == "online" || status == "offline" {
		args = append(args, activeThresholdSecs)
		op := ">"
		if status == "offline" {
			op = "<="
		}
		q += fmt.Sprintf(" AND d.last_seen_at %s NOW() - ($%d * INTERVAL '1 second')", op, len(args))
	}
	switch battery {
	case "low":
		q += " AND d.latest_battery_pct < 20"
	case "mid":
		q += " AND d.latest_battery_pct BETWEEN 20 AND 49"
	case "ok":
		q += " AND d.latest_battery_pct >= 50"
	}
	args = append(args, limit)
	q += fmt.Sprintf(" ORDER BY (d.restaurant_id IS NULL) DESC, d.last_seen_at DESC LIMIT $%d", len(args))
	rows, err := d.pool.Query(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var dev Device
		if err := rows.Scan(&dev.SerialNumber, &dev.BuildID, &dev.BatteryPct, &dev.LastSeenAt, &dev.RestaurantName); err != nil {
			return nil, err
		}
		out = append(out, dev)
	}
	return out, rows.Err()
}

// GetRestaurantDailyStats returns the last `days` days of stats rolled up across every
// device in the restaurant, oldest first. Mirrors GetGroupDailyStats but keyed by venue.
func (d *DB) GetRestaurantDailyStats(ctx context.Context, restaurantID uuid.UUID, days int) ([]GroupDailyStat, error) {
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
		JOIN devices d ON d.id = s.device_id
		WHERE d.restaurant_id = $1 AND s.day >= CURRENT_DATE - ($2::int - 1)
		GROUP BY s.day
		ORDER BY s.day`, restaurantID, days)
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

// GetRestaurantHealth returns a health scorecard per restaurant, worst score first.
// Reuses the GroupHealth struct (GroupID carries the restaurant id, Name the restaurant
// name). activeSecs is the offline threshold. windowDays sizes the recent aggregation
// window and the equal-length prior window used for the delta: pass 7 for the dashboard's
// week-over-week view, or 1 for the Daily Report (today vs yesterday).
func (d *DB) GetRestaurantHealth(ctx context.Context, activeSecs, windowDays int) ([]GroupHealth, error) {
	if activeSecs <= 0 {
		activeSecs = 180
	}
	if windowDays <= 0 {
		windowDays = 7
	}
	rows, err := d.pool.Query(ctx, `
		WITH recent AS (
			SELECT d.restaurant_id,
				AVG(s.battery_max)   AS battery_avg,
				AVG(s.charging_frac) AS charging_avg,
				MAX(s.temp_max)      AS temp_max,
				COUNT(DISTINCT NULLIF(s.build_id, '')) AS builds
			FROM device_daily_stats s
			JOIN devices d ON d.id = s.device_id
			WHERE s.day > CURRENT_DATE - $2::int AND d.restaurant_id IS NOT NULL
			GROUP BY d.restaurant_id
		),
		prior AS (
			SELECT d.restaurant_id, AVG(s.battery_max) AS battery_avg
			FROM device_daily_stats s
			JOIN devices d ON d.id = s.device_id
			WHERE s.day <= CURRENT_DATE - $2::int AND s.day > CURRENT_DATE - ($2::int * 2) AND d.restaurant_id IS NOT NULL
			GROUP BY d.restaurant_id
		),
		hottest AS (
			-- the single device that drove each restaurant's MAX(temp_max) in the window,
			-- so the report can name the unit instead of just citing the peak number.
			SELECT DISTINCT ON (d.restaurant_id) d.restaurant_id, d.serial_number AS hot_serial
			FROM device_daily_stats s
			JOIN devices d ON d.id = s.device_id
			WHERE s.day > CURRENT_DATE - $2::int AND d.restaurant_id IS NOT NULL AND s.temp_max IS NOT NULL
			ORDER BY d.restaurant_id, s.temp_max DESC
		),
		devs AS (
			SELECT d.restaurant_id,
				COUNT(*) AS device_count,
				COUNT(*) FILTER (WHERE d.last_seen_at < NOW() - ($1 * INTERVAL '1 second')) AS offline_count,
				COUNT(*) AS deployed_count  -- every device in a restaurant is deployed
			FROM devices d
			WHERE NOT d.hidden AND d.restaurant_id IS NOT NULL
			GROUP BY d.restaurant_id
		),
		al AS (
			SELECT d.restaurant_id,
				COUNT(*) FILTER (WHERE a.severity = 'critical')  AS crit,
				COUNT(*) FILTER (WHERE a.severity <> 'critical') AS warn
			FROM alerts a
			JOIN devices d ON d.id = a.device_id
			WHERE a.status <> 'resolved' AND d.restaurant_id IS NOT NULL
			GROUP BY d.restaurant_id
		)
		SELECT r.id, r.name,
			COALESCE(devs.device_count, 0), COALESCE(devs.offline_count, 0),
			COALESCE(al.crit, 0), COALESCE(al.warn, 0),
			recent.battery_avg, (recent.battery_avg - prior.battery_avg),
			recent.charging_avg, recent.temp_max, hottest.hot_serial, COALESCE(recent.builds, 0),
			true, COALESCE(devs.deployed_count, 0)  -- a restaurant is a live venue by definition
		FROM restaurants r
		LEFT JOIN devs    ON devs.restaurant_id    = r.id
		LEFT JOIN recent  ON recent.restaurant_id  = r.id
		LEFT JOIN prior   ON prior.restaurant_id   = r.id
		LEFT JOIN hottest ON hottest.restaurant_id = r.id
		LEFT JOIN al      ON al.restaurant_id      = r.id
		ORDER BY r.name`, activeSecs, windowDays)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GroupHealth
	for rows.Next() {
		var g GroupHealth
		if err := rows.Scan(&g.GroupID, &g.Name, &g.DeviceCount, &g.OfflineCount,
			&g.OpenCritical, &g.OpenWarning, &g.BatteryAvg, &g.BatteryDelta,
			&g.ChargingAvg, &g.TempMax, &g.TempMaxSerial, &g.DistinctBuilds, &g.Deployed, &g.DeployedCount); err != nil {
			return nil, err
		}
		g.computeScore()
		out = append(out, g)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Score < out[j].Score })
	return out, nil
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

// GetDeviceIDsInGroups expands a set of group IDs into the distinct device IDs that
// belong to any of them. Used to target ad-hoc actions (e.g. a fleet log capture)
// at whole groups while operating on concrete devices.
func (d *DB) GetDeviceIDsInGroups(ctx context.Context, groupIDs []uuid.UUID) ([]uuid.UUID, error) {
	if len(groupIDs) == 0 {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, `SELECT DISTINCT device_id FROM device_groups WHERE group_id = ANY($1)`, groupIDs)
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

// ShellCommandSuggestions returns previously-sent shell commands for quick re-use in the
// builder: the most recently used (distinct, newest first) and the most frequently used.
func (d *DB) ShellCommandSuggestions(ctx context.Context, limit int) (recent, popular []string, err error) {
	if limit <= 0 {
		limit = 6
	}
	collect := func(q string) ([]string, error) {
		rows, err := d.pool.Query(ctx, q, limit)
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
	recent, err = collect(`
		SELECT cmd FROM (
			SELECT payload->>'cmd' AS cmd, MAX(created_at) AS last
			FROM commands
			WHERE type = 'shell' AND COALESCE(payload->>'cmd','') <> ''
			GROUP BY payload->>'cmd'
			ORDER BY last DESC
			LIMIT $1
		) t`)
	if err != nil {
		return nil, nil, err
	}
	popular, err = collect(`
		SELECT payload->>'cmd' AS cmd
		FROM commands
		WHERE type = 'shell' AND COALESCE(payload->>'cmd','') <> ''
		GROUP BY payload->>'cmd'
		ORDER BY COUNT(*) DESC, MAX(created_at) DESC
		LIMIT $1`)
	if err != nil {
		return nil, nil, err
	}
	return recent, popular, nil
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
	// Build the full set of targeted devices — explicit device targets and the members
	// of any targeted groups — then LEFT JOIN command_status so devices that haven't
	// received the command yet (offline / not checked in) still show as 'pending'.
	// "all" broadcasts aren't enumerable, so for those we fall back to status rows.
	rows, err := d.pool.Query(ctx, fmt.Sprintf(`
		WITH target_devices AS (
			SELECT ct.target_id AS device_id
			FROM command_targets ct
			JOIN commands c ON c.id = ct.command_id
			WHERE ct.command_id = $1 AND c.target_type = 'devices'
			UNION
			SELECT dg.device_id
			FROM command_targets ct
			JOIN commands c ON c.id = ct.command_id
			JOIN device_groups dg ON dg.group_id = ct.target_id
			WHERE ct.command_id = $1 AND c.target_type = 'groups'
			UNION
			SELECT cs.device_id FROM command_status cs WHERE cs.command_id = $1
		)
		SELECT td.device_id,
		       d.serial_number,
		       CASE
		         WHEN c.type IN ('shell', 'screenshot', 'reboot')
		              AND COALESCE(cs.status, 'pending') IN ('pending', 'delivered')
		              AND c.created_at <= NOW() - INTERVAL '%d seconds'
		           THEN 'expired'
		         ELSE COALESCE(cs.status, 'pending')
		       END AS status,
		       COALESCE(cs.updated_at, c.created_at) AS updated_at,
		       COALESCE(cr.output, '') AS output,
		       d.last_seen_at
		FROM target_devices td
		JOIN devices d ON d.id = td.device_id
		JOIN commands c ON c.id = $1
		LEFT JOIN command_status cs ON cs.command_id = $1 AND cs.device_id = td.device_id
		LEFT JOIN command_results cr ON cr.command_id = $1 AND cr.device_id = td.device_id
		ORDER BY COALESCE(cs.updated_at, c.created_at) DESC, d.serial_number
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

// ── Device Packages ───────────────────────────────────────────────────────────

type DevicePackage struct {
	PackageName string    `json:"package_name"`
	AppName     string    `json:"app_name"`
	VersionName string    `json:"version_name"`
	IsSystem    bool      `json:"is_system"`
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
		systems := make([]bool, len(packages))
		for i, p := range packages {
			names[i] = p.PackageName
			appNames[i] = p.AppName
			versions[i] = p.VersionName
			systems[i] = p.IsSystem
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO device_packages (device_id, package_name, app_name, version_name, is_system)
			SELECT $1, unnest($2::text[]), unnest($3::text[]), unnest($4::text[]), unnest($5::bool[])
			ON CONFLICT (device_id, package_name) DO UPDATE SET app_name = EXCLUDED.app_name, version_name = EXCLUDED.version_name, is_system = EXCLUDED.is_system, updated_at = NOW()
		`, deviceID, names, appNames, versions, systems); err != nil {
			return err
		}
	}

	return tx.Commit(ctx)
}

func (d *DB) GetDevicePackages(ctx context.Context, deviceID uuid.UUID) ([]DevicePackage, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT package_name, app_name, version_name, is_system, updated_at
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
		if err := rows.Scan(&p.PackageName, &p.AppName, &p.VersionName, &p.IsSystem, &p.UpdatedAt); err != nil {
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
			WHERE (dp.package_name ILIKE $1 OR dp.app_name ILIKE $1) AND NOT dp.is_system
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
			WHERE NOT dp.is_system
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
	// Telemetry is change-gated, so check-ins are irregular in time. Average/fraction/
	// duration aggregates are therefore TIME-WEIGHTED: each sample is weighted by the gap to
	// the next sample (LEAD), capped at 600 s so a long silent/offline stretch can't dominate.
	// The day's last sample has no successor (NULL gap → weight 0); when all weights are 0
	// (e.g. a single sample) we fall back to the plain row average. MAX/MIN/last-value
	// aggregates are insensitive to sampling cadence and stay as-is.
	tag, err := d.pool.Exec(ctx, `
		WITH samples AS (
			SELECT c.device_id, c.battery_pct, c.build_id, c.created_at, c.extra,
				COALESCE(LEAST(EXTRACT(EPOCH FROM (
					LEAD(c.created_at) OVER (PARTITION BY c.device_id ORDER BY c.created_at)
					- c.created_at)), 600), 0) AS w
			FROM checkins c
			WHERE c.created_at >= $1::date AND c.created_at < ($1::date + INTERVAL '1 day')
		)
		INSERT INTO device_daily_stats AS s (
			device_id, day, checkin_count, battery_min, battery_max, battery_avg,
			temp_max, ram_pct_peak, charging_frac, online_minutes, build_id,
			first_seen_at, last_seen_at,
			wlc_guest_frac, pad_readable, storage_free_last_gb, computed_at)
		SELECT
			device_id,
			$1::date,
			COUNT(*),
			MIN(battery_pct),
			MAX(battery_pct),
			COALESCE((SUM(battery_pct * w) / NULLIF(SUM(w), 0))::real, AVG(battery_pct)::real),
			MAX((extra->>'battery_temp_c')::numeric)::real,
			MAX(COALESCE(
				((extra->'ram_usage_mb'->>'used')::numeric * 100)
					/ NULLIF((extra->'ram_usage_mb'->>'total')::numeric, 0),
				0))::smallint,
			COALESCE(
				(SUM(CASE WHEN (extra->>'charging')::boolean THEN w ELSE 0 END) / NULLIF(SUM(w), 0))::real,
				AVG(CASE WHEN (extra->>'charging')::boolean THEN 1 ELSE 0 END)::real),
			(SUM(w) / 60.0)::int,
			(ARRAY_AGG(build_id ORDER BY created_at DESC))[1],
			MIN(created_at),
			MAX(created_at),
			-- T7 pad utilisation: time-weighted fraction with a guest device on the pad,
			-- whether the pad was readable at all that day, and the day's last storage reading.
			COALESCE(
				(SUM(CASE WHEN extra->>'wlc_status' = '1' THEN w ELSE 0 END) / NULLIF(SUM(w), 0))::real,
				AVG(CASE WHEN extra->>'wlc_status' = '1' THEN 1 ELSE 0 END)::real),
			bool_or(extra->>'wlc_status' IS NOT NULL AND extra->>'wlc_status' <> '-1'),
			(ARRAY_AGG((extra->>'storage_free_gb')::numeric ORDER BY created_at DESC))[1]::real,
			NOW()
		FROM samples
		GROUP BY device_id
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
			wlc_guest_frac       = EXCLUDED.wlc_guest_frac,
			pad_readable         = EXCLUDED.pad_readable,
			storage_free_last_gb = EXCLUDED.storage_free_last_gb,
			computed_at    = EXCLUDED.computed_at
	`, dayStr)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// backfillFlag marks the one-time historical daily-stats backfill as done, so it never
// re-runs on subsequent deploys (the hourly housekeeping keeps recent days current).
const backfillFlag = "daily_stats_backfilled_v1"

// BackfillDailyStats rolls up recent calendar days that have checkins but no
// device_daily_stats row yet, so historical telemetry is captured the first time the
// feature is deployed. Bounded to the last maxDays days (checkins beyond retention are
// pruned, so older days have no data to roll up) and probed with cheap indexed EXISTS
// queries — no full-table scan. Runs at most once ever (guarded by app_flags), so every
// later deploy returns immediately and startup stays fast. Returns days processed.
func (d *DB) BackfillDailyStats(ctx context.Context, maxDays int) (int, error) {
	var done bool
	if err := d.pool.QueryRow(ctx,
		`SELECT EXISTS(SELECT 1 FROM app_flags WHERE flag = $1)`, backfillFlag).Scan(&done); err != nil {
		return 0, err
	}
	if done {
		return 0, nil
	}
	if maxDays <= 0 || maxDays > 120 {
		maxDays = 120 // cap work even when retention is "keep forever"
	}
	today := time.Now().UTC()
	n := 0
	for i := 1; i <= maxDays; i++ { // start at yesterday; today is owned by housekeeping
		dayStr := today.AddDate(0, 0, -i).Format("2006-01-02")
		var rolled, hasCheckins bool
		if err := d.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM device_daily_stats WHERE day = $1::date)`, dayStr).Scan(&rolled); err != nil {
			return n, err
		}
		if rolled {
			continue
		}
		if err := d.pool.QueryRow(ctx,
			`SELECT EXISTS(SELECT 1 FROM checkins WHERE created_at >= $1::date AND created_at < ($1::date + INTERVAL '1 day'))`,
			dayStr).Scan(&hasCheckins); err != nil {
			return n, err
		}
		if !hasCheckins {
			continue
		}
		day, _ := time.Parse("2006-01-02", dayStr)
		if _, err := d.RollupDailyStats(ctx, day); err != nil {
			return n, err
		}
		n++
	}
	_, err := d.pool.Exec(ctx, `INSERT INTO app_flags (flag) VALUES ($1) ON CONFLICT DO NOTHING`, backfillFlag)
	return n, err
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
	BatteryAvg     *float64  `json:"battery_avg"`     // recent avg daily peak battery (overnight fullness)
	BatteryDelta   *float64  `json:"battery_delta"`   // recent minus prior week (negative = declining)
	ChargingAvg    *float64  `json:"charging_avg"`    // recent avg charging coverage (0-1)
	TempMax        *float64  `json:"temp_max"`        // hottest device in the window
	TempMaxSerial  *string   `json:"temp_max_serial"` // serial of the device that hit TempMax
	DistinctBuilds int       `json:"distinct_builds"`
	Deployed       bool      `json:"deployed"`       // true for restaurants (a venue); false for tag-groups
	DeployedCount  int       `json:"deployed_count"` // devices that are deployed (assigned to a restaurant)
	Score          int       `json:"score"`          // 0-100, higher is healthier
	ScoreClass     string    `json:"score_class"`    // ok | warn | danger (for badge styling)
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
				COUNT(*) FILTER (WHERE d.last_seen_at < NOW() - ($1 * INTERVAL '1 second')) AS offline_count,
				COUNT(*) FILTER (WHERE d.restaurant_id IS NOT NULL) AS deployed_count
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
			recent.charging_avg, recent.temp_max, COALESCE(recent.builds, 0),
			false, COALESCE(devs.deployed_count, 0)  -- a group is a tag, not a venue
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
			&g.ChargingAvg, &g.TempMax, &g.DistinctBuilds, &g.Deployed, &g.DeployedCount); err != nil {
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
	ID           uuid.UUID       `json:"id"`
	Type         string          `json:"type"`
	Name         string          `json:"name"`
	Enabled      bool            `json:"enabled"`
	Params       json.RawMessage `json:"params"`
	ScopeType    string          `json:"scope_type"`
	ScopeID      *uuid.UUID      `json:"scope_id"`
	ActiveWindow string          `json:"active_window"` // "" / always | service | overnight
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
	FiredAt        time.Time       `json:"fired_at"`
	LastSeenAt     time.Time       `json:"last_seen_at"`
	ResolvedAt     *time.Time      `json:"resolved_at"`
	UpdatedAt      time.Time       `json:"updated_at"`
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
	{"overheating", "Device overheating", `{"temp_c":45}`, "always", true},
	// Memory pressure gives the report a configurable RAM cutoff; off by default.
	{"memory_pressure", "Memory pressure", `{"ram_pct":85}`, "always", false},
	{"storage_filling", "Storage filling fast", `{"low_gb":1.5,"drop_gb":0.2}`, "always", true},
	// Recent-tier rules (T7 matrix).
	{"offline", "Device offline", `{"offline_minutes":5}`, "always", true},
	{"storage_low", "Storage critically low", `{"free_gb":0.5}`, "always", true},
	{"temp_elevated", "Temperature elevated", `{"temp_min":38,"temp_max":45}`, "always", true},
	{"memory_low", "Memory low (available)", `{"avail_mb":400}`, "always", true},
	{"wifi_weak", "Weak Wi-Fi signal", `{"rssi_dbm":-75,"sustain_min":10}`, "always", true},
}

// EnsureDefaultRules inserts each default rule only if no rule of that type exists.
func (d *DB) EnsureDefaultRules(ctx context.Context) error {
	for _, r := range defaultAlertRules {
		if _, err := d.pool.Exec(ctx, `
			INSERT INTO alert_rules (type, name, enabled, params, active_window)
			SELECT $1, $2, $3, $4::jsonb, $5
			WHERE NOT EXISTS (SELECT 1 FROM alert_rules WHERE type = $1)
		`, r.Type, r.Name, r.Enabled, r.Params, r.ActiveWindow); err != nil {
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
	q := `SELECT id, type, name, enabled, params, scope_type, scope_id, active_window, created_at FROM alert_rules`
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
		if err := rows.Scan(&r.ID, &r.Type, &r.Name, &r.Enabled, &r.Params, &r.ScopeType, &r.ScopeID, &r.ActiveWindow, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// UpdateAlertRule sets a rule's enabled flag and threshold params. activeWindow is
// applied only when non-empty (so updating a non-windowed rule leaves it unchanged);
// pass "always"/"service"/"overnight" to set it.
func (d *DB) UpdateAlertRule(ctx context.Context, id uuid.UUID, enabled bool, params json.RawMessage, activeWindow string) error {
	if len(params) == 0 {
		params = json.RawMessage("{}")
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE alert_rules
		SET enabled = $2, params = $3::jsonb,
		    active_window = COALESCE(NULLIF($4, ''), active_window)
		WHERE id = $1
	`, id, enabled, params, activeWindow)
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
	// On a re-fire of the same open (type, device) condition, bump the occurrence
	// count and refresh the summary/detail/last-seen instead of dropping the row.
	// The (xmax = 0) flag is true only for a genuine INSERT, so callers still
	// broadcast/notify exactly once per distinct occurrence (not on every re-fire).
	var inserted bool
	err := d.pool.QueryRow(ctx, `
		INSERT INTO alerts (rule_id, type, device_id, severity, summary, detail)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)
		ON CONFLICT (type, device_id) WHERE status <> 'resolved'
		DO UPDATE SET occurrences  = alerts.occurrences + 1,
		              last_seen_at  = NOW(),
		              severity      = EXCLUDED.severity,
		              summary       = EXCLUDED.summary,
		              detail        = EXCLUDED.detail,
		              updated_at    = NOW()
		RETURNING (xmax = 0)
	`, ruleID, typ, deviceID, severity, summary, detailJSON).Scan(&inserted)
	if err != nil {
		return false, err
	}
	return inserted, nil
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
		       COALESCE(r.name, ''), a.severity, a.status, a.summary, a.detail,
		       a.occurrences, a.fired_at, a.last_seen_at, a.resolved_at, a.updated_at
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
			&a.Occurrences, &a.FiredAt, &a.LastSeenAt, &a.ResolvedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AlertSummaryCounts returns headline counts for the alerts page in a single query.
func (d *DB) AlertSummaryCounts(ctx context.Context) (AlertSummary, error) {
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
	`).Scan(&s.Total, &s.Open, &s.Acknowledged, &s.Resolved, &s.Critical, &s.Warning, &s.Info)
	return s, err
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
func (d *DB) DeleteAllAlerts(ctx context.Context) (int64, error) {
	tag, err := d.pool.Exec(ctx, `DELETE FROM alerts`)
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

// DeviceAIAnalysis is one stored AI reading of a device.
type DeviceAIAnalysis struct {
	ID        int64     `json:"id"`
	Model     string    `json:"model"`
	Text      string    `json:"text"`
	CreatedAt time.Time `json:"created_at"`
}

// SaveDeviceAIAnalysis records one analysis; old rows beyond the most recent 20
// per device are trimmed so the history stays small.
func (d *DB) SaveDeviceAIAnalysis(ctx context.Context, deviceID uuid.UUID, model, text string) error {
	if _, err := d.pool.Exec(ctx,
		`INSERT INTO device_ai_analyses (device_id, model, text) VALUES ($1, $2, $3)`,
		deviceID, model, text); err != nil {
		return err
	}
	_, _ = d.pool.Exec(ctx, `
		DELETE FROM device_ai_analyses
		WHERE device_id = $1 AND id NOT IN (
			SELECT id FROM device_ai_analyses WHERE device_id = $1 ORDER BY created_at DESC LIMIT 20
		)`, deviceID)
	return nil
}

// ListDeviceAIAnalyses returns a device's recent analyses, newest first.
func (d *DB) ListDeviceAIAnalyses(ctx context.Context, deviceID uuid.UUID, limit int) ([]DeviceAIAnalysis, error) {
	if limit <= 0 {
		limit = 20
	}
	rows, err := d.pool.Query(ctx,
		`SELECT id, model, text, created_at FROM device_ai_analyses
		 WHERE device_id = $1 ORDER BY created_at DESC LIMIT $2`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceAIAnalysis
	for rows.Next() {
		var a DeviceAIAnalysis
		if err := rows.Scan(&a.ID, &a.Model, &a.Text, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
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
	// EventAt is when the underlying problem occurred (e.g. the peak-temperature
	// reading); falls back to the detection time. Timezone is the device's reported
	// IANA zone for rendering EventAt locally ("" → UTC).
	EventAt  time.Time
	Timezone string
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

// localTime converts UTC now to the venue's/device's local time. It accepts an IANA
// zone name ("America/New_York", DST-aware) or a legacy "GMT±h" offset; unknown or
// empty falls back to UTC. (tzdata is embedded via the time/tzdata import in main.)
func localTime(now time.Time, tz string) time.Time {
	if tz != "" && !strings.HasPrefix(tz, "GMT") {
		if loc, err := time.LoadLocation(tz); err == nil {
			return now.In(loc)
		}
	}
	return now.UTC().Add(time.Duration(gmtOffset(tz)) * time.Hour)
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

// ── Service windows (Tier 5 §9) ─────────────────────────────────────────────────

// ServiceWindow is a venue's open/close + overnight hours, in local minutes past
// midnight. TZ is the GMT±offset string used to convert UTC "now" to local time;
// empty TZ means fall back to the device's own reported extra.timezone.
type ServiceWindow struct {
	RestaurantID  *uuid.UUID `json:"restaurant_id"`
	OpenMin       int        `json:"open_min"`
	CloseMin      int        `json:"close_min"`
	NightOpenMin  int        `json:"night_open_min"`
	NightCloseMin int        `json:"night_close_min"`
	TZ            string     `json:"timezone"`
}

// inMinWindow reports whether local minute m falls in [start,end), supporting windows
// that wrap past midnight (e.g. 1410→360). start==end means "never".
func inMinWindow(m, start, end int) bool {
	if start == end {
		return false
	}
	if start < end {
		return m >= start && m < end
	}
	return m >= start || m < end
}

// inActiveWindow reports whether a device with reported timezone devTZ is currently
// inside the rule's active window. aw is "" / "always" (always true), "service", or
// "overnight". w.TZ overrides devTZ when set (per-venue timezone).
func inActiveWindow(now time.Time, devTZ string, w ServiceWindow, aw string) bool {
	if aw == "" || aw == "always" {
		return true
	}
	tz := w.TZ
	if tz == "" {
		tz = devTZ
	}
	lt := localTime(now, tz)
	localMin := lt.Hour()*60 + lt.Minute()
	switch aw {
	case "service":
		return inMinWindow(localMin, w.OpenMin, w.CloseMin)
	case "overnight":
		return inMinWindow(localMin, w.NightOpenMin, w.NightCloseMin)
	}
	return true
}

// GetFleetServiceWindow returns the fleet-default service window (the row with both
// restaurant_id and group_id NULL, seeded by the migration).
func (d *DB) GetFleetServiceWindow(ctx context.Context) (ServiceWindow, error) {
	var w ServiceWindow
	err := d.pool.QueryRow(ctx, `
		SELECT open_min, close_min, night_open_min, night_close_min, timezone
		FROM service_windows WHERE restaurant_id IS NULL AND group_id IS NULL`).
		Scan(&w.OpenMin, &w.CloseMin, &w.NightOpenMin, &w.NightCloseMin, &w.TZ)
	if errors.Is(err, pgx.ErrNoRows) {
		// Defensive fallback matching the migration defaults (07:00–23:00 / 23:30–06:00).
		return ServiceWindow{OpenMin: 420, CloseMin: 1380, NightOpenMin: 1410, NightCloseMin: 360}, nil
	}
	return w, err
}

// FleetWindowActive reports whether the fleet-default service window says we are
// currently inside aw ("" / always | service | overnight). Used to gate channel
// delivery (e.g. an on-call channel that should only page during service hours).
func (d *DB) FleetWindowActive(ctx context.Context, aw string) bool {
	if aw == "" || aw == "always" {
		return true
	}
	w, err := d.GetFleetServiceWindow(ctx)
	if err != nil {
		return true // fail open: better a stray notification than a silently dropped alert
	}
	return inActiveWindow(time.Now().UTC(), w.TZ, w, aw)
}

// GetRestaurantServiceWindow returns a restaurant's own service window and whether it has
// one. When the restaurant has no row, ok is false and the fleet default is returned.
func (d *DB) GetRestaurantServiceWindow(ctx context.Context, restaurantID uuid.UUID) (w ServiceWindow, ok bool, err error) {
	err = d.pool.QueryRow(ctx, `
		SELECT open_min, close_min, night_open_min, night_close_min, timezone
		FROM service_windows WHERE restaurant_id = $1`, restaurantID).
		Scan(&w.OpenMin, &w.CloseMin, &w.NightOpenMin, &w.NightCloseMin, &w.TZ)
	if errors.Is(err, pgx.ErrNoRows) {
		fleet, ferr := d.GetFleetServiceWindow(ctx)
		return fleet, false, ferr
	}
	if err != nil {
		return ServiceWindow{}, false, err
	}
	id := restaurantID
	w.RestaurantID = &id
	return w, true, nil
}

// DeleteServiceWindow removes a restaurant's window so it falls back to the fleet default.
func (d *DB) DeleteServiceWindow(ctx context.Context, restaurantID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM service_windows WHERE restaurant_id = $1`, restaurantID)
	return err
}

// SetServiceWindow upserts a restaurant's service window (pass a nil RestaurantID to set
// the fleet default).
func (d *DB) SetServiceWindow(ctx context.Context, w ServiceWindow) error {
	if w.RestaurantID == nil {
		// Fleet default is the single both-NULL row seeded by the migration: update it
		// (ON CONFLICT can't target NULL, which never conflicts).
		_, err := d.pool.Exec(ctx, `
			UPDATE service_windows SET
				open_min = $1, close_min = $2, night_open_min = $3, night_close_min = $4,
				timezone = $5, updated_at = NOW()
			WHERE restaurant_id IS NULL AND group_id IS NULL`,
			w.OpenMin, w.CloseMin, w.NightOpenMin, w.NightCloseMin, w.TZ)
		return err
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO service_windows (restaurant_id, open_min, close_min, night_open_min, night_close_min, timezone, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, NOW())
		ON CONFLICT (restaurant_id) WHERE restaurant_id IS NOT NULL DO UPDATE SET
			open_min = EXCLUDED.open_min, close_min = EXCLUDED.close_min,
			night_open_min = EXCLUDED.night_open_min, night_close_min = EXCLUDED.night_close_min,
			timezone = EXCLUDED.timezone, updated_at = NOW()
	`, w.RestaurantID, w.OpenMin, w.CloseMin, w.NightOpenMin, w.NightCloseMin, w.TZ)
	return err
}

// effectiveWindows returns the resolved service window for every non-hidden device:
// its restaurant's window if it has one, else the fleet default. Loaded once per
// evaluation pass so window-gated rules don't re-query per device.
func (d *DB) effectiveWindows(ctx context.Context) (map[uuid.UUID]ServiceWindow, error) {
	fleet, err := d.GetFleetServiceWindow(ctx)
	if err != nil {
		return nil, err
	}
	rows, err := d.pool.Query(ctx, `
		SELECT d.id, COALESCE(d.latest_extra->>'timezone', ''),
		       sw.open_min, sw.close_min, sw.night_open_min, sw.night_close_min, sw.timezone
		FROM devices d
		LEFT JOIN service_windows sw ON sw.restaurant_id = d.restaurant_id
		WHERE NOT d.hidden`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]ServiceWindow)
	for rows.Next() {
		var id uuid.UUID
		var devTZ string
		var open, closeM, nOpen, nClose *int
		var winTZ *string
		if err := rows.Scan(&id, &devTZ, &open, &closeM, &nOpen, &nClose, &winTZ); err != nil {
			return nil, err
		}
		w := fleet // copy fleet defaults, override with the group's window when present
		if open != nil {
			w.OpenMin, w.CloseMin, w.NightOpenMin, w.NightCloseMin = *open, *closeM, *nOpen, *nClose
			if winTZ != nil {
				w.TZ = *winTZ
			}
		}
		// Bake the effective tz: the venue window's tz wins, else the device's own.
		if w.TZ == "" {
			w.TZ = devTZ
		}
		out[id] = w
	}
	return out, rows.Err()
}

// deployedDeviceSet returns the IDs of devices that are deployed — i.e. assigned to a
// restaurant. Operational service-window rules from the T7 matrix only apply to these: a
// bench/lab unit (no restaurant) idle or unplugged during the default day window is
// expected, not an alert. Hardware rules (storage_low, temp_elevated, overheating) still
// fire fleet-wide so genuine lab faults surface. Restaurant assignment IS the deploy signal.
func (d *DB) deployedDeviceSet(ctx context.Context) (map[uuid.UUID]bool, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.id, (d.restaurant_id IS NOT NULL)
		FROM devices d
		WHERE NOT d.hidden`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]bool)
	for rows.Next() {
		var id uuid.UUID
		var dep bool
		if err := rows.Scan(&id, &dep); err != nil {
			return nil, err
		}
		if dep {
			out[id] = true
		}
	}
	return out, rows.Err()
}

// windowFor returns the resolved window for a device, falling back to fleet defaults
// (already baked into the map) or a hard default if the device is absent.
func windowFor(m map[uuid.UUID]ServiceWindow, id uuid.UUID) ServiceWindow {
	if w, ok := m[id]; ok {
		return w
	}
	return ServiceWindow{OpenMin: 420, CloseMin: 1380, NightOpenMin: 1410, NightCloseMin: 360}
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
		if isRecentType(r.Type) {
			continue // owned by the recent tier (EvaluateRecentAlerts)
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
				at, tz := notifyTimeFrom(h.Detail)
				created = append(created, AlertNotification{Type: r.Type, Severity: severity, Summary: h.Summary, Serial: h.Serial, EventAt: at, Timezone: tz})
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
			localHour := localTime(now, tz).Hour()
			if inQuiet(localHour, qs, qe) {
				continue
			}
			down := int(now.Sub(lastSeen).Minutes())
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Offline — last check-in %dm ago", down),
				map[string]any{"offline_minutes": down, "last_seen": lastSeen, "timezone": tz}})
		}
		return hits, "critical", rows.Err()

	case "memory_pressure":
		limit := param(p, "ram_pct", 85)
		rows, err := d.pool.Query(ctx, `
			SELECT s.device_id, dv.serial_number, s.ram_pct_peak
			FROM device_daily_stats s JOIN devices dv ON dv.id = s.device_id
			WHERE s.day = CURRENT_DATE AND s.ram_pct_peak >= $1`, limit)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var ram int
			if err := rows.Scan(&id, &serial, &ram); err != nil {
				return nil, "warning", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Peak RAM hit %d%% today (limit %.0f%%)", ram, limit),
				map[string]any{"ram_pct": ram, "limit_pct": limit}})
		}
		return hits, "warning", rows.Err()

	case "storage_filling":
		lowGB := param(p, "low_gb", 1.5)
		dropGB := param(p, "drop_gb", 0.2)
		// Today's free storage is below the warning floor, or dropped sharply vs
		// yesterday — but still above the critical floor (storage_low owns < 0.5 GB).
		rows, err := d.pool.Query(ctx, `
			SELECT t.device_id, dv.serial_number, t.today, t.yday FROM (
				SELECT today.device_id,
				       today.storage_free_last_gb AS today,
				       yday.storage_free_last_gb  AS yday
				FROM device_daily_stats today
				LEFT JOIN device_daily_stats yday
				  ON yday.device_id = today.device_id AND yday.day = CURRENT_DATE - 1
				WHERE today.day = CURRENT_DATE
			) t JOIN devices dv ON dv.id = t.device_id
			WHERE t.today IS NOT NULL AND t.today >= 0.5
			  AND (t.today < $1 OR (t.yday IS NOT NULL AND (t.yday - t.today) > $2))`, lowGB, dropGB)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var today float64
			var yday *float64
			if err := rows.Scan(&id, &serial, &today, &yday); err != nil {
				return nil, "warning", err
			}
			summary := fmt.Sprintf("Storage down to %.1f GB free", today)
			if yday != nil && (*yday-today) > dropGB {
				summary = fmt.Sprintf("Storage dropped %.1f→%.1f GB in 24h", *yday, today)
			}
			hits = append(hits, alertHit{id, serial, summary,
				map[string]any{"today_gb": today, "low_gb": lowGB, "drop_gb": dropGB}})
		}
		return hits, "warning", rows.Err()
	}
	return nil, "warning", nil
}

// ── Recent-tier alert evaluation (Tier 5 §10) ───────────────────────────────────
//
// The daily evaluator (EvaluateAlerts) runs hourly over device_daily_stats and serves
// trend rules. Point-in-time and short-window rate/sustained rules from the T7 matrix
// need finer granularity, so they run here on a 1-minute ticker over devices.latest_extra
// and recent `checkins` rows. Both tiers feed the same `alerts` table and dedupe/resolve
// machinery; each rule type belongs to exactly one tier (recentRuleTypes below) so a rule
// is never evaluated — and spuriously resolved — by the wrong tier.

// recentRuleTypes is the set of rule types evaluated by the recent tier.
var recentRuleTypes = map[string]bool{
	"offline":       true,
	"overheating":   true,
	"storage_low":   true,
	"temp_elevated": true,
	"memory_low":    true,
	"wifi_weak":     true,
}

func isRecentType(typ string) bool { return recentRuleTypes[typ] }

// defaultActiveWindow is the window a recent rule is gated to when its active_window
// column is unset. All current rules are fleet-wide ("always"); the window machinery
// (service/overnight, deployed-only) stays in EvaluateRecentAlerts for future rules.
func defaultActiveWindow(typ string) string {
	return "always"
}

// EvaluateRecentAlerts runs every enabled recent-tier rule against current telemetry,
// gating each hit to the rule's active window (per-venue service hours), then creating
// and auto-resolving alerts exactly like EvaluateAlerts. Called from the 1-minute ticker.
func (d *DB) EvaluateRecentAlerts(ctx context.Context) (created []AlertNotification, resolved int, err error) {
	rules, err := d.ListAlertRules(ctx, true)
	if err != nil {
		return nil, 0, err
	}
	windows, err := d.effectiveWindows(ctx)
	if err != nil {
		return nil, 0, err
	}
	deployed, err := d.deployedDeviceSet(ctx)
	if err != nil {
		return nil, 0, err
	}
	now := time.Now().UTC()
	for _, r := range rules {
		if !isRecentType(r.Type) || r.ScopeType != "fleet" {
			continue
		}
		var p map[string]float64
		if len(r.Params) > 0 {
			_ = json.Unmarshal(r.Params, &p)
		}
		hits, severity, e := d.detectRecentRule(ctx, r.Type, p)
		if e != nil {
			return created, resolved, e
		}
		aw := r.ActiveWindow
		if aw == "" {
			aw = defaultActiveWindow(r.Type)
		}
		windowed := aw == "service" || aw == "overnight"
		ids := make([]uuid.UUID, 0, len(hits))
		ruleID := r.ID
		for _, h := range hits {
			// Window-gated rules are operational (assume the unit is live in a restaurant
			// on a service/charge schedule), so they apply to deployed units only — a
			// bench/lab unit idle or unplugged during the day window is expected, not an
			// alert. Hardware rules (always-on) still fire fleet-wide.
			if windowed && !deployed[h.DeviceID] {
				continue
			}
			// Skip devices outside the rule's active window; they auto-resolve below.
			if !inActiveWindow(now, "", windowFor(windows, h.DeviceID), aw) {
				continue
			}
			ids = append(ids, h.DeviceID)
			ok, e := d.CreateAlertIfAbsent(ctx, &ruleID, r.Type, h.DeviceID, severity, h.Summary, h.Detail)
			if e != nil {
				return created, resolved, e
			}
			if ok {
				at, tz := notifyTimeFrom(h.Detail)
				created = append(created, AlertNotification{Type: r.Type, Severity: severity, Summary: h.Summary, Serial: h.Serial, EventAt: at, Timezone: tz})
			}
		}
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

// recentReportingCutoff bounds "current" telemetry: a device silent longer than this is
// handled by the offline rule, not the SoC/pad/storage rules (whose latest_extra is stale).
const recentReportingCutoff = "15 minutes"

// detectRecentRule returns devices currently violating a recent-tier rule. Window gating
// is applied by the caller (EvaluateRecentAlerts).
func (d *DB) detectRecentRule(ctx context.Context, typ string, p map[string]float64) ([]alertHit, string, error) {
	switch typ {
	case "offline":
		mins := int(param(p, "offline_minutes", 5))
		rows, err := d.pool.Query(ctx, `
			SELECT id, serial_number, last_seen_at FROM devices
			WHERE NOT hidden AND last_seen_at < NOW() - ($1 * INTERVAL '1 minute')`, mins)
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		now := time.Now().UTC()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var last time.Time
			if err := rows.Scan(&id, &serial, &last); err != nil {
				return nil, "critical", err
			}
			down := int(now.Sub(last).Minutes())
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Offline — last check-in %dm ago", down),
				map[string]any{"offline_minutes": down, "last_seen": last}})
		}
		return hits, "critical", rows.Err()

	case "storage_low":
		freeGB := param(p, "free_gb", 0.5)
		rows, err := d.pool.Query(ctx, `
			SELECT d.id, d.serial_number, (d.latest_extra->>'storage_free_gb')::numeric
			FROM devices d
			WHERE NOT d.hidden AND d.last_seen_at > NOW() - INTERVAL '`+recentReportingCutoff+`'
			  AND (d.latest_extra->>'storage_free_gb')::numeric < $1`, freeGB)
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var free float64
			if err := rows.Scan(&id, &serial, &free); err != nil {
				return nil, "critical", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Only %.0f MB storage free", free*1024),
				map[string]any{"storage_free_gb": free, "limit_gb": freeGB}})
		}
		return hits, "critical", rows.Err()

	case "overheating":
		limit := param(p, "temp_c", 45)
		// Point-in-time: the device's most recent reading is at/over the limit. EventAt is
		// the check-in that reported it, so the alert lands within ~1 min of the spike and
		// auto-resolves once it cools.
		rows, err := d.pool.Query(ctx, `
			SELECT d.id, d.serial_number, (d.latest_extra->>'battery_temp_c')::numeric,
			       d.last_seen_at, d.latest_extra->>'timezone'
			FROM devices d
			WHERE NOT d.hidden AND d.last_seen_at > NOW() - INTERVAL '`+recentReportingCutoff+`'
			  AND (d.latest_extra->>'battery_temp_c')::numeric >= $1`, limit)
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var temp float64
			var seen time.Time
			var tz *string
			if err := rows.Scan(&id, &serial, &temp, &seen, &tz); err != nil {
				return nil, "critical", err
			}
			detail := map[string]any{"temp_c": temp, "limit_c": limit, "event_at": seen}
			if tz != nil && *tz != "" {
				detail["timezone"] = *tz
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Device temperature is %.0f°C (limit %.0f°C)", temp, limit),
				detail})
		}
		return hits, "critical", rows.Err()

	case "temp_elevated":
		lo := param(p, "temp_min", 38)
		hi := param(p, "temp_max", 45)
		// Sustained: every reading in the last ~20 min is in [lo,hi) and spans ≥14 min.
		rows, err := d.pool.Query(ctx, `
			SELECT c.device_id, dv.serial_number, MAX(c.temp) FROM (
				SELECT device_id, (extra->>'battery_temp_c')::numeric AS temp, created_at
				FROM checkins WHERE created_at > NOW() - INTERVAL '20 minutes'
			) c JOIN devices dv ON dv.id = c.device_id
			WHERE c.temp IS NOT NULL
			GROUP BY c.device_id, dv.serial_number
			HAVING MIN(c.temp) >= $1 AND MAX(c.temp) < $2
			   AND (MAX(c.created_at) - MIN(c.created_at)) >= INTERVAL '14 minutes'`, lo, hi)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var tmax float64
			if err := rows.Scan(&id, &serial, &tmax); err != nil {
				return nil, "warning", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Device held %.0f–%.0f°C for >15 min (peak %.0f°C)", lo, hi, tmax),
				map[string]any{"temp_min": lo, "temp_max": hi, "peak_c": tmax}})
		}
		return hits, "warning", rows.Err()

	case "memory_low":
		availMB := param(p, "avail_mb", 400)
		// Sustained low available RAM (total-used) over the last ~10 min: even the
		// highest reading in the window stays below the floor. Always-on hardware rule.
		rows, err := d.pool.Query(ctx, `
			SELECT c.device_id, dv.serial_number, MAX(c.avail) FROM (
				SELECT device_id,
					((extra->'ram_usage_mb'->>'total')::numeric - (extra->'ram_usage_mb'->>'used')::numeric) AS avail,
					created_at
				FROM checkins WHERE created_at > NOW() - INTERVAL '12 minutes'
			) c JOIN devices dv ON dv.id = c.device_id
			WHERE c.avail IS NOT NULL AND NOT dv.hidden
			GROUP BY c.device_id, dv.serial_number
			HAVING MAX(c.avail) < $1 AND (MAX(c.created_at) - MIN(c.created_at)) >= INTERVAL '8 minutes'`, availMB)
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var avail float64
			if err := rows.Scan(&id, &serial, &avail); err != nil {
				return nil, "critical", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Available RAM held under %.0f MB for >8 min (peak %.0f MB)", availMB, avail),
				map[string]any{"avail_mb": avail, "limit_mb": availMB}})
		}
		return hits, "critical", rows.Err()

	case "wifi_weak":
		thr := param(p, "rssi_dbm", -75)
		sustain := param(p, "sustain_min", 10)
		// Sustained weak connected-link RSSI: even the strongest reading in the window
		// stays below (more negative than) the floor, spanning ≥ sustain_min. RSSI is the
		// connected AP's signal (extra.wifi_rssi), not the scan list. Always-on rule.
		rows, err := d.pool.Query(ctx, `
			SELECT c.device_id, dv.serial_number, MAX(c.rssi) FROM (
				SELECT device_id, (extra->>'wifi_rssi')::numeric AS rssi, created_at
				FROM checkins WHERE created_at > NOW() - INTERVAL '15 minutes'
			) c JOIN devices dv ON dv.id = c.device_id
			WHERE c.rssi IS NOT NULL AND NOT dv.hidden
			GROUP BY c.device_id, dv.serial_number
			HAVING MAX(c.rssi) < $1 AND (MAX(c.created_at) - MIN(c.created_at)) >= ($2 * INTERVAL '1 minute')`, thr, sustain)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var best float64
			if err := rows.Scan(&id, &serial, &best); err != nil {
				return nil, "warning", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Wi-Fi signal held below %.0f dBm for >%.0f min (best %.0f dBm)", thr, sustain, best),
				map[string]any{"rssi_dbm": best, "limit_dbm": thr}})
		}
		return hits, "warning", rows.Err()
	}
	return nil, "warning", nil
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
	// Exact counts for the small tables; instant planner estimates (pg_class.reltuples,
	// maintained by autovacuum/ANALYZE) for the large high-churn ones — an exact
	// count(*) on checkins/logcat_results full-scans millions of rows and was making the
	// Settings page take seconds to load. Estimates are fine for a stats readout.
	err := d.pool.QueryRow(ctx, `
		SELECT (SELECT count(*) FROM devices),
		       (SELECT GREATEST(reltuples, 0)::bigint FROM pg_class WHERE oid = 'checkins'::regclass),
		       (SELECT count(*) FROM commands),
		       (SELECT GREATEST(reltuples, 0)::bigint FROM pg_class WHERE oid = 'logcat_results'::regclass)
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
ALTER TABLE device_packages ADD COLUMN IF NOT EXISTS is_system BOOLEAN NOT NULL DEFAULT FALSE;

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
    type       TEXT NOT NULL,                 -- overheating | temp_elevated | offline | storage_low | ...
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

-- Per-device AI analyses, kept as a short history so the device page can show
-- past readings alongside a fresh one.
CREATE TABLE IF NOT EXISTS device_ai_analyses (
    id         BIGSERIAL PRIMARY KEY,
    device_id  UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    model      TEXT NOT NULL DEFAULT '',
    text       TEXT NOT NULL DEFAULT '',
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_device_ai_analyses_device ON device_ai_analyses (device_id, created_at DESC);

-- One-time data migrations that must run exactly once (no versioned migration tool).
CREATE TABLE IF NOT EXISTS app_flags (
    flag       TEXT PRIMARY KEY,
    applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Offline alerts were de-prioritized: disable the existing offline rule once, so an
-- upgrade reflects the new default without clobbering it if the admin re-enables it.
DO $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM app_flags WHERE flag = 'offline_rule_disabled_v1') THEN
        UPDATE alert_rules SET enabled = false WHERE type = 'offline';
        INSERT INTO app_flags (flag) VALUES ('offline_rule_disabled_v1');
    END IF;
END $$;

-- (The legacy deployed flags on groups/devices/restaurants are dropped further down:
-- a device is now "deployed" iff it is assigned to a restaurant — see the DROP COLUMN
-- statements at the end of this migration.)

-- Server-side dashboard sessions (GB-04/GB-08). The cookie carries only the
-- opaque session id; identity, role and validity live here so logout and admin
-- revoke delete the row immediately and a single stolen cookie can be killed
-- without rotating the signing key for everyone. user_id is NULL for the static
-- env-configured admin; ON DELETE CASCADE revokes a DB user's sessions when the
-- user is removed.
CREATE TABLE IF NOT EXISTS sessions (
    id         TEXT PRIMARY KEY,
    user_id    UUID REFERENCES users(id) ON DELETE CASCADE,
    username   TEXT NOT NULL,
    role       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    last_seen  TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    expires_at TIMESTAMPTZ NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);
CREATE INDEX IF NOT EXISTS idx_sessions_expires ON sessions(expires_at);

-- Per-device OTA detail for the deployment view: when the row last changed
-- state (to surface "last updated" and flag devices stalled mid-update) and the
-- error_code the device reported on failure (to show why it failed).
ALTER TABLE update_devices ADD COLUMN IF NOT EXISTS updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
ALTER TABLE update_devices ADD COLUMN IF NOT EXISTS error_code TEXT        NOT NULL DEFAULT '';
-- Update timing: started_at is stamped when the device begins downloading and
-- completed_at when it reports installed, so we can show per-device "time taken"
-- and the deployment's average. Both set once (never bumped by later transitions).
ALTER TABLE update_devices ADD COLUMN IF NOT EXISTS started_at   TIMESTAMPTZ;
ALTER TABLE update_devices ADD COLUMN IF NOT EXISTS completed_at TIMESTAMPTZ;

-- Releases group the packages for one target build version (the full image plus
-- its incrementals) under a single lifecycle (draft -> published -> yanked).
-- Deploying a release lets the server pick the right artifact per device.
CREATE TABLE IF NOT EXISTS releases (
    id           SERIAL      PRIMARY KEY,
    version      TEXT        NOT NULL UNIQUE,
    name         TEXT        NOT NULL DEFAULT '',
    changelog    TEXT        NOT NULL DEFAULT '',
    status       TEXT        NOT NULL DEFAULT 'draft',
    created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    published_at TIMESTAMPTZ
);
ALTER TABLE ota_packages ADD COLUMN IF NOT EXISTS release_id INTEGER REFERENCES releases(id);
ALTER TABLE updates      ADD COLUMN IF NOT EXISTS release_id INTEGER REFERENCES releases(id);

-- Backfill one release per existing target build (falling back to the legacy
-- build_id when target_build_id is blank). Existing packages are marked published
-- so deployments already in flight keep resolving; then link packages and updates.
INSERT INTO releases (version, status, changelog, created_at, published_at)
  SELECT COALESCE(NULLIF(target_build_id,''), build_id), 'published', MAX(changelog), MIN(created_at), MIN(created_at)
  FROM ota_packages
  GROUP BY COALESCE(NULLIF(target_build_id,''), build_id)
  ON CONFLICT (version) DO NOTHING;
UPDATE ota_packages p SET release_id = r.id
  FROM releases r
  WHERE r.version = COALESCE(NULLIF(p.target_build_id,''), p.build_id) AND p.release_id IS NULL;
UPDATE updates u SET release_id = p.release_id
  FROM ota_packages p WHERE p.id = u.ota_package_id AND u.release_id IS NULL;

-- Release deployments pick the per-device artifact at resolve time, so a
-- deployment no longer needs a single package. (DROP NOT NULL is idempotent.)
ALTER TABLE updates ALTER COLUMN ota_package_id DROP NOT NULL;

-- ── Tier 5: T7 alert-matrix (see docs/analytics-and-alerting.md §8) ─────────────

-- Per-restaurant service windows. Rules with active_window=service|overnight gate
-- against a group's open/close hours (local minutes past midnight) so alerts only
-- fire when they matter for that venue. A group with no row uses the fleet default
-- seeded below; overnight = the configured night window (defaults to the matrix's
-- 23:30–06:00). timezone falls back to each device's reported extra.timezone.
-- group_id is nullable: a NULL row is the single fleet-default window; non-NULL rows
-- are per-group overrides. Not a PK (PKs can't be NULL); uniqueness is enforced by the
-- two partial indexes below.
CREATE TABLE IF NOT EXISTS service_windows (
    group_id        UUID REFERENCES groups(id) ON DELETE CASCADE,
    open_min        INTEGER NOT NULL DEFAULT 420,   -- 07:00 local
    close_min       INTEGER NOT NULL DEFAULT 1380,  -- 23:00 local
    night_open_min  INTEGER NOT NULL DEFAULT 1410,  -- 23:30 local
    night_close_min INTEGER NOT NULL DEFAULT 360,   -- 06:00 local
    timezone        TEXT    NOT NULL DEFAULT '',     -- '' = use device-reported tz
    updated_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- one window per group, and at most one fleet-default (NULL) row.
CREATE UNIQUE INDEX IF NOT EXISTS idx_service_windows_group
    ON service_windows (group_id) WHERE group_id IS NOT NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_service_windows_fleet_default
    ON service_windows ((1)) WHERE group_id IS NULL;
INSERT INTO service_windows (group_id) SELECT NULL
    WHERE NOT EXISTS (SELECT 1 FROM service_windows WHERE group_id IS NULL);

-- active_window on a rule: service | overnight | always. NULL/'' = always (back-compat).
ALTER TABLE alert_rules ADD COLUMN IF NOT EXISTS active_window TEXT NOT NULL DEFAULT '';

-- Daily-stats columns for the pad-utilisation and storage-trend rules (§8.1 #11,#12,#23).
ALTER TABLE device_daily_stats ADD COLUMN IF NOT EXISTS wlc_guest_frac      REAL;     -- fraction of checkins with a guest device on the pad (wlc_status=1)
ALTER TABLE device_daily_stats ADD COLUMN IF NOT EXISTS pad_readable        BOOLEAN;  -- pad was readable at all that day (any wlc_status >= 0)
ALTER TABLE device_daily_stats ADD COLUMN IF NOT EXISTS storage_free_last_gb REAL;    -- last storage_free_gb reading of the day, for the 24h-delta rule

-- Alert channels: where fired alerts are routed. Each channel takes alerts at or
-- above min_severity; realtime channels POST immediately, digest channels batch into
-- the daily digest. active_window optionally restricts a channel to service/overnight.
-- The legacy single AlertWebhookURL setting is migrated into one channel in code.
CREATE TABLE IF NOT EXISTS alert_channels (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name          TEXT NOT NULL DEFAULT '',
    kind          TEXT NOT NULL DEFAULT 'webhook',     -- webhook (Slack/Discord/Mattermost-compatible)
    url           TEXT NOT NULL DEFAULT '',
    min_severity  TEXT NOT NULL DEFAULT 'warning',     -- info | warning | critical
    mode          TEXT NOT NULL DEFAULT 'realtime',    -- realtime | digest
    active_window TEXT NOT NULL DEFAULT '',             -- '' = always | service | overnight
    enabled       BOOLEAN NOT NULL DEFAULT true,
    notify_resolve BOOLEAN NOT NULL DEFAULT true,       -- post a one-liner when an alert auto-resolves
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- ── Restaurants (venue object — see docs/release-versioning-and-restaurants.md) ──
-- A restaurant is a real venue a device physically lives in (1 device → at most 1
-- restaurant; NULL = lab/bench unit). Restaurants own the venue semantics that used to
-- be bolted onto free-form groups: the deployed flag, service windows, health and the
-- AI-report bucketing. Groups stay as the test team's free-form tags ("mic issue", …).
CREATE TABLE IF NOT EXISTS restaurants (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name        TEXT NOT NULL,
    address     TEXT NOT NULL DEFAULT '',
    latitude    DOUBLE PRECISION,
    longitude   DOUBLE PRECISION,
    timezone    TEXT NOT NULL DEFAULT '',       -- IANA tz / GMT offset; '' = use device-reported
    notes       TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- A device lives in at most one restaurant; NULL = lab/bench unit.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS restaurant_id UUID REFERENCES restaurants(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_devices_restaurant ON devices(restaurant_id);

-- Service windows move from group_id to restaurant_id. The fleet-default row has both
-- NULL; per-restaurant rows set restaurant_id (group_id stays NULL, now unused). Redefine
-- the fleet-default uniqueness to require BOTH NULL so per-restaurant rows don't collide.
ALTER TABLE service_windows ADD COLUMN IF NOT EXISTS restaurant_id UUID REFERENCES restaurants(id) ON DELETE CASCADE;
DROP INDEX IF EXISTS idx_service_windows_fleet_default;
CREATE UNIQUE INDEX IF NOT EXISTS idx_service_windows_fleet_default
    ON service_windows ((1)) WHERE group_id IS NULL AND restaurant_id IS NULL;
CREATE UNIQUE INDEX IF NOT EXISTS idx_service_windows_restaurant
    ON service_windows (restaurant_id) WHERE restaurant_id IS NOT NULL;

-- Release tracking: hide irrelevant releases from the main list (reversible), and let a
-- hard delete cascade to the release's packages and deployments so it can be fully removed.
-- (DROP+ADD makes the on-delete behavior idempotent across restarts.)
ALTER TABLE releases ADD COLUMN IF NOT EXISTS hidden BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE ota_packages DROP CONSTRAINT IF EXISTS ota_packages_release_id_fkey;
ALTER TABLE ota_packages ADD CONSTRAINT ota_packages_release_id_fkey
    FOREIGN KEY (release_id) REFERENCES releases(id) ON DELETE CASCADE;
ALTER TABLE updates DROP CONSTRAINT IF EXISTS updates_release_id_fkey;
ALTER TABLE updates ADD CONSTRAINT updates_release_id_fkey
    FOREIGN KEY (release_id) REFERENCES releases(id) ON DELETE CASCADE;

-- Retire the deployed flag entirely: a device is "deployed" iff it is assigned to a
-- restaurant (devices.restaurant_id IS NOT NULL). No more per-device / per-group /
-- per-restaurant deployed booleans.
ALTER TABLE devices     DROP COLUMN IF EXISTS deployed;
ALTER TABLE groups      DROP COLUMN IF EXISTS deployed;
ALTER TABLE restaurants DROP COLUMN IF EXISTS deployed;

-- Versions seen on devices that ops has dismissed from the releases list (not worth
-- tracking as a managed release). Keyed by the reported version string; only affects
-- not-tracked versions — a managed release uses releases.hidden instead.
CREATE TABLE IF NOT EXISTS hidden_versions (
    version    TEXT PRIMARY KEY,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Manual drag order for the releases list, keyed by version string (covers both managed
-- releases and not-tracked device versions). Versions absent here sort alphabetically
-- after the positioned ones. Lower position = higher in the list.
CREATE TABLE IF NOT EXISTS version_order (
    version  TEXT PRIMARY KEY,
    position INTEGER NOT NULL
);

-- Test team (QA): widen the user role check to allow a 'tester' account. Drop-then-add
-- keeps it idempotent across restarts (the inline constraint is auto-named users_role_check).
-- NOTE: this re-ADD re-validates every existing row on every boot, so its role list
-- must contain EVERY role that can exist in the table — including ones added by later
-- statements (e.g. 'dev' below). A narrower list here fails validation against a row a
-- later statement legitimately allows, crashing the migration. Keep this the full set.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD  CONSTRAINT users_role_check CHECK (role IN ('viewer','operator','tester','dev'));

-- Test cases: a reusable library plus per-release cases. base=true cases are checked on
-- every release; base=false cases belong to one release (release_id set). active=false
-- archives a case without deleting its history.
CREATE TABLE IF NOT EXISTS test_cases (
    id              UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    title           TEXT NOT NULL,
    area            TEXT NOT NULL DEFAULT '',
    steps           TEXT NOT NULL DEFAULT '',
    expected_result TEXT NOT NULL DEFAULT '',
    base            BOOLEAN NOT NULL DEFAULT false,
    release_id      INTEGER REFERENCES releases(id) ON DELETE CASCADE,
    active          BOOLEAN NOT NULL DEFAULT true,
    created_by      TEXT NOT NULL DEFAULT '',
    created_at      TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_test_cases_release ON test_cases(release_id);

-- Per-release test outcomes. The applicable checklist is computed (active base cases plus
-- this release's own cases); a row is written only once a tester records a status, so a
-- fresh release starts all-untested by absence.
CREATE TABLE IF NOT EXISTS release_test_results (
    release_id   INTEGER NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
    test_case_id UUID    NOT NULL REFERENCES test_cases(id) ON DELETE CASCADE,
    status       TEXT    NOT NULL DEFAULT 'untested', -- untested|pass|fail|blocked|skip
    notes        TEXT    NOT NULL DEFAULT '',
    tested_by    TEXT    NOT NULL DEFAULT '',
    tested_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (release_id, test_case_id)
);

-- When true, the release's QA checklist skips base (standard) cases and only its
-- own release-specific cases apply.
ALTER TABLE releases ADD COLUMN IF NOT EXISTS skip_base_tests BOOLEAN NOT NULL DEFAULT false;

-- 'dev' role: full operational access (releases, OTA, devices, …) but NOT settings
-- or user management; it is also the only role allowed to sign off a release.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD  CONSTRAINT users_role_check CHECK (role IN ('viewer','operator','tester','dev'));

-- Dev sign-off on a release ("smoke-tested by dev, OK for QA to pick up").
ALTER TABLE releases ADD COLUMN IF NOT EXISTS signed_off_by TEXT        NOT NULL DEFAULT '';
ALTER TABLE releases ADD COLUMN IF NOT EXISTS signed_off_at TIMESTAMPTZ;

-- Force the full OTA image for this device on its deployment, bypassing any
-- matching incremental. Set when an incremental fails (a block diff can't apply
-- to a source image that isn't byte-identical — update_engine error 29) or when
-- an operator retries a failed device, so the guaranteed-applicable full image
-- is served instead of re-attempting the same failing incremental.
ALTER TABLE update_devices ADD COLUMN IF NOT EXISTS force_full BOOLEAN NOT NULL DEFAULT false;

-- Per-channel alert-type allowlist. NULL = deliver all types (back-compat); a
-- non-empty array restricts the channel to those alert type keys.
ALTER TABLE alert_channels ADD COLUMN IF NOT EXISTS alert_types TEXT[];

-- Retired alert types: battery-health (untrusted sysfs proxy), Wi-Fi disconnect
-- counting, and the app/kiosk behavioural rules. Purge any rows seeded before they
-- were removed from the code so existing DBs match a fresh seed. Idempotent.
DELETE FROM alerts WHERE type IN (
	'battery_health_decline','battery_health_low','battery_health_critical',
	'battery_cycles_high','wifi_disconnects','app_not_foreground','kiosk_disabled',
	'app_crash','app_anr',
	'no_overnight_charge','soc_low_service','soc_low_guest_charging',
	'overnight_not_charging','overnight_slow_charge','discharge_rate_idle','discharge_rate_active',
	'pad_disconnected','pad_unused','unexpected_reboot');
DELETE FROM alert_rules WHERE type IN (
	'battery_health_decline','battery_health_low','battery_health_critical',
	'battery_cycles_high','wifi_disconnects','app_not_foreground','kiosk_disabled',
	'app_crash','app_anr',
	'no_overnight_charge','soc_low_service','soc_low_guest_charging',
	'overnight_not_charging','overnight_slow_charge','discharge_rate_idle','discharge_rate_active',
	'pad_disconnected','pad_unused','unexpected_reboot');

-- Scrub removed alert types from per-channel allowlists so the "Alert types"
-- selector count reflects the lean set (a channel that had "select all" stored the
-- full old list). array_agg over the filtered elements yields NULL when nothing is
-- left, which the dispatcher treats as "all current types" — the intended default.
UPDATE alert_channels SET alert_types = (
	SELECT array_agg(t) FROM unnest(alert_types) AS t
	WHERE t NOT IN (
		'battery_health_decline','battery_health_low','battery_health_critical',
		'battery_cycles_high','wifi_disconnects','app_not_foreground','kiosk_disabled',
		'app_crash','app_anr',
		'no_overnight_charge','soc_low_service','soc_low_guest_charging',
		'overnight_not_charging','overnight_slow_charge','discharge_rate_idle','discharge_rate_active',
		'pad_disconnected','pad_unused','unexpected_reboot')
) WHERE alert_types IS NOT NULL;

-- offline is a fleet-wide device-health alert (fires on lab/bench units too), not a
-- deployed-only operational rule. It self-suppresses overnight via its own quiet
-- window (quiet_start/quiet_end), so the service window is unnecessary. Flip legacy
-- rows off the old service-window default so they stop being gated to deployed units.
UPDATE alert_rules SET active_window = 'always' WHERE type = 'offline' AND active_window = 'service';

-- Alert occurrence tracking: count re-fires of the same open (type, device) condition
-- instead of suppressing them silently, and record when the condition was last seen.
ALTER TABLE alerts ADD COLUMN IF NOT EXISTS occurrences INTEGER NOT NULL DEFAULT 1;
ALTER TABLE alerts ADD COLUMN IF NOT EXISTS last_seen_at TIMESTAMPTZ NOT NULL DEFAULT NOW();
`

// ── OTA Packages ──────────────────────────────────────────────────────────────

func (d *DB) CreateOTAPackage(ctx context.Context, typ, targetBuildID, sourceBuildID, updateURL, changelog string, releaseDate time.Time) (*OTAPackage, error) {
	rel, err := d.GetOrCreateRelease(ctx, targetBuildID)
	if err != nil {
		return nil, err
	}
	// build_id is UNIQUE per artifact, so a full and its incrementals for the
	// same target build must differ — qualify incrementals by source build.
	buildID := targetBuildID
	if typ == "incremental" && sourceBuildID != "" {
		buildID = targetBuildID + "~from~" + sourceBuildID
	}
	var p OTAPackage
	err = d.pool.QueryRow(ctx, `
		INSERT INTO ota_packages (type, target_build_id, source_build_id, build_id, update_url, changelog, release_date, release_id)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
		RETURNING id, release_id, type, target_build_id, source_build_id, release_date, update_url, changelog, status, created_at
	`, typ, targetBuildID, sourceBuildID, buildID, updateURL, changelog, releaseDate, rel.ID).
		Scan(&p.ID, &p.ReleaseID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.Changelog, &p.Status, &p.CreatedAt)
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

// ── Releases ──────────────────────────────────────────────────────────────────

// GetOrCreateRelease returns the release for a version (target build id),
// creating it in 'draft' if absent.
func (d *DB) GetOrCreateRelease(ctx context.Context, version string) (*Release, error) {
	if _, err := d.pool.Exec(ctx,
		`INSERT INTO releases (version) VALUES ($1) ON CONFLICT (version) DO NOTHING`, version); err != nil {
		return nil, err
	}
	return d.GetReleaseByVersion(ctx, version)
}

func (d *DB) GetReleaseByVersion(ctx context.Context, version string) (*Release, error) {
	var r Release
	err := d.pool.QueryRow(ctx, `
		SELECT id, version, name, changelog, status, created_at, published_at
		FROM releases WHERE version = $1
	`, version).Scan(&r.ID, &r.Version, &r.Name, &r.Changelog, &r.Status, &r.CreatedAt, &r.PublishedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (d *DB) GetRelease(ctx context.Context, id int) (*Release, error) {
	var r Release
	err := d.pool.QueryRow(ctx, `
		SELECT id, version, name, changelog, status, skip_base_tests, created_at, published_at
		FROM releases WHERE id = $1
	`, id).Scan(&r.ID, &r.Version, &r.Name, &r.Changelog, &r.Status, &r.SkipBaseTests, &r.CreatedAt, &r.PublishedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (d *DB) ListReleases(ctx context.Context) ([]Release, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT r.id, r.version, r.name, r.changelog, r.status, r.hidden, r.created_at, r.published_at,
		       r.signed_off_by, r.signed_off_at,
		       COUNT(DISTINCT p.id) AS package_count,
		       COUNT(DISTINCT u.id) AS deploy_count
		FROM releases r
		LEFT JOIN ota_packages p ON p.release_id = r.id
		LEFT JOIN updates u ON u.release_id = r.id
		GROUP BY r.id
		ORDER BY r.created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		var r Release
		if err := rows.Scan(&r.ID, &r.Version, &r.Name, &r.Changelog, &r.Status, &r.Hidden, &r.CreatedAt, &r.PublishedAt, &r.SignedOffBy, &r.SignedOffAt, &r.PackageCount, &r.DeployCount); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetReleaseHidden hides/unhides a release from the main list (irrelevant releases).
func (d *DB) SetReleaseHidden(ctx context.Context, id int, hidden bool) error {
	_, err := d.pool.Exec(ctx, `UPDATE releases SET hidden = $2 WHERE id = $1`, id, hidden)
	return err
}

// DeleteRelease hard-deletes a release. The release_id FKs cascade, so its packages and
// deployments (and their per-device rows) go with it.
func (d *DB) DeleteRelease(ctx context.Context, id int) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM releases WHERE id = $1`, id)
	return err
}

// GetFleetVersions returns every release version actually reported by non-hidden devices,
// with the devices on each and a link to the managed release (if one exists). This is the
// "what's really running in the field" view for release tracking.
func (d *DB) GetFleetVersions(ctx context.Context) ([]FleetVersion, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.build_id, COUNT(*)::int,
		       array_agg(d.serial_number ORDER BY d.serial_number),
		       rel.id, COALESCE(rel.status, ''), COALESCE(rel.hidden, false)
		FROM devices d
		LEFT JOIN releases rel ON rel.version = d.build_id
		WHERE NOT d.hidden AND d.build_id <> ''
		GROUP BY d.build_id, rel.id, rel.status, rel.hidden
		ORDER BY COUNT(*) DESC, d.build_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FleetVersion
	for rows.Next() {
		var v FleetVersion
		if err := rows.Scan(&v.Version, &v.DeviceCount, &v.Serials, &v.ReleaseID, &v.ReleaseStatus, &v.ReleaseHidden); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// HideVersion dismisses a device-reported version from the releases list (for versions
// that are not managed releases — managed ones use releases.hidden).
func (d *DB) HideVersion(ctx context.Context, version string) error {
	_, err := d.pool.Exec(ctx, `INSERT INTO hidden_versions (version) VALUES ($1) ON CONFLICT DO NOTHING`, version)
	return err
}

// UnhideVersion brings a dismissed version back into the list.
func (d *DB) UnhideVersion(ctx context.Context, version string) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM hidden_versions WHERE version = $1`, version)
	return err
}

// ListHiddenVersions returns the set of dismissed (not-tracked) versions.
func (d *DB) ListHiddenVersions(ctx context.Context) (map[string]bool, error) {
	rows, err := d.pool.Query(ctx, `SELECT version FROM hidden_versions`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]bool)
	for rows.Next() {
		var v string
		if err := rows.Scan(&v); err != nil {
			return nil, err
		}
		out[v] = true
	}
	return out, rows.Err()
}

// GetVersionOrder returns the saved manual order (version -> position) for the releases list.
func (d *DB) GetVersionOrder(ctx context.Context) (map[string]int, error) {
	rows, err := d.pool.Query(ctx, `SELECT version, position FROM version_order`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]int)
	for rows.Next() {
		var v string
		var p int
		if err := rows.Scan(&v, &p); err != nil {
			return nil, err
		}
		out[v] = p
	}
	return out, rows.Err()
}

// SetVersionOrder replaces the manual order with the given versions (position = index).
func (d *DB) SetVersionOrder(ctx context.Context, versions []string) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM version_order`); err != nil {
		return err
	}
	for i, v := range versions {
		if v == "" {
			continue
		}
		if _, err := tx.Exec(ctx,
			`INSERT INTO version_order (version, position) VALUES ($1, $2)
			 ON CONFLICT (version) DO UPDATE SET position = EXCLUDED.position`, v, i); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// AdoptionPoint is one day of "how many devices were on this version".
type AdoptionPoint struct {
	Day         time.Time `json:"day"`
	DeviceCount int       `json:"device_count"`
}

// GetReleaseAdoption returns the per-day count of devices reporting a version, from the
// daily rollup (device_daily_stats.build_id = last build seen that day), oldest first.
func (d *DB) GetReleaseAdoption(ctx context.Context, version string, days int) ([]AdoptionPoint, error) {
	if days <= 0 {
		days = 30
	}
	rows, err := d.pool.Query(ctx, `
		SELECT day, COUNT(DISTINCT device_id)::int
		FROM device_daily_stats
		WHERE build_id = $1 AND day >= CURRENT_DATE - ($2::int - 1)
		GROUP BY day ORDER BY day
	`, version, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []AdoptionPoint
	for rows.Next() {
		var p AdoptionPoint
		if err := rows.Scan(&p.Day, &p.DeviceCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CountDevicesByVersion returns how many non-hidden devices currently report a version.
func (d *DB) CountDevicesByVersion(ctx context.Context, version string) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM devices WHERE NOT hidden AND build_id = $1`, version).Scan(&n)
	return n, err
}

// ListDevicesByVersion returns the non-hidden devices currently reporting a build version,
// for the release-adoption panel.
func (d *DB) ListDevicesByVersion(ctx context.Context, version string) ([]Device, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.serial_number, d.latest_battery_pct, d.last_seen_at, COALESCE(rest.name, '')
		FROM devices d
		LEFT JOIN restaurants rest ON rest.id = d.restaurant_id
		WHERE NOT d.hidden AND d.build_id = $1
		ORDER BY d.serial_number
	`, version)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var dev Device
		if err := rows.Scan(&dev.SerialNumber, &dev.BatteryPct, &dev.LastSeenAt, &dev.RestaurantName); err != nil {
			return nil, err
		}
		out = append(out, dev)
	}
	return out, rows.Err()
}

// SetReleaseStatus moves a release through its lifecycle; publishing stamps
// published_at the first time.
func (d *DB) SetReleaseStatus(ctx context.Context, id int, status string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE releases
		SET status = $2,
		    published_at = CASE WHEN $2 = 'published' AND published_at IS NULL THEN NOW() ELSE published_at END
		WHERE id = $1
	`, id, status)
	return err
}

// SetReleaseSignOff records a dev's smoke-test sign-off on a release (the
// "tested at our end, OK for QA" gate). signer is the dev's username.
func (d *DB) SetReleaseSignOff(ctx context.Context, id int, signer string) error {
	_, err := d.pool.Exec(ctx, `UPDATE releases SET signed_off_by = $2, signed_off_at = NOW() WHERE id = $1`, id, signer)
	return err
}

// ClearReleaseSignOff revokes a previously recorded dev sign-off.
func (d *DB) ClearReleaseSignOff(ctx context.Context, id int) error {
	_, err := d.pool.Exec(ctx, `UPDATE releases SET signed_off_by = '', signed_off_at = NULL WHERE id = $1`, id)
	return err
}

// SetReleaseMeta updates the editable release fields (name, changelog).
func (d *DB) SetReleaseMeta(ctx context.Context, id int, name, changelog string) error {
	_, err := d.pool.Exec(ctx, `UPDATE releases SET name = $2, changelog = $3 WHERE id = $1`, id, name, changelog)
	return err
}

// SetReleaseSkipBaseTests sets whether a release's QA skips base (standard) cases,
// testing only its release-specific cases.
func (d *DB) SetReleaseSkipBaseTests(ctx context.Context, id int, skip bool) error {
	_, err := d.pool.Exec(ctx, `UPDATE releases SET skip_base_tests = $2 WHERE id = $1`, id, skip)
	return err
}

// ListPackagesByRelease returns the packages (full + incrementals) of a release.
func (d *DB) ListPackagesByRelease(ctx context.Context, releaseID int) ([]OTAPackage, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, release_id, type, target_build_id, source_build_id, release_date, update_url, changelog, status, created_at
		FROM ota_packages WHERE release_id = $1
		ORDER BY (type = 'full') DESC, source_build_id, created_at DESC
	`, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OTAPackage
	for rows.Next() {
		var p OTAPackage
		if err := rows.Scan(&p.ID, &p.ReleaseID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.Changelog, &p.Status, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ── Updates (Deployments) ─────────────────────────────────────────────────────

func (d *DB) CreateUpdate(ctx context.Context, otaPackageID int, rebootBehavior string, scheduledTime *time.Time) (*Update, error) {
	var u Update
	err := d.pool.QueryRow(ctx, `
		INSERT INTO updates (ota_package_id, release_id, reboot_behavior, scheduled_time, status)
		SELECT $1, p.release_id, $2, $3, 'pending' FROM ota_packages p WHERE p.id = $1
		RETURNING id, COALESCE(ota_package_id, 0), release_id, reboot_behavior, scheduled_time, status, created_at
	`, otaPackageID, rebootBehavior, scheduledTime).
		Scan(&u.ID, &u.OtaPackageID, &u.ReleaseID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt)
	return &u, err
}

// CreateReleaseUpdate creates a deployment for a whole release. ota_package_id is
// set to the release's full package when present (representative / back-compat);
// the per-device artifact is chosen at resolve time by ResolveUpdateForDevice.
func (d *DB) CreateReleaseUpdate(ctx context.Context, releaseID int, rebootBehavior string, scheduledTime *time.Time) (*Update, error) {
	var u Update
	err := d.pool.QueryRow(ctx, `
		INSERT INTO updates (release_id, ota_package_id, reboot_behavior, scheduled_time, status)
		VALUES ($1,
		        (SELECT id FROM ota_packages WHERE release_id = $1 AND type = 'full' ORDER BY created_at DESC LIMIT 1),
		        $2, $3, 'pending')
		RETURNING id, COALESCE(ota_package_id, 0), release_id, reboot_behavior, scheduled_time, status, created_at
	`, releaseID, rebootBehavior, scheduledTime).
		Scan(&u.ID, &u.OtaPackageID, &u.ReleaseID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt)
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

// ListDeploymentsByRelease returns deployments for a release with device counts.
func (d *DB) ListDeploymentsByRelease(ctx context.Context, releaseID int) ([]Update, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT u.id, COALESCE(u.ota_package_id, 0), u.release_id, u.reboot_behavior, u.scheduled_time, u.status, u.created_at,
		       COUNT(ud.device_id) AS device_total,
		       COUNT(CASE WHEN ud.status = 'installed' THEN 1 END) AS device_installed
		FROM updates u
		LEFT JOIN update_devices ud ON ud.update_id = u.id
		WHERE u.release_id = $1
		GROUP BY u.id
		ORDER BY u.created_at DESC
	`, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Update
	for rows.Next() {
		var u Update
		if err := rows.Scan(&u.ID, &u.OtaPackageID, &u.ReleaseID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt,
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
	var rel Release
	err := d.pool.QueryRow(ctx, `
		SELECT u.id, COALESCE(u.ota_package_id, 0), u.release_id, u.reboot_behavior, u.scheduled_time, u.status, u.created_at,
		       rel.id, rel.version, rel.name, rel.changelog, rel.status, rel.created_at, rel.published_at
		FROM updates u
		JOIN releases rel ON rel.id = u.release_id
		WHERE u.id = $1
	`, id).Scan(&u.ID, &u.OtaPackageID, &u.ReleaseID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt,
		&rel.ID, &rel.Version, &rel.Name, &rel.Changelog, &rel.Status, &rel.CreatedAt, &rel.PublishedAt)
	if err != nil {
		return nil, err
	}
	u.Release = &rel
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
	// Activate the deployment. Reactivate a 'complete' one too: adding targets to
	// a finished deployment must re-arm it, or the new pending rows are stranded
	// (ResolveUpdateForDevice only serves status='active').
	_, err := d.pool.Exec(ctx, `UPDATE updates SET status = 'active' WHERE id = $1 AND status IN ('pending', 'complete')`, updateID)
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
	// Resolve via the deployment's release and pick the per-device artifact: the
	// incremental whose source build matches the device's current build if one
	// exists, otherwise the full image. Only published releases and active
	// packages are eligible.
	err := d.pool.QueryRow(ctx, `
		SELECT u.id, COALESCE(u.ota_package_id, 0), u.release_id, u.reboot_behavior, u.scheduled_time, u.status, u.created_at, ud.status,
		       p.id, p.release_id, p.type, p.target_build_id, p.source_build_id, p.release_date, p.update_url, p.changelog, p.status, p.created_at
		FROM update_devices ud
		JOIN updates u ON u.id = ud.update_id
		JOIN releases rel ON rel.id = u.release_id
		JOIN devices d ON d.id = ud.device_id
		JOIN LATERAL (
			SELECT pk.* FROM ota_packages pk
			WHERE pk.release_id = u.release_id AND pk.status = 'active'
			  AND (pk.type = 'full'
			       OR (pk.type = 'incremental' AND NOT ud.force_full AND pk.source_build_id = d.build_id))
			ORDER BY (pk.type = 'incremental' AND NOT ud.force_full AND pk.source_build_id = d.build_id) DESC, pk.created_at DESC
			LIMIT 1
		) p ON true
		WHERE ud.device_id = $1 AND u.status = 'active' AND ud.status != 'installed' AND rel.status = 'published'
		ORDER BY u.created_at DESC
		LIMIT 1
	`, deviceID).Scan(&u.ID, &u.OtaPackageID, &u.ReleaseID, &u.RebootBehavior, &u.ScheduledTime, &u.Status, &u.CreatedAt, &u.DeviceStatus,
		&p.ID, &p.ReleaseID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.Changelog, &p.Status, &p.CreatedAt)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	u.OtaPackage = &p
	return &u, nil
}

// DueReboot is a device whose scheduled-reboot deployment time has arrived.
type DueReboot struct {
	UpdateID int
	DeviceID uuid.UUID
}

// ListDueScheduledReboots returns devices that have installed an update under a
// "scheduled" reboot policy and whose scheduled time has passed.
func (d *DB) ListDueScheduledReboots(ctx context.Context) ([]DueReboot, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT ud.update_id, ud.device_id
		FROM update_devices ud
		JOIN updates u ON u.id = ud.update_id
		WHERE u.reboot_behavior = 'scheduled'
		  AND u.scheduled_time IS NOT NULL
		  AND u.scheduled_time <= NOW()
		  AND u.status = 'active'
		  AND ud.status = 'awaiting_reboot'
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DueReboot
	for rows.Next() {
		var dr DueReboot
		if err := rows.Scan(&dr.UpdateID, &dr.DeviceID); err != nil {
			return nil, err
		}
		out = append(out, dr)
	}
	return out, rows.Err()
}

// SetUpdateDeviceStatus updates the status of a device within an update. It
// stamps updated_at and clears any prior error_code, so a device that moves on
// from a failure (e.g. on retry) doesn't keep showing a stale reason.
func (d *DB) SetUpdateDeviceStatus(ctx context.Context, updateID int, deviceID uuid.UUID, status string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE update_devices SET status = $3, error_code = '', updated_at = NOW(),
			started_at   = CASE WHEN $3 = 'downloading' AND started_at   IS NULL THEN NOW() ELSE started_at   END,
			completed_at = CASE WHEN $3 = 'installed'   AND completed_at IS NULL THEN NOW() ELSE completed_at END
		WHERE update_id = $1 AND device_id = $2
	`, updateID, deviceID, status)
	return err
}

// RemoveDeviceFromUpdate drops a device's row from a deployment, but only while it
// is still 'pending' — i.e. nothing has been sent to the device yet. Returns true if
// a row was actually removed (false when the device already moved past pending or was
// not a target). Once a device has started downloading it can no longer be removed.
func (d *DB) RemoveDeviceFromUpdate(ctx context.Context, updateID int, deviceID uuid.UUID) (bool, error) {
	tag, err := d.pool.Exec(ctx, `
		DELETE FROM update_devices
		WHERE update_id = $1 AND device_id = $2 AND status = 'pending'
	`, updateID, deviceID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
}

// CompleteUpdatesAtTargetBuild marks installed every active, not-yet-terminal
// deployment row for this device whose release ships a package targeting the
// device's current build. This is the authoritative "the device is now running
// the new build" reconciliation, and it deliberately does NOT go through
// ResolveUpdateForDevice: that resolver only returns a row while an active
// package still matches the device's *current* build (source_build_id for an
// incremental), so once a device updates off the source build onto the target
// it resolves to nil — leaving the row stuck at reboot_sent and, worse, letting
// the OTA be re-sent. Matching on target_build_id closes both gaps. Returns the
// affected update IDs so the caller can complete the deployment and refresh it.
func (d *DB) CompleteUpdatesAtTargetBuild(ctx context.Context, deviceID uuid.UUID, buildID string) ([]int, error) {
	if buildID == "" {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, `
		UPDATE update_devices ud
		SET status = 'installed', error_code = '', updated_at = NOW(),
		    completed_at = COALESCE(ud.completed_at, NOW())
		FROM updates u
		WHERE ud.update_id = u.id
		  AND ud.device_id = $1
		  AND u.status = 'active'
		  AND ud.status NOT IN ('installed', 'canceled')
		  AND EXISTS (
		      SELECT 1 FROM ota_packages pk
		      WHERE pk.release_id = u.release_id
		        AND pk.status = 'active'
		        AND pk.target_build_id = $2
		  )
		RETURNING ud.update_id
	`, deviceID, buildID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var ids []int
	for rows.Next() {
		var id int
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// SetUpdateDeviceFailed marks a device's deployment row failed and records the
// error_code it reported, so the deployment view can show why it failed.
func (d *DB) SetUpdateDeviceFailed(ctx context.Context, updateID int, deviceID uuid.UUID, errorCode string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE update_devices SET status = 'failed', error_code = $3, updated_at = NOW()
		WHERE update_id = $1 AND device_id = $2
	`, updateID, deviceID, errorCode)
	return err
}

// SetUpdateDeviceForceFull pins a device's deployment row to the full OTA image,
// so ResolveUpdateForDevice stops offering a matching incremental. Used after an
// incremental fails on the device, and on operator retry.
func (d *DB) SetUpdateDeviceForceFull(ctx context.Context, updateID int, deviceID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE update_devices SET force_full = true, updated_at = NOW()
		WHERE update_id = $1 AND device_id = $2
	`, updateID, deviceID)
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

// ReactivateUpdate flips a previously-completed update back to 'active'. Needed
// when a device is retried or newly added under a deployment that
// CheckAndCompleteUpdate already marked complete — otherwise
// ResolveUpdateForDevice (which requires status='active') would never serve the
// re-pending device and it would sit at "pending" forever.
func (d *DB) ReactivateUpdate(ctx context.Context, updateID int) error {
	_, err := d.pool.Exec(ctx, `UPDATE updates SET status = 'active' WHERE id = $1 AND status = 'complete'`, updateID)
	return err
}

// ReconcileStrandedUpdates reactivates any deployment that was marked 'complete'
// while it still has a device in a non-terminal state (e.g. one re-pended by a
// retry). Such devices are never served by ResolveUpdateForDevice (status='active'
// only) and sit at "pending" forever. Best-effort: called once at startup, NOT
// part of RunMigrations — a data reconciliation must never crash the server. It
// is idempotent (once the device installs, CheckAndCompleteUpdate completes it
// again) and excludes failed/canceled rows so they are not silently re-pushed.
func (d *DB) ReconcileStrandedUpdates(ctx context.Context) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE updates u SET status = 'active'
		WHERE u.status = 'complete'
		  AND EXISTS (
		    SELECT 1 FROM update_devices ud
		    WHERE ud.update_id = u.id
		      AND ud.status NOT IN ('installed', 'canceled', 'failed')
		  )
	`)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// CancelDeployment stops an active deployment: devices that haven't started yet
// (pending) are marked canceled, and the update leaves 'active' so
// ResolveUpdateForDevice no longer hands it out on check-in. Devices already
// downloading/installed are left alone — their work continues on-device.
func (d *DB) CancelDeployment(ctx context.Context, updateID int) error {
	if _, err := d.pool.Exec(ctx, `
		UPDATE update_devices SET status = 'canceled', error_code = '', updated_at = NOW()
		WHERE update_id = $1 AND status = 'pending'
	`, updateID); err != nil {
		return err
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE updates SET status = 'canceled' WHERE id = $1 AND status = 'active'
	`, updateID)
	return err
}

// GetUpdateTargets returns the device targets for an update.
func (d *DB) GetUpdateTargets(ctx context.Context, updateID int) ([]UpdateTarget, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT ud.update_id, ud.device_id, d.serial_number, d.build_id, ud.status, ud.error_code, ud.updated_at,
		       ud.started_at, ud.completed_at
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
		if err := rows.Scan(&t.UpdateID, &t.DeviceID, &t.SerialNumber, &t.BuildID, &t.Status, &t.ErrorCode, &t.UpdatedAt, &t.StartedAt, &t.CompletedAt); err != nil {
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

// ── Dashboard sessions ──────────────────────────────────────────────────────

// Session is a server-side dashboard session. UserID is nil for the static
// env-configured admin (which has no users row).
type Session struct {
	ID        string
	UserID    *uuid.UUID
	Username  string
	Role      string
	CreatedAt time.Time
	LastSeen  time.Time
	ExpiresAt time.Time
}

// CreateSession persists a new session row.
func (d *DB) CreateSession(ctx context.Context, s Session) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO sessions (id, user_id, username, role, created_at, last_seen, expires_at)
		VALUES ($1, $2, $3, $4, NOW(), NOW(), $5)
	`, s.ID, s.UserID, s.Username, s.Role, s.ExpiresAt)
	return err
}

// GetSession returns the session by id, or an error if it does not exist.
func (d *DB) GetSession(ctx context.Context, id string) (*Session, error) {
	var s Session
	err := d.pool.QueryRow(ctx, `
		SELECT id, user_id, username, role, created_at, last_seen, expires_at
		FROM sessions WHERE id = $1
	`, id).Scan(&s.ID, &s.UserID, &s.Username, &s.Role, &s.CreatedAt, &s.LastSeen, &s.ExpiresAt)
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// TouchSession slides a session's last_seen forward to keep an active user
// logged in (the absolute expires_at is left unchanged).
func (d *DB) TouchSession(ctx context.Context, id string) error {
	_, err := d.pool.Exec(ctx, `UPDATE sessions SET last_seen = NOW() WHERE id = $1`, id)
	return err
}

// DeleteSession removes a single session (logout).
func (d *DB) DeleteSession(ctx context.Context, id string) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM sessions WHERE id = $1`, id)
	return err
}

// DeleteAllSessions removes every session (admin "log out all").
func (d *DB) DeleteAllSessions(ctx context.Context) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM sessions`)
	return err
}

// DeleteExpiredSessions prunes sessions past their absolute expiry; called by
// housekeeping. Idle-timeout enforcement happens at read time.
func (d *DB) DeleteExpiredSessions(ctx context.Context) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM sessions WHERE expires_at < NOW()`)
	return err
}

// FleetDailyStat is one day of stats aggregated across the whole (non-hidden)
// fleet, feeding the overview page sparklines. Aggregated from device_daily_stats,
// so "today" reflects the last rollup, not the live minute.
type FleetDailyStat struct {
	Day        time.Time `json:"day"`
	Active     int       `json:"active"`      // devices with at least one checkin that day
	LowBattery int       `json:"low_battery"` // devices whose daily minimum dipped under 20%
	Hot        int       `json:"hot"`         // devices whose daily max temp reached 45°C
	BatteryAvg *float32  `json:"battery_avg"` // mean of per-device daily averages
}

// GetFleetDailyStats returns the last `days` days of fleet-wide rollups, oldest
// first. Days with no checkins at all simply have no row.
func (d *DB) GetFleetDailyStats(ctx context.Context, days int) ([]FleetDailyStat, error) {
	if days <= 0 {
		days = 7
	}
	rows, err := d.pool.Query(ctx, `
		SELECT s.day,
		       COUNT(*) FILTER (WHERE s.checkin_count > 0),
		       COUNT(*) FILTER (WHERE s.battery_min < 20),
		       COUNT(*) FILTER (WHERE s.temp_max >= 45),
		       AVG(s.battery_avg)::real
		FROM device_daily_stats s
		JOIN devices dv ON dv.id = s.device_id AND NOT dv.hidden
		WHERE s.day >= CURRENT_DATE - ($1::int - 1)
		GROUP BY s.day
		ORDER BY s.day`, days)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var stats []FleetDailyStat
	for rows.Next() {
		var s FleetDailyStat
		if err := rows.Scan(&s.Day, &s.Active, &s.LowBattery, &s.Hot, &s.BatteryAvg); err != nil {
			return nil, err
		}
		stats = append(stats, s)
	}
	return stats, rows.Err()
}

// CountHotDevices returns how many non-hidden devices are currently reporting a
// device temperature at or above 45°C (the dashboard's warn threshold).
func (d *DB) CountHotDevices(ctx context.Context) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM devices
		WHERE NOT hidden AND COALESCE((latest_extra->>'battery_temp_c')::numeric, 0) >= 45`).Scan(&n)
	return n, err
}

// ── Test team / QA ──────────────────────────────────────────────────────────────

// TestCase is a single thing to verify. Base cases apply to every release; cases with a
// ReleaseID apply only to that release.
type TestCase struct {
	ID        uuid.UUID `json:"id"`
	Title     string    `json:"title"`
	Area      string    `json:"area"`
	Steps     string    `json:"steps"`
	Expected  string    `json:"expected_result"`
	Base      bool      `json:"base"`
	ReleaseID *int      `json:"release_id"`
	Active    bool      `json:"active"`
	CreatedBy string    `json:"created_by"`
	CreatedAt time.Time `json:"created_at"`
}

// ChecklistItem is one test case joined with its result for a given release.
type ChecklistItem struct {
	TestCase
	Status   string     `json:"status"` // untested|pass|fail|blocked|skip
	Notes    string     `json:"notes"`
	TestedBy string     `json:"tested_by"`
	TestedAt *time.Time `json:"tested_at"`
}

// QASummary holds the per-release status counts.
type QASummary struct {
	Total, Pass, Fail, Blocked, Skip, Untested int
}

// Passed reports whether every applicable case passed (and there is at least one).
func (q QASummary) Passed() bool { return q.Total > 0 && q.Pass == q.Total }

// CreateTestCase inserts a base (releaseID nil) or release-specific test case.
func (d *DB) CreateTestCase(ctx context.Context, tc TestCase) (uuid.UUID, error) {
	var id uuid.UUID
	err := d.pool.QueryRow(ctx, `
		INSERT INTO test_cases (title, area, steps, expected_result, base, release_id, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		tc.Title, tc.Area, tc.Steps, tc.Expected, tc.Base, tc.ReleaseID, tc.CreatedBy).Scan(&id)
	return id, err
}

// UpdateTestCase edits an existing case's content and active flag.
func (d *DB) UpdateTestCase(ctx context.Context, id uuid.UUID, title, area, steps, expected string, active bool) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE test_cases SET title=$2, area=$3, steps=$4, expected_result=$5, active=$6 WHERE id=$1`,
		id, title, area, steps, expected, active)
	return err
}

// DeleteTestCase removes a case (and its results, via FK cascade).
func (d *DB) DeleteTestCase(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM test_cases WHERE id=$1`, id)
	return err
}

func scanTestCases(rows pgx.Rows) ([]TestCase, error) {
	defer rows.Close()
	var out []TestCase
	for rows.Next() {
		var t TestCase
		if err := rows.Scan(&t.ID, &t.Title, &t.Area, &t.Steps, &t.Expected, &t.Base, &t.ReleaseID, &t.Active, &t.CreatedBy, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// ListBaseTestCases returns the standard cases (optionally only active ones).
func (d *DB) ListBaseTestCases(ctx context.Context, onlyActive bool) ([]TestCase, error) {
	q := `SELECT id, title, area, steps, expected_result, base, release_id, active, created_by, created_at
	      FROM test_cases WHERE base = true`
	if onlyActive {
		q += ` AND active = true`
	}
	q += ` ORDER BY area, title`
	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	return scanTestCases(rows)
}

// ListReleaseTestCases returns the cases specific to one release.
func (d *DB) ListReleaseTestCases(ctx context.Context, releaseID int) ([]TestCase, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, title, area, steps, expected_result, base, release_id, active, created_by, created_at
		FROM test_cases WHERE release_id = $1 ORDER BY area, title`, releaseID)
	if err != nil {
		return nil, err
	}
	return scanTestCases(rows)
}

// GetReleaseChecklist returns the applicable cases for a release (active base cases plus
// this release's own cases) joined with any recorded result.
func (d *DB) GetReleaseChecklist(ctx context.Context, releaseID int) ([]ChecklistItem, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT tc.id, tc.title, tc.area, tc.steps, tc.expected_result, tc.base, tc.release_id,
		       tc.active, tc.created_by, tc.created_at,
		       COALESCE(r.status, 'untested'), COALESCE(r.notes, ''), COALESCE(r.tested_by, ''), r.tested_at
		FROM test_cases tc
		LEFT JOIN release_test_results r ON r.test_case_id = tc.id AND r.release_id = $1
		WHERE tc.active AND (tc.release_id = $1
		      OR (tc.base AND NOT COALESCE((SELECT skip_base_tests FROM releases WHERE id = $1), false)))
		ORDER BY tc.base DESC, tc.area, tc.title`, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChecklistItem
	for rows.Next() {
		var c ChecklistItem
		if err := rows.Scan(&c.ID, &c.Title, &c.Area, &c.Steps, &c.Expected, &c.Base, &c.ReleaseID,
			&c.Active, &c.CreatedBy, &c.CreatedAt,
			&c.Status, &c.Notes, &c.TestedBy, &c.TestedAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetTestResult records (upserts) a tester's outcome for one case on one release.
func (d *DB) SetTestResult(ctx context.Context, releaseID int, testCaseID uuid.UUID, status, notes, testedBy string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO release_test_results (release_id, test_case_id, status, notes, tested_by, tested_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (release_id, test_case_id)
		DO UPDATE SET status = $3, notes = $4, tested_by = $5, tested_at = NOW()`,
		releaseID, testCaseID, status, notes, testedBy)
	return err
}

// ReleaseQASummary computes the status counts for a release's checklist.
func (d *DB) ReleaseQASummary(ctx context.Context, releaseID int) (QASummary, error) {
	var s QASummary
	err := d.pool.QueryRow(ctx, `
		SELECT
			COUNT(*),
			COUNT(*) FILTER (WHERE status='pass'),
			COUNT(*) FILTER (WHERE status='fail'),
			COUNT(*) FILTER (WHERE status='blocked'),
			COUNT(*) FILTER (WHERE status='skip'),
			COUNT(*) FILTER (WHERE status='untested')
		FROM (
			SELECT COALESCE(r.status, 'untested') AS status
			FROM test_cases tc
			LEFT JOIN release_test_results r ON r.test_case_id = tc.id AND r.release_id = $1
			WHERE tc.active AND (tc.release_id = $1
			      OR (tc.base AND NOT COALESCE((SELECT skip_base_tests FROM releases WHERE id = $1), false)))
		) c`, releaseID).Scan(&s.Total, &s.Pass, &s.Fail, &s.Blocked, &s.Skip, &s.Untested)
	return s, err
}
