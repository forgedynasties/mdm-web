package db

import (
	"context"
	"crypto/rand"
	"encoding/base32"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	prod "mdm/internal/product"
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
	// Product is the hardware category the device reports (e.g. "t7", "kiosk27"), or
	// "" if it never reported one. Use ProductLabel()/Caps() rather than reading this
	// raw so empty/unknown resolves to the default product. See internal/product.
	Product string `json:"product"`
	// DischargeTotalPct is the lifetime cumulative percent of battery capacity
	// discharged (never resets). BatteryCycles() renders it as equivalent full cycles.
	DischargeTotalPct int64 `json:"discharge_total_pct"`
	// DischargeLegacyPct is the prior range-based estimate, kept during the transition
	// to the corrected discharge accounting so the two can be compared. Zero once the
	// difference no longer matters. Rendered by BatteryCyclesLegacy().
	DischargeLegacyPct int64 `json:"discharge_legacy_pct"`
	// DischargeBackfilled is false until the one-time history seed has run for this
	// device. While false the cycle count is not yet meaningful (show "—", not 0.0).
	DischargeBackfilled bool `json:"discharge_backfilled"`
	// RestaurantID is the venue the device physically lives in (nil = lab/bench unit).
	// RestaurantName is joined for display. A device is "deployed" iff it has a restaurant.
	RestaurantID   *uuid.UUID `json:"restaurant_id,omitempty"`
	RestaurantName string     `json:"restaurant_name,omitempty"`
	// DeployedEffective is true when the device is live in a restaurant (RestaurantID set);
	// false = lab/bench unit. There is no separate deployed flag — assignment is the signal.
	DeployedEffective bool `json:"deployed_effective"`
}

// ProductLabel is the human display label for the device's product ("T7", "Kiosk 27"),
// resolving empty/unknown to the default product. Safe to call from templates.
func (d Device) ProductLabel() string { return prod.Label(d.Product) }

// ProductKey is the canonical product key ("t7", "kiosk27"), default-substituted so
// legacy devices with an empty product still report as the default. Use for grouping.
func (d Device) ProductKey() string {
	p, _ := prod.Resolve(d.Product)
	return p.Key
}

// Caps returns the hardware capabilities of the device's product. Gate wlc/charging
// UI and telemetry on these instead of checking whether a telemetry key is present.
func (d Device) Caps() prod.Caps { return prod.CapsFor(d.Product) }

// HasWLC / HasCharging / HasBattery are template-friendly capability shortcuts.
func (d Device) HasWLC() bool      { return d.Caps().HasWLC }
func (d Device) HasCharging() bool { return d.Caps().HasCharging }
func (d Device) HasBattery() bool  { return d.Caps().HasBattery }

// BatteryCycles converts the lifetime cumulative discharge into equivalent full
// battery cycles (1 cycle = 100% of capacity discharged). 250% total => 2.5 cycles.
func (d Device) BatteryCycles() float64 {
	return float64(d.DischargeTotalPct) / 100
}

// BatteryCyclesLegacy renders the prior range-based estimate as cycles, for
// side-by-side comparison during the transition to corrected discharge accounting.
func (d Device) BatteryCyclesLegacy() float64 {
	return float64(d.DischargeLegacyPct) / 100
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
	// WlcChargingEnabled controls wireless-charging on the pad (client writes the
	// customer_gpio line). Default true.
	WlcChargingEnabled bool `json:"wlc_charging_enabled"`
	// Offline kiosk-exit: a per-device TOTP seed (base32) lets a technician leave
	// kiosk mode on-device with no server access. Seed is generated on enable.
	OfflineExitEnabled bool   `json:"offline_exit_enabled"`
	OfflineExitSeed    string `json:"offline_exit_seed"`
	OfflineExitRelock  string `json:"offline_exit_relock"`
	UpdatedAt          time.Time `json:"updated_at"`
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

// QFILPackage is a Qualcomm Flash Image Loader bundle attached to a release — the
// artifact the test team uses to flash a device from scratch over USB. It carries
// only a URL to the externally hosted bundle plus a label/notes; it is never
// deployed OTA, so it has no build/deployment plumbing of its own.
type QFILPackage struct {
	ID        int       `json:"id"`
	ReleaseID int       `json:"release_id"`
	Label     string    `json:"label"`
	URL       string    `json:"url"`
	Notes     string    `json:"notes"`
	AddedBy   string    `json:"added_by"`
	Status    string    `json:"status"` // "active" or "removed"
	CreatedAt time.Time `json:"created_at"`
}

// Release groups the packages for one target build version under a single
// lifecycle. Version equals the target build id.
type Release struct {
	ID                  int        `json:"id"`
	Version             string     `json:"version"`
	Product             string     `json:"product"` // hardware product this release targets (t7/kiosk18/22/27)
	Name                string     `json:"name"`
	Changelog           string     `json:"changelog"`
	Status              string     `json:"status"`          // "draft" | "published"
	Hidden              bool       `json:"hidden"`          // hidden from the main releases list (irrelevant)
	SkipBaseTests       bool       `json:"skip_base_tests"` // QA tests only release-specific cases, not base
	CreatedAt           time.Time  `json:"created_at"`
	PublishedAt         *time.Time `json:"published_at"`
	SignedOffBy         string     `json:"signed_off_by"` // dev who smoke-tested; "" when not signed off
	SignedOffAt         *time.Time `json:"signed_off_at"`
	TestingDoneAt       *time.Time `json:"testing_done_at"` // finish line: nil = active (under test), set = inactive
	TestingDoneBy       string     `json:"testing_done_by"`
	ParentReleaseID     *int       `json:"parent_release_id"`       // set for branch builds — the release this forked from
	IsBranch            bool       `json:"is_branch"`               // off-mainline temporary test build
	PackageCount        int        `json:"package_count,omitempty"` // populated by ListReleases
	DeployCount         int        `json:"deploy_count,omitempty"`  // populated by ListReleases
	MergedFromReleaseID *int       `json:"merged_from_release_id"`  // mainline node: the branch it absorbed on merge
	MergedIntoReleaseID *int       `json:"merged_into_release_id"`  // branch: the mainline release it merged into (terminal)
	MergedAt            *time.Time `json:"merged_at"`
	MergedBy            string     `json:"merged_by"`
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
	Product       string   `json:"product"` // product of the devices reporting this build (empty->t7)
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
	DeviceTotal       int          `json:"device_total,omitempty"`       // populated by ListDeploymentsByPackage
	DeviceInstalled   int          `json:"device_installed,omitempty"`   // populated by ListDeploymentsByPackage
	DeviceDownloading int          `json:"device_downloading,omitempty"` // populated by ListDeployments
	DeviceFailed      int          `json:"device_failed,omitempty"`      // populated by ListDeployments
	Product           string       `json:"product,omitempty"`            // release product (populated by ListDeployments)
	DeviceStatus      string       `json:"device_status,omitempty"`      // populated by ResolveUpdateForDevice
}

type UpdateTarget struct {
	UpdateID     int       `json:"update_id"`
	DeviceID     uuid.UUID `json:"device_id"`
	SerialNumber string    `json:"serial_number"` // joined from devices
	BuildID      string    `json:"build_id"`      // current device build
	Status       string    `json:"status"`        // "pending", "downloading", "installing", "installed"
	ErrorCode    string    `json:"error_code"`    // device-reported code when status == "failed"
	UpdatedAt    time.Time `json:"updated_at"`    // when this row last changed state

	StartedAt    *time.Time `json:"started_at"`     // when the device began downloading (nil if not yet)
	CompletedAt  *time.Time `json:"completed_at"`   // when the device reported installed (nil if not yet)
	RebootSentAt *time.Time `json:"reboot_sent_at"` // when the applying reboot was pushed to the device (nil if not yet)
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
	ID          uuid.UUID       `json:"id"`
	Type        string          `json:"type"`
	ApkURL      string          `json:"apk_url"`
	Payload     json.RawMessage `json:"payload"`
	TargetType  string          `json:"target_type"`
	CreatedBy   string          `json:"created_by"` // display name/email snapshotted at creation; "" for system-initiated
	CreatedAt   time.Time       `json:"created_at"`
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
	Progress     *int      `json:"progress,omitempty"` // 0-100 while an install is downloading; nil otherwise
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
	Product             string    // filter by hardware product key ("t7", "kiosk27", ...), or "" (no filter)
	Online              string    // "online", "offline", or "" (no filter)
	BuildID             string    // exact build_id match, or "" (no filter)
	Battery             string    // "low" (<20%), "mid" (20-49%), "ok" (>=50%), or "" (no filter)
	Kiosk               string    // "enabled" (kiosk on), "disabled" (kiosk off), or "" (no filter)
	Charging            string    // "yes" (charging), "no" (not charging), or "" (no filter)
	Timezone            string    // exact timezone match (latest_extra->>'timezone'), or "" (no filter)
	Hidden              string    // "include" (show all), "only" (hidden only), or "" (active only)
	ActiveThresholdSecs int       // legacy: seconds before a device is considered offline (unused for online/offline now)
	// Connected is the set of device IDs with a live WebSocket, used to compute
	// online/offline from real presence rather than check-in recency. Supplied by the
	// handler from ws.Hub; nil means "nobody connected" (all offline).
	Connected []uuid.UUID
	// OnlyIDs, when non-nil, restricts results to these device ids (an empty
	// non-nil slice matches nothing). Set from the user's access policy when they
	// may only see part of the fleet.
	OnlyIDs []uuid.UUID
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
	ID              uuid.UUID  `json:"id"`
	Username        string     `json:"username"`
	Role            string     `json:"role"` // "viewer" | "operator"
	PasswordHash    string     `json:"-"`
	Email           *string    `json:"email,omitempty"`
	EmailVerifiedAt *time.Time `json:"email_verified_at,omitempty"`
	FirstName       string     `json:"first_name"`
	LastName        string     `json:"last_name"`
	CreatedAt       time.Time  `json:"created_at"`
}

// DisplayName returns "First Last" when a name is on file, else the username
// (usually an email) so the UI always has something readable to show.
func (u User) DisplayName() string {
	n := strings.TrimSpace(u.FirstName + " " + u.LastName)
	if n == "" {
		return u.Username
	}
	return n
}

// CreateUser inserts a new DB user. email may be nil (legacy/admin-created accounts
// with no email on file); emailVerified marks it pre-verified (admin vouches for it,
// or the account has no email so verification doesn't apply) vs. NULL (self-signup,
// pending the emailed verify link).
func (d *DB) CreateUser(ctx context.Context, username, passwordHash, role string, email *string, emailVerified bool) (*User, error) {
	return d.CreateUserNamed(ctx, username, passwordHash, role, email, emailVerified, "", "")
}

// CreateUserNamed is CreateUser plus a first/last name captured at sign-up.
func (d *DB) CreateUserNamed(ctx context.Context, username, passwordHash, role string, email *string, emailVerified bool, firstName, lastName string) (*User, error) {
	var u User
	err := d.pool.QueryRow(ctx, `
		INSERT INTO users (username, password_hash, role, email, email_verified_at, first_name, last_name)
		VALUES ($1, $2, $3, $4, CASE WHEN $5 THEN NOW() ELSE NULL END, $6, $7)
		RETURNING id, username, role, password_hash, email, email_verified_at, first_name, last_name, created_at
	`, username, passwordHash, role, email, emailVerified, firstName, lastName).Scan(
		&u.ID, &u.Username, &u.Role, &u.PasswordHash, &u.Email, &u.EmailVerifiedAt, &u.FirstName, &u.LastName, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (d *DB) GetUserByUsername(ctx context.Context, username string) (*User, error) {
	var u User
	err := d.pool.QueryRow(ctx, `
		SELECT id, username, role, password_hash, email, email_verified_at, first_name, last_name, created_at
		FROM users WHERE username = $1
	`, username).Scan(&u.ID, &u.Username, &u.Role, &u.PasswordHash, &u.Email, &u.EmailVerifiedAt, &u.FirstName, &u.LastName, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUserByEmail looks up a user by their (case-insensitive) email — used by the
// forgot-password flow. Accounts with no email on file never match.
func (d *DB) GetUserByEmail(ctx context.Context, email string) (*User, error) {
	var u User
	err := d.pool.QueryRow(ctx, `
		SELECT id, username, role, password_hash, email, email_verified_at, first_name, last_name, created_at
		FROM users WHERE lower(email) = lower($1)
	`, email).Scan(&u.ID, &u.Username, &u.Role, &u.PasswordHash, &u.Email, &u.EmailVerifiedAt, &u.FirstName, &u.LastName, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// GetUser looks up a user by id (e.g. to resolve "who ran this command" for the
// activity log / actions history).
func (d *DB) GetUser(ctx context.Context, id uuid.UUID) (*User, error) {
	var u User
	err := d.pool.QueryRow(ctx, `
		SELECT id, username, role, password_hash, email, email_verified_at, first_name, last_name, created_at
		FROM users WHERE id = $1
	`, id).Scan(&u.ID, &u.Username, &u.Role, &u.PasswordHash, &u.Email, &u.EmailVerifiedAt, &u.FirstName, &u.LastName, &u.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// SetUserEmailVerified marks a user's email as verified now (consumed by the
// verify-email link).
func (d *DB) SetUserEmailVerified(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `UPDATE users SET email_verified_at = NOW() WHERE id = $1`, id)
	return err
}

func (d *DB) ListUsers(ctx context.Context) ([]User, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, username, role, email, email_verified_at, first_name, last_name, created_at FROM users ORDER BY created_at ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var users []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.Username, &u.Role, &u.Email, &u.EmailVerifiedAt, &u.FirstName, &u.LastName, &u.CreatedAt); err != nil {
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

// UpdateUserName sets a user's first/last name — set at sign-up, editable
// afterward by the user or an admin (e.g. for accounts created before names
// existed, or to correct a typo).
// actorColumns lists every (table, column) that snapshots a username as the
// person who did something. Kept in one place so MergeActor and ListOrphanActors
// can never drift apart.
var actorColumns = [][2]string{
	{"audit_log", "actor"},
	{"commands", "created_by"},
	{"test_cases", "created_by"},
	{"command_recipes", "created_by"},
	{"scheduled_recipes", "created_by"},
	{"device_queries", "created_by"},
	{"release_test_results", "tested_by"},
	{"qfil_packages", "added_by"},
	{"dismissed_commands", "dismissed_by"},
	{"release_problems", "reported_by"},
	{"releases", "signed_off_by"},
	{"releases", "testing_done_by"},
	{"releases", "merged_by"},
}

// OrphanActor is a username that appears on past actions but no longer matches
// any account — typically a user deleted after their replacement (e.g. a
// Microsoft sign-in) was created. Merging re-points those rows to the new user.
type OrphanActor struct {
	Username string
	Rows     int64
	LastAt   *time.Time
}

// ListOrphanActors returns usernames present in attribution columns but absent
// from users, with how many rows carry them and when they were last active.
// Non-account actors ("unknown", "", "Scheduled recipe: …") are excluded.
func (d *DB) ListOrphanActors(ctx context.Context) ([]OrphanActor, error) {
	var parts []string
	for _, tc := range actorColumns {
		ts := "NULL::timestamptz"
		if tc[0] == "audit_log" || tc[0] == "commands" {
			ts = "created_at"
		}
		parts = append(parts, fmt.Sprintf("SELECT %s AS u, %s AS t FROM %s", tc[1], ts, tc[0]))
	}
	rows, err := d.pool.Query(ctx, `
		SELECT u, COUNT(*), MAX(t) FROM (`+strings.Join(parts, " UNION ALL ")+`) a
		WHERE u <> '' AND u <> 'unknown' AND u NOT LIKE '%: %'
		  AND NOT EXISTS (SELECT 1 FROM users x WHERE x.username = a.u)
		GROUP BY u ORDER BY MAX(t) DESC NULLS LAST, u`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OrphanActor
	for rows.Next() {
		var o OrphanActor
		if err := rows.Scan(&o.Username, &o.Rows, &o.LastAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// MergeActor rewrites every attribution row carrying username `from` to `to`
// (an existing account's username), plus the saved dashboard layout if the
// target has none. Returns rows rewritten. Runs in one transaction.
func (d *DB) MergeActor(ctx context.Context, from, to string) (int64, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var total int64
	for _, tc := range actorColumns {
		tag, err := tx.Exec(ctx, fmt.Sprintf(`UPDATE %s SET %s = $2 WHERE %s = $1`, tc[0], tc[1], tc[1]), from, to)
		if err != nil {
			return 0, fmt.Errorf("%s.%s: %w", tc[0], tc[1], err)
		}
		total += tag.RowsAffected()
	}
	if _, err := tx.Exec(ctx, `
		UPDATE user_layouts SET username = $2 WHERE username = $1
		  AND NOT EXISTS (SELECT 1 FROM user_layouts x WHERE x.username = $2)`, from, to); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx, `DELETE FROM user_layouts WHERE username = $1`, from); err != nil {
		return 0, err
	}
	return total, tx.Commit(ctx)
}

// UserStats is one person's footprint in the system, for the profile page and the
// "your year" slide of Fleet Wrapped. Everything is keyed by the snapshotted
// username on attribution columns (see actorColumns), so merged/renamed users
// stay consistent with Activity.
type UserStats struct {
	Username string

	Actions    int64 // audit_log rows, excluding page views
	PageViews  int64 // dashboard pages opened
	ActiveDays int   // distinct days with at least one action
	FirstAt    *time.Time
	LastAt     *time.Time
	TopActions []NamedCount // audit actions, most frequent first (max 6)
	Rank       int          // 1 = most active actor by audit rows; 0 = no actions
	Actors     int          // number of actors ranked

	Commands       int64
	CommandTypes   []NamedCount // install_apk, reboot, … most frequent first
	DevicesTouched int64        // distinct devices directly targeted by their commands
	Installs       int64

	ReleasesSignedOff int64
	ReleasesTested    int64
	ReleasesMerged    int64
	TestResults       int64
	TestsPassed       int64
	TestsFailed       int64
	QueriesCreated    int64
	RecipesCreated    int64
	TestCasesCreated  int64
}

// NamedCount is a label with a count, for small breakdown lists.
type NamedCount struct {
	Name  string
	Count int64
	Pct   int // share of the largest entry in its list, 0–100, for bar widths
}

func fillPct(list []NamedCount) {
	if len(list) == 0 || list[0].Count == 0 {
		return
	}
	for i := range list {
		list[i].Pct = int(100 * list[i].Count / list[0].Count)
	}
}

// ── Per-user access policy ──────────────────────────────────────────────────
//
// AccessPolicy refines a role. Admins/devs ignore it. For an operator it decides
// which actions they may take on which devices; for a viewer only "view" matters
// (which devices they can see). Evaluation for (action, device):
//   1. rules whose Actions contain the action (or "*") and whose scope contains
//      the device are applicable; a "deny" among them wins; else an "allow" allows;
//   2. otherwise Base applies ("allow" = everything the role permits, "deny" = nothing).
// Actions that are not about one device (QA, groups) consider fleet-wide rules only.
type AccessPolicy struct {
	Base           string       `json:"base,omitempty"`             // "allow" (default) | "deny"
	HideOutOfScope bool         `json:"hide_out_of_scope,omitempty"` // hide devices the user can't view, vs show with actions disabled
	Rules          []AccessRule `json:"rules,omitempty"`
}

type AccessRule struct {
	Effect    string   `json:"effect"`             // "allow" | "deny"
	Actions   []string `json:"actions"`            // action keys, or ["*"]
	ScopeType string   `json:"scope_type"`         // "all" | "group" | "restaurant"
	ScopeID   string   `json:"scope_id,omitempty"` // group / restaurant id
}

// IsEmpty reports a policy that changes nothing (base allow, no rules, no hiding).
func (p AccessPolicy) IsEmpty() bool {
	return (p.Base == "" || p.Base == "allow") && len(p.Rules) == 0 && !p.HideOutOfScope
}

func (d *DB) GetUserAccess(ctx context.Context, username string) (AccessPolicy, error) {
	var raw []byte
	var pol AccessPolicy
	err := d.pool.QueryRow(ctx, `SELECT access FROM users WHERE username = $1`, username).Scan(&raw)
	if err != nil {
		if err == pgx.ErrNoRows {
			return pol, nil
		}
		return pol, err
	}
	if len(raw) > 0 {
		_ = json.Unmarshal(raw, &pol)
	}
	return pol, nil
}

func (d *DB) SetUserAccess(ctx context.Context, id uuid.UUID, pol AccessPolicy) error {
	b, err := json.Marshal(pol)
	if err != nil {
		return err
	}
	_, err = d.pool.Exec(ctx, `UPDATE users SET access = $2 WHERE id = $1`, id, b)
	return err
}

// DeviceScope is what a device belongs to, for policy scope matching.
type DeviceScope struct {
	RestaurantID *uuid.UUID
	Groups       []uuid.UUID
}

// DeviceScopes loads every device's restaurant and group memberships (two small
// queries) so a request can evaluate scope rules for any number of devices.
func (d *DB) DeviceScopes(ctx context.Context) (map[uuid.UUID]DeviceScope, error) {
	out := map[uuid.UUID]DeviceScope{}
	rows, err := d.pool.Query(ctx, `SELECT id, restaurant_id FROM devices`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var id uuid.UUID
		var rid *uuid.UUID
		if err := rows.Scan(&id, &rid); err != nil {
			rows.Close()
			return nil, err
		}
		out[id] = DeviceScope{RestaurantID: rid}
	}
	rows.Close()
	rows, err = d.pool.Query(ctx, `SELECT device_id, group_id FROM device_groups`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var did, gid uuid.UUID
		if err := rows.Scan(&did, &gid); err != nil {
			return nil, err
		}
		sc := out[did]
		sc.Groups = append(sc.Groups, gid)
		out[did] = sc
	}
	return out, rows.Err()
}

// TopActors ranks people by recorded actions (page views excluded), with
// display names resolved live from users; former usernames show as-is.
func (d *DB) TopActors(ctx context.Context, limit int, exclude string) ([]NamedCount, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT a.actor, COALESCE(NULLIF(TRIM(u.first_name || ' ' || u.last_name), ''), a.actor), COUNT(*) AS n
		FROM audit_log a LEFT JOIN users u ON u.username = a.actor
		WHERE a.actor <> '' AND a.actor <> 'unknown' AND a.actor <> $2 AND a.action <> '`+PageViewAction+`'
		GROUP BY a.actor, u.first_name, u.last_name ORDER BY n DESC LIMIT $1`, limit, exclude)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []NamedCount
	for rows.Next() {
		var key string
		var nc NamedCount
		if err := rows.Scan(&key, &nc.Name, &nc.Count); err != nil {
			return nil, err
		}
		out = append(out, nc)
	}
	fillPct(out)
	return out, rows.Err()
}

// ActorSummary is the per-username footprint shown on the Users roster.
type ActorSummary struct {
	Actions  int64
	Commands int64
	LastAt   *time.Time
}

// ActorSummaries returns actions/commands/last-active per username in two
// grouped queries (cheap: audit_log and commands are small tables).
func (d *DB) ActorSummaries(ctx context.Context) (map[string]ActorSummary, error) {
	out := map[string]ActorSummary{}
	rows, err := d.pool.Query(ctx, `SELECT actor, COUNT(*) FILTER (WHERE action <> '`+PageViewAction+`'), MAX(created_at) FROM audit_log WHERE actor <> '' GROUP BY actor`)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var u string
		var a ActorSummary
		if err := rows.Scan(&u, &a.Actions, &a.LastAt); err != nil {
			rows.Close()
			return nil, err
		}
		out[u] = a
	}
	rows.Close()
	rows, err = d.pool.Query(ctx, `SELECT created_by, COUNT(*), MAX(created_at) FROM commands WHERE created_by <> '' GROUP BY created_by`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u string
		var n int64
		var t *time.Time
		if err := rows.Scan(&u, &n, &t); err != nil {
			return nil, err
		}
		a := out[u]
		a.Commands = n
		if t != nil && (a.LastAt == nil || t.After(*a.LastAt)) {
			a.LastAt = t
		}
		out[u] = a
	}
	return out, rows.Err()
}

func (d *DB) UserStats(ctx context.Context, username string) (*UserStats, error) {
	st := &UserStats{Username: username}
	if username == "" {
		return st, nil
	}
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FILTER (WHERE action <> '`+PageViewAction+`'),
		       COUNT(*) FILTER (WHERE action = '`+PageViewAction+`'),
		       COUNT(DISTINCT (created_at AT TIME ZONE 'UTC')::date), MIN(created_at), MAX(created_at)
		FROM audit_log WHERE actor = $1`, username).Scan(&st.Actions, &st.PageViews, &st.ActiveDays, &st.FirstAt, &st.LastAt)
	if err != nil {
		return nil, err
	}
	rows, err := d.pool.Query(ctx, `
		SELECT action, COUNT(*) FROM audit_log WHERE actor = $1 AND action <> '`+PageViewAction+`'
		GROUP BY action ORDER BY COUNT(*) DESC, action LIMIT 6`, username)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var nc NamedCount
		if err := rows.Scan(&nc.Name, &nc.Count); err != nil {
			rows.Close()
			return nil, err
		}
		st.TopActions = append(st.TopActions, nc)
	}
	rows.Close()
	if st.Actions > 0 {
		err = d.pool.QueryRow(ctx, `
			WITH r AS (SELECT actor, RANK() OVER (ORDER BY COUNT(*) DESC) AS rk, COUNT(*) OVER () AS n
			           FROM audit_log WHERE actor <> '' AND actor <> 'unknown' AND action <> '`+PageViewAction+`' GROUP BY actor)
			SELECT rk, n FROM r WHERE actor = $1`, username).Scan(&st.Rank, &st.Actors)
		if err != nil && err != pgx.ErrNoRows {
			return nil, err
		}
	}
	err = d.pool.QueryRow(ctx, `
		SELECT COUNT(*),
		       COUNT(*) FILTER (WHERE type = 'install_apk'),
		       (SELECT COUNT(DISTINCT ct.target_id) FROM commands c2 JOIN command_targets ct ON ct.command_id = c2.id
		        WHERE c2.created_by = $1 AND c2.target_type = 'devices')
		FROM commands WHERE created_by = $1`, username).Scan(&st.Commands, &st.Installs, &st.DevicesTouched)
	if err != nil {
		return nil, err
	}
	rows, err = d.pool.Query(ctx, `
		SELECT type, COUNT(*) FROM commands WHERE created_by = $1
		GROUP BY type ORDER BY COUNT(*) DESC, type`, username)
	if err != nil {
		return nil, err
	}
	for rows.Next() {
		var nc NamedCount
		if err := rows.Scan(&nc.Name, &nc.Count); err != nil {
			rows.Close()
			return nil, err
		}
		st.CommandTypes = append(st.CommandTypes, nc)
	}
	rows.Close()
	err = d.pool.QueryRow(ctx, `
		SELECT
		  (SELECT COUNT(*) FROM releases WHERE signed_off_by = $1),
		  (SELECT COUNT(*) FROM releases WHERE testing_done_by = $1),
		  (SELECT COUNT(*) FROM releases WHERE merged_by = $1),
		  (SELECT COUNT(*) FROM release_test_results WHERE tested_by = $1 AND status <> 'untested'),
		  (SELECT COUNT(*) FROM release_test_results WHERE tested_by = $1 AND status = 'pass'),
		  (SELECT COUNT(*) FROM release_test_results WHERE tested_by = $1 AND status = 'fail'),
		  (SELECT COUNT(*) FROM device_queries WHERE created_by = $1),
		  (SELECT COUNT(*) FROM command_recipes WHERE created_by = $1),
		  (SELECT COUNT(*) FROM test_cases WHERE created_by = $1)`, username).Scan(
		&st.ReleasesSignedOff, &st.ReleasesTested, &st.ReleasesMerged, &st.TestResults, &st.TestsPassed, &st.TestsFailed,
		&st.QueriesCreated, &st.RecipesCreated, &st.TestCasesCreated)
	if err != nil {
		return nil, err
	}
	fillPct(st.TopActions)
	fillPct(st.CommandTypes)
	return st, nil
}

func (d *DB) UpdateUserName(ctx context.Context, id uuid.UUID, firstName, lastName string) error {
	_, err := d.pool.Exec(ctx, `UPDATE users SET first_name = $2, last_name = $3 WHERE id = $1`, id, firstName, lastName)
	return err
}

// SetUserPassword replaces a user's bcrypt password hash.
func (d *DB) SetUserPassword(ctx context.Context, id uuid.UUID, passwordHash string) error {
	_, err := d.pool.Exec(ctx, `UPDATE users SET password_hash = $2 WHERE id = $1`, id, passwordHash)
	return err
}

// UserToken backs the sign-up email-verification and forgot-password links: a
// single-use, time-limited token tied to one user and one purpose.
type UserToken struct {
	Token     string
	UserID    uuid.UUID
	Purpose   string // "verify" | "reset"
	ExpiresAt time.Time
	UsedAt    *time.Time
	CreatedAt time.Time
}

// CreateUserToken stores a caller-generated token (random, unguessable — see
// internal/dashboard's newToken helper) for one user/purpose.
func (d *DB) CreateUserToken(ctx context.Context, token string, userID uuid.UUID, purpose string, expiresAt time.Time) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO user_tokens (token, user_id, purpose, expires_at)
		VALUES ($1, $2, $3, $4)
	`, token, userID, purpose, expiresAt)
	return err
}

// PeekUserToken validates a token without consuming it — read-only, safe to call
// from a plain GET. Used to render the verify-email confirm page: many mail
// clients/security gateways prefetch/scan links in an email automatically before
// the user ever clicks, which would silently consume a token on GET and leave the
// user's real click seeing "invalid or expired" even though verification already
// succeeded. Requiring an explicit POST (which prefetchers never submit) before
// ConsumeUserToken actually fires avoids that. Same validity rule as
// ConsumeUserToken (unused, unexpired), same errors.
func (d *DB) PeekUserToken(ctx context.Context, token, purpose string) (uuid.UUID, error) {
	var userID uuid.UUID
	err := d.pool.QueryRow(ctx, `
		SELECT user_id FROM user_tokens
		WHERE token = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > NOW()
	`, token, purpose).Scan(&userID)
	if err != nil {
		return uuid.Nil, err
	}
	return userID, nil
}

// ConsumeUserToken atomically validates and marks a token used in one statement,
// so a token can never be redeemed twice even under concurrent requests. Returns
// the owning user_id, or an error (including pgx.ErrNoRows) if the token is
// missing, wrong purpose, already used, or expired.
func (d *DB) ConsumeUserToken(ctx context.Context, token, purpose string) (uuid.UUID, error) {
	var userID uuid.UUID
	err := d.pool.QueryRow(ctx, `
		UPDATE user_tokens SET used_at = NOW()
		WHERE token = $1 AND purpose = $2 AND used_at IS NULL AND expires_at > NOW()
		RETURNING user_id
	`, token, purpose).Scan(&userID)
	if err != nil {
		return uuid.Nil, err
	}
	return userID, nil
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

	// checkinSampleSec is the coalescing window for UpsertCheckin (see there). Set
	// from config at startup and whenever the setting is saved.
	checkinSampleSec atomic.Int32

	// cmdSummaryMu/cmdSummaryCache short-TTL-cache GetCommandDeliverySummaries: it's
	// a full join+CASE-classify over the commands/command_status window, polled
	// every 20s by the Actions page from every open tab/browser. Collapsing repeat
	// calls within a few seconds of each other avoids redoing that join for every
	// concurrent viewer without serving meaningfully stale data.
	cmdSummaryMu    sync.Mutex
	cmdSummaryCache map[[2]int]cmdSummaryCacheEntry
}

type cmdSummaryCacheEntry struct {
	at   time.Time
	data map[uuid.UUID]CommandDeliverySummary
}

var ErrCommandNotTargeted = errors.New("command does not target device")

// ErrLogcatNotTargeted is returned when a device submits a logcat result for a
// request that was not issued to it.
var ErrLogcatNotTargeted = errors.New("logcat request does not target device")

func New(ctx context.Context, connStr string) (*DB, error) {
	cfg, err := pgxpool.ParseConfig(connStr)
	if err != nil {
		return nil, err
	}
	// The default max-conns (~4) starves the pool under concurrent device check-ins
	// plus dashboard traffic, serializing requests. Raise it unless the connection
	// string already specifies pool_max_conns.
	if cfg.MaxConns < 20 && !strings.Contains(connStr, "pool_max_conns") {
		// 25 was tight: the device-list page fans out ~12 concurrent queries, so a
		// couple of dashboards loading it plus check-in upserts could saturate the
		// pool. Since then, Overview (~8), Actions (~8), Fleet Wrapped (~15) and
		// DeviceDetail (~15) all gained the same concurrent-fan-out treatment, so a
		// few admins hitting different heavy pages at once can now approach the old
		// 40 headroom on top of steady check-in traffic. Raised further as a
		// preemptive margin.
		cfg.MaxConns = 60
	}
	cfg.MinConns = 2
	// Recycle connections so a long-lived pool rebalances after failovers/restarts;
	// jitter avoids all conns expiring at once (thundering-herd reconnect).
	cfg.MaxConnLifetime = time.Hour
	cfg.MaxConnLifetimeJitter = 5 * time.Minute
	cfg.MaxConnIdleTime = 30 * time.Minute
	// Pin the session timezone to UTC so `::date` bucketing in the daily-stats
	// rollup is deterministic regardless of the server/container locale (and matches
	// the Go side, which uses time.Now().UTC()).
	if cfg.ConnConfig.RuntimeParams == nil {
		cfg.ConnConfig.RuntimeParams = map[string]string{}
	}
	cfg.ConnConfig.RuntimeParams["timezone"] = "UTC"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
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

// PoolStats returns connection-pool metrics for observability (surfaced at
// /debug/vars). Pool saturation — AcquiredConns approaching MaxConns with a rising
// EmptyAcquireCount — is otherwise invisible and the most likely silent degradation.
func (d *DB) PoolStats() map[string]int64 {
	s := d.pool.Stat()
	return map[string]int64{
		"max_conns":           int64(s.MaxConns()),
		"total_conns":         int64(s.TotalConns()),
		"acquired_conns":      int64(s.AcquiredConns()),
		"idle_conns":          int64(s.IdleConns()),
		"empty_acquire_count": s.EmptyAcquireCount(),
		"acquire_count":       s.AcquireCount(),
		"canceled_acquire":    s.CanceledAcquireCount(),
	}
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
func (d *DB) UpsertCheckin(ctx context.Context, serial, buildID string, batteryPct *int, extra json.RawMessage, mergeExtra bool, product string) (deviceID uuid.UUID, pollIntervalMs int, isNew bool, err error) {
	if len(extra) == 0 {
		extra = json.RawMessage("{}")
	}
	// Normalize once here so every check-in path (HTTP keyframe, WS delta) stores the
	// same canonical key. Empty is left as-is: a delta frame that omits product must
	// not wipe a product already learned from an earlier keyframe (see COALESCE below).
	product = prod.Normalize(product)

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, 0, false, err
	}
	defer tx.Rollback(ctx)

	// Record a build change before the snapshot below overwrites devices.build_id.
	// PK lookup by serial; no row for a brand-new device or an unchanged build.
	if buildID != "" {
		if _, err = tx.Exec(ctx, `
			INSERT INTO device_build_history (device_id, at, from_build, to_build)
			SELECT id, NOW(), build_id, $2 FROM devices
			WHERE serial_number = $1 AND build_id <> '' AND build_id <> $2
			ON CONFLICT DO NOTHING
		`, serial, buildID); err != nil {
			return uuid.Nil, 0, false, err
		}
	}

	extraExpr := "EXCLUDED.latest_extra"
	if mergeExtra {
		extraExpr = "COALESCE(devices.latest_extra, '{}'::jsonb) || EXCLUDED.latest_extra"
	}
	var merged json.RawMessage
	var battery int
	err = tx.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO devices (serial_number, build_id, last_seen_at, latest_battery_pct, latest_extra, product)
		VALUES ($1, $2, NOW(), COALESCE($3, 0), $4, $5)
		ON CONFLICT (serial_number) DO UPDATE
			SET build_id           = EXCLUDED.build_id,
			    last_seen_at       = NOW(),
			    latest_battery_pct = COALESCE($3, devices.latest_battery_pct),
			    -- A check-in reactivates an inactive device: it was only hidden for going
			    -- silent, so hearing from it again brings it back into every list and count.
			    hidden             = false,
			    -- Only overwrite product when the device actually reported one; an empty
			    -- value (delta frame / legacy client) keeps whatever was last learned.
			    product            = COALESCE(NULLIF(EXCLUDED.product, ''), devices.product),
			    latest_extra       = %s
		RETURNING id, poll_interval_ms, (xmax = 0) AS is_new, latest_battery_pct, latest_extra
	`, extraExpr), serial, buildID, batteryPct, extra, product).Scan(&deviceID, &pollIntervalMs, &isNew, &battery, &merged)
	if err != nil {
		return uuid.Nil, 0, false, err
	}

	// crash_events is a bulky per-crash trace list (hundreds of KB) the client resends
	// on every check-in; it is already ingested into device_events by IngestDeviceEvents,
	// so persisting a copy in every checkin row is pure redundant bloat. It ballooned the
	// checkins table to >1GB for one device and made the device page's 48h chart pull
	// hundreds of MB of TOASTed jsonb, timing the page out. Strip it from the historical
	// row (it stays in devices.latest_extra and device_events).
	//
	// wifi_scan is the same story: a multi-KB AP list the geolocation resolver has
	// already consumed by the time we get here, then carried into every later row by
	// the snapshot merge until the next scan replaced it (3.5 GB of a 13.7 GB dump).
	//
	// Coalescing: WS delta frames arrive sub-second, and each one used to store a full
	// snapshot row even when nothing but a volatile reading moved. If the device's most
	// recent row is younger than the sample window and equal on everything except the
	// volatile keys, skip the history row — the live snapshot on devices was updated
	// above regardless. Any transition (battery %, build, charging, pad state, kiosk,
	// boot…) still lands the instant it happens, so charts keep every real event.
	sample := int(d.checkinSampleSec.Load())
	_, err = tx.Exec(ctx, `
		INSERT INTO checkins (device_id, battery_pct, build_id, extra)
		SELECT $1, $2, $3, ($4::jsonb) - `+checkinStripKeys+`
		WHERE NOT EXISTS (
			SELECT 1 FROM (
				SELECT created_at, battery_pct, build_id, extra
				FROM checkins WHERE device_id = $1
				ORDER BY created_at DESC LIMIT 1
			) last
			WHERE $5 > 0
			  AND last.created_at > NOW() - make_interval(secs => $5)
			  AND last.battery_pct = $2
			  AND last.build_id = $3
			  AND (last.extra - `+checkinVolatileKeys+`)
			    = ((($4::jsonb) - `+checkinStripKeys+`) - `+checkinVolatileKeys+`)
		)
	`, deviceID, battery, buildID, merged, sample)
	if err != nil {
		return uuid.Nil, 0, false, err
	}

	return deviceID, pollIntervalMs, isNew, tx.Commit(ctx)
}

// checkinStripKeys are jsonb keys never persisted in checkins.extra (kept only in
// devices.latest_extra): bulky, re-sent every frame, and already stored elsewhere
// (device_events / the learned Wi-Fi index).
const checkinStripKeys = `'crash_events' - 'wifi_scan'`

// checkinVolatileKeys are readings that drift every frame without meaning a state
// change; two rows equal on everything else within the sample window are one sample.
const checkinVolatileKeys = `ARRAY['uptime_seconds','wifi_rssi','ram_usage_mb','battery_temp_c','storage_free_gb','ota_progress','wifi_disconnects_1h','location_accuracy']`

// SetCheckinSampleSec sets the coalescing window used by UpsertCheckin (0 = off).
func (d *DB) SetCheckinSampleSec(sec int) { d.checkinSampleSec.Store(int32(sec)) }

// StripLegacyCheckinKeys removes checkinStripKeys from up to `limit` checkins rows
// of one UTC day — the one-off cleanup for rows written before UpsertCheckin
// stripped them at insert. Returns rows rewritten; fewer than `limit` means the
// day is clean. Small batches matter: the affected rows are tens of KB each
// (TOASTed crash traces), so rewriting a whole day at once was tens of GB of I/O
// that starved every other query on a small production box. The caller paces
// batches and walks the cursor.
func (d *DB) StripLegacyCheckinKeys(ctx context.Context, day time.Time, limit int) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE checkins SET extra = extra - `+checkinStripKeys+`
		WHERE ctid IN (
			SELECT ctid FROM checkins
			WHERE created_at >= $1::date AND created_at < ($1::date + INTERVAL '1 day')
			  AND extra ?| ARRAY['crash_events','wifi_scan']
			LIMIT $2
		)
	`, day.Format("2006-01-02"), limit)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// OldestCheckinDay returns the UTC date of the oldest check-in row, or ok=false when
// the table is empty. Index-backed (created_at).
func (d *DB) OldestCheckinDay(ctx context.Context) (time.Time, bool, error) {
	var t *time.Time
	if err := d.pool.QueryRow(ctx, `SELECT MIN(created_at) FROM checkins`).Scan(&t); err != nil {
		return time.Time{}, false, err
	}
	if t == nil {
		return time.Time{}, false, nil
	}
	return t.UTC().Truncate(24 * time.Hour), true, nil
}

// TouchLastSeen stamps a device's last_seen_at without recording a check-in. It is
// called when a WebSocket drops so "Last seen" reflects the moment the device was
// last live over WS, not its last change-gated check-in — those can lag several
// minutes on an idle-but-online device, which is what made a just-dropped device
// read as "8m ago". While connected the device shows Online and the text is hidden,
// so this value only surfaces once it goes offline.
func (d *DB) TouchLastSeen(ctx context.Context, deviceID uuid.UUID, t time.Time) error {
	_, err := d.pool.Exec(ctx, `UPDATE devices SET last_seen_at = $2 WHERE id = $1`, deviceID, t)
	return err
}

// IngestDeviceEvents records crash/ANR/tombstone entries and reboots carried in a
// check-in's extra. Crashes are deduped by (device, kind, occurred_at) so the client
// can safely re-report the last hour every check-in; a reboot is recorded when the
// per-boot id changes (occurred_at anchored to boot time = now − uptime, so it dedupes
// across the many check-ins of one boot). No-op when neither signal is present — cheap
// on the hot path. Best-effort: parse/insert errors are swallowed rather than failing
// the check-in the caller already committed.
func (d *DB) IngestDeviceEvents(ctx context.Context, deviceID uuid.UUID, buildID string, extra json.RawMessage) {
	if len(extra) == 0 {
		return
	}
	var e struct {
		BootID     string `json:"boot_id"`
		BootReason string `json:"boot_reason"`
		Uptime     *int64 `json:"uptime_seconds"`
		Crashes    []struct {
			Kind    string `json:"kind"`
			TimeMs  int64  `json:"time_ms"`
			Summary string `json:"summary"`
			Trace   string `json:"trace"`
		} `json:"crash_events"`
	}
	if err := json.Unmarshal(extra, &e); err != nil {
		return
	}
	// Cap the device-supplied crash list: without this, a single check-in could pack
	// tens of thousands of tiny crash objects and drive one INSERT round-trip each,
	// amplifying a flood into hundreds of thousands of queries. 200 is far more than an
	// hour of genuine crashes (crashes are reported within the hour they occur).
	const maxCrashEvents = 200
	if len(e.Crashes) > maxCrashEvents {
		e.Crashes = e.Crashes[:maxCrashEvents]
	}
	now := time.Now().UTC()
	for _, c := range e.Crashes {
		if c.Kind == "" || c.TimeMs <= 0 {
			continue
		}
		summary := c.Summary
		if len(summary) > 300 {
			summary = summary[:300]
		}
		detail := c.Trace
		if len(detail) > 64*1024 {
			detail = detail[:64*1024]
		}
		// build_id is the build the device is running as it reports this crash — crashes
		// are reported within the hour they occur, so this attributes the event to the
		// build it happened on, not whatever the device later updates to. That's what
		// keeps a v2.0.2-era crash off a newer release's page (see CrashesOnBuild).
		// DO UPDATE (not DO NOTHING) so a later report of the same crash can backfill
		// the trace if the first report happened to carry only the summary.
		_, _ = d.pool.Exec(ctx, `
			INSERT INTO device_events (device_id, kind, summary, detail, build_id, occurred_at)
			VALUES ($1, $2, $3, $4, $5, $6)
			ON CONFLICT (device_id, kind, occurred_at)
			DO UPDATE SET detail = EXCLUDED.detail
			WHERE device_events.detail = '' AND EXCLUDED.detail <> ''`,
			deviceID, c.Kind, summary, detail, buildID, time.UnixMilli(c.TimeMs).UTC())
	}
	if e.BootID != "" {
		var prev string
		if err := d.pool.QueryRow(ctx, `SELECT last_boot_id FROM devices WHERE id = $1`, deviceID).Scan(&prev); err == nil && prev != e.BootID {
			if prev != "" { // not the first boot id we've seen for this device → a real reboot
				bootTime := now
				if e.Uptime != nil && *e.Uptime > 0 {
					bootTime = now.Add(-time.Duration(*e.Uptime) * time.Second)
				}
				_, _ = d.pool.Exec(ctx, `
					INSERT INTO device_events (device_id, kind, summary, occurred_at)
					VALUES ($1, 'reboot', $2, $3)
					ON CONFLICT (device_id, kind, occurred_at) DO NOTHING`,
					deviceID, e.BootReason, bootTime.Truncate(time.Second))
			}
			_, _ = d.pool.Exec(ctx, `UPDATE devices SET last_boot_id = $2 WHERE id = $1`, deviceID, e.BootID)
		}
	}
}

// GetSummaryFiltered computes the quick-view counts scoped to the contextual
// filters (group, restaurant, production, search, build, timezone, hidden) while
// ignoring the quick-view dimensions themselves (online/battery/kiosk/charging) —
// so the All / Online / Offline / Low-battery / Out-of-kiosk pills show the
// breakdown within the currently selected group or restaurant rather than the
// whole fleet.
func (d *DB) GetSummaryFiltered(ctx context.Context, f DeviceFilter) (Summary, error) {
	activeSecs := f.ActiveThresholdSecs
	if activeSecs <= 0 {
		activeSecs = 180
	}
	var args []interface{}
	argN := 1
	var joins []string
	wheres := []string{"true"}
	switch f.Hidden {
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
	if f.RestaurantID != uuid.Nil {
		wheres = append(wheres, fmt.Sprintf("d.restaurant_id = $%d", argN))
		args = append(args, f.RestaurantID)
		argN++
	}
	if f.ProductionID != uuid.Nil {
		joins = append(joins, fmt.Sprintf("JOIN productions prod ON prod.id = $%d AND d.serial_number LIKE (prod.product_code || prod.model_code || prod.variant || prod.sku || prod.batch || '%%') AND LENGTH(d.serial_number) = 14 AND SUBSTRING(d.serial_number FROM 10 FOR 5) ~ '^[0-9]+$' AND CAST(SUBSTRING(d.serial_number FROM 10 FOR 5) AS INT) BETWEEN prod.start_sequence AND prod.end_sequence", argN))
		args = append(args, f.ProductionID)
		argN++
	}
	if f.BuildID != "" {
		wheres = append(wheres, fmt.Sprintf("d.build_id = $%d", argN))
		args = append(args, f.BuildID)
		argN++
	}
	if f.Timezone != "" {
		wheres = append(wheres, fmt.Sprintf("d.latest_extra->>'timezone' = $%d", argN))
		args = append(args, f.Timezone)
		argN++
	}
	if w, a := productWhere(f.Product, &argN); w != "" {
		wheres = append(wheres, w)
		args = append(args, a...)
	}
	_ = activeSecs // retained for signature compatibility; online is now WS-based
	args = append(args, f.Connected)
	connArg := argN
	q := fmt.Sprintf(`SELECT
			COUNT(d.id),
			COUNT(*) FILTER (WHERE d.id = ANY($%d::uuid[])),
			COUNT(*) FILTER (WHERE d.latest_battery_pct < 20),
			COUNT(DISTINCT d.build_id),
			COUNT(*) FILTER (WHERE EXISTS (SELECT 1 FROM device_config dck WHERE dck.device_id = d.id AND dck.kiosk_enabled = true))
		FROM devices d`, connArg)
	for _, j := range joins {
		q += "\n" + j
	}
	q += "\nWHERE " + strings.Join(wheres, " AND ")
	var s Summary
	err := d.pool.QueryRow(ctx, q, args...).Scan(&s.Total, &s.RecentlyActive, &s.LowBattery, &s.UniqueBuilds, &s.KioskCount)
	return s, err
}

// GetSummary returns fleet counts. RecentlyActive is the number of devices with a live
// WebSocket (from the connected set), so "online" matches the dashboard's per-device
// WS indicator rather than check-in recency. A nil/empty set means none online.
func (d *DB) GetSummary(ctx context.Context, connected []uuid.UUID) (Summary, error) {
	var s Summary
	err := d.pool.QueryRow(ctx, `
		SELECT
			COUNT(d.id),
			COUNT(*) FILTER (WHERE d.id = ANY($1::uuid[])),
			COUNT(*) FILTER (WHERE d.latest_battery_pct < 20),
			COUNT(DISTINCT d.build_id),
			COUNT(*) FILTER (WHERE dc.kiosk_enabled = true)
		FROM devices d
		LEFT JOIN device_config dc ON dc.device_id = d.id
		WHERE NOT d.hidden
	`, connected).Scan(&s.Total, &s.RecentlyActive, &s.LowBattery, &s.UniqueBuilds, &s.KioskCount)
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
		if err := rows.Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct, &dev.PollIntervalMs, &dev.KioskEnabled, &dev.KioskPackage, &dev.LatestExtra, &dev.Hidden, &dev.RestaurantName, &dev.DischargeTotalPct, &dev.DischargeBackfilled, &dev.Product); err != nil {
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

// productWhere builds the SQL predicate for a product filter, appending positional
// args and advancing *argN. Returns "" (no clause) when key is empty. Filtering by the
// default product (T7) also matches legacy devices with an empty product, since those
// resolve to T7 — otherwise the pre-product fleet would vanish from the T7 view.
func productWhere(key string, argN *int) (string, []interface{}) {
	key = prod.Normalize(key)
	if key == "" {
		return "", nil
	}
	if key == prod.DefaultKey {
		w := fmt.Sprintf("(d.product = $%d OR d.product = '')", *argN)
		*argN++
		return w, []interface{}{key}
	}
	w := fmt.Sprintf("d.product = $%d", *argN)
	*argN++
	return w, []interface{}{key}
}

func (d *DB) buildDeviceQuery(f DeviceFilter, sort, dir string, selectRows bool, limit, offset int) (string, []interface{}) {
	var args []interface{}
	argN := 1

	var joins []string
	wheres := []string{"true"}
	// Retired ("hidden") devices are excluded from every list; only the dedicated
	// admin "Retired" view ("only") surfaces them (for reactivation). There is no
	// "show active + retired together" mode — retired means retired.
	switch f.Hidden {
	case "only":
		wheres = append(wheres, "d.hidden")
	default:
		wheres = append(wheres, "NOT d.hidden")
	}

	if f.OnlyIDs != nil {
		wheres = append(wheres, fmt.Sprintf("d.id = ANY($%d)", argN))
		args = append(args, f.OnlyIDs)
		argN++
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
		joins = append(joins, fmt.Sprintf("JOIN productions prod ON prod.id = $%d AND d.serial_number LIKE (prod.product_code || prod.model_code || prod.variant || prod.sku || prod.batch || '%%') AND LENGTH(d.serial_number) = 14 AND SUBSTRING(d.serial_number FROM 10 FOR 5) ~ '^[0-9]+$' AND CAST(SUBSTRING(d.serial_number FROM 10 FOR 5) AS INT) BETWEEN prod.start_sequence AND prod.end_sequence", argN))
		args = append(args, f.ProductionID)
		argN++
	}

	if f.Online == "online" || f.Online == "offline" {
		// Online/offline is live WebSocket presence, not check-in recency: filter by
		// membership in the connected set the handler supplied.
		args = append(args, f.Connected)
		if f.Online == "online" {
			wheres = append(wheres, fmt.Sprintf("d.id = ANY($%d::uuid[])", argN))
		} else {
			wheres = append(wheres, fmt.Sprintf("d.id <> ALL($%d::uuid[])", argN))
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

	if w, a := productWhere(f.Product, &argN); w != "" {
		wheres = append(wheres, w)
		args = append(args, a...)
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
			COALESCE(r.name, ''),
			d.discharge_total_pct, d.discharge_backfilled,
			d.product
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
		case "cycles":
			if dir == "asc" {
				orderClause = "d.discharge_total_pct ASC"
			} else {
				orderClause = "d.discharge_total_pct DESC"
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
		// Append the primary key as a tiebreaker so rows sharing the sort value
		// (e.g. twenty devices at 15% battery) have a stable total order — otherwise
		// tied rows can reorder between page requests and be skipped or duplicated.
		base += "\nORDER BY " + orderClause + ", d.id"

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

// CountDevicesByProduct returns the number of non-hidden devices per catalog
// product key. The stored product column is normalized the same way the rest of
// the app resolves it (empty/unknown → default T7), so legacy rows with no
// product land under the default rather than vanishing. Keys are catalog keys.
func (d *DB) CountDevicesByProduct(ctx context.Context) (map[string]int, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT COALESCE(product, ''), COUNT(*)
		FROM devices
		WHERE NOT hidden
		GROUP BY COALESCE(product, '')
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	counts := make(map[string]int)
	for rows.Next() {
		var raw string
		var n int
		if err := rows.Scan(&raw, &n); err != nil {
			return nil, err
		}
		p, _ := prod.Resolve(raw) // empty/unknown → default product
		counts[p.Key] += n
	}
	return counts, rows.Err()
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
// …, end (inclusive). Each mark carries the latest check-in at or before it, held
// forward until the device reports something different.
//
// Carrying forward is correct, not a fabrication: client telemetry is change-gated
// (a frame is only sent when a gated key or the battery level actually moves), so the
// absence of a check-in means "identical to the last one", not "unknown". A grid finer
// than the device's reporting cadence would otherwise come back mostly empty — a 30 s
// grid against a device reporting every ~2 min is 3 empty marks in 4.
//
// The carry stops at a staleness cap, so a device that goes dark shows a real gap
// instead of a value frozen forever. The cap is per-device rather than fixed: a plugged
// kiosk polling at 30 s and a battery-powered unit deferred to 5 min by the HTTP safety
// net plus Doze cannot share one threshold. It is also never shorter than the grid step
// itself, which would reintroduce the empty marks this exists to avoid.
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
			  AND c.created_at > g.ts - GREATEST(
			        make_interval(secs => $4),
			        make_interval(secs => COALESCE(d.poll_interval_ms, 30000) / 1000.0 * 10),
			        INTERVAL '15 minutes')
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
			d.discharge_total_pct, d.discharge_legacy_pct, d.discharge_backfilled,
			d.restaurant_id, COALESCE(r.name, ''),
			(d.restaurant_id IS NOT NULL) AS deployed_effective,
			d.product
		FROM devices d
		LEFT JOIN device_config dc ON dc.device_id = d.id
		LEFT JOIN restaurants r ON r.id = d.restaurant_id
		WHERE d.serial_number = $1
	`, serial).Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct, &dev.PollIntervalMs, &dev.KioskEnabled, &dev.KioskPackage, &dev.LatestExtra, &dev.DischargeTotalPct, &dev.DischargeLegacyPct, &dev.DischargeBackfilled, &dev.RestaurantID, &dev.RestaurantName, &dev.DeployedEffective, &dev.Product)
	if err != nil {
		return nil, fmt.Errorf("device not found: %w", err)
	}
	return &dev, nil
}

// RecomputeDischargeCycles (re)computes devices.discharge_total_pct for EVERY device as
// the sum of daily battery swing (battery_max − battery_min per day) from
// device_daily_stats. This is the whole cycle metric — there is deliberately NO
// per-check-in counter: summing raw per-check-in SoC deltas integrated battery-gauge
// jitter (a device idling on the charger reads e.g. 99/100/99/100…), which over
// ~345k check-ins inflated the total to thousands of bogus "cycles". The daily
// max−min is immune to that jitter (intra-day noise stays within [min,max]); it can
// undercount days with multiple full cycles, which is the safe direction for a wear
// metric. Cheap (device_daily_stats is ~one small row per device per day), so it runs
// as one set-based statement at startup and hourly from housekeeping — always
// overwriting, so any previously-inflated value self-heals.
func (d *DB) RecomputeDischargeCycles(ctx context.Context) (int, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE devices dv SET
			-- Corrected: exact per-day discharge where we have it (days rolled after the
			-- fix), falling back to the old range estimate for pre-existing days — so
			-- history is estimated and going-forward is exact, without rescanning check-ins.
			discharge_total_pct  = COALESCE(s.total, 0),
			-- Legacy: the pure old range-based figure, kept for transition comparison.
			discharge_legacy_pct = COALESCE(s.legacy, 0),
			discharge_backfilled = true
		FROM (
			SELECT device_id,
			       SUM(COALESCE(discharge_pct, GREATEST(0, battery_max - battery_min)))::bigint AS total,
			       SUM(GREATEST(0, battery_max - battery_min))::bigint AS legacy
			FROM device_daily_stats
			WHERE battery_max IS NOT NULL AND battery_min IS NOT NULL
			GROUP BY device_id
		) s
		WHERE dv.id = s.device_id
		  AND (dv.discharge_total_pct <> COALESCE(s.total, 0)
		       OR dv.discharge_legacy_pct <> COALESCE(s.legacy, 0)
		       OR NOT dv.discharge_backfilled)`)
	if err != nil {
		return 0, err
	}
	updated := int(tag.RowsAffected())
	// Devices with no daily-stats yet: mark computed (counter stays 0) so the detail
	// page shows 0.0 rather than a "seeding…" dash forever.
	if _, err := d.pool.Exec(ctx, `UPDATE devices SET discharge_backfilled = true WHERE NOT discharge_backfilled`); err != nil {
		return updated, err
	}
	return updated, nil
}

// BackfillDischargeCycles is the startup entry point (kept for the main.go call site);
// it just runs the full recompute.
func (d *DB) BackfillDischargeCycles(ctx context.Context) (int, error) {
	return d.RecomputeDischargeCycles(ctx)
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
			d.hidden,
			d.product
		FROM devices d
		LEFT JOIN device_config dc ON dc.device_id = d.id
		WHERE d.id = $1
	`, id).Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct, &dev.PollIntervalMs, &dev.KioskEnabled, &dev.KioskPackage, &dev.LatestExtra, &dev.Hidden, &dev.Product)
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

// GetDeviceNotes returns the freeform operator notes for a device (empty if none).
// Kept off the Device struct (which is scanned in many places) so adding notes
// doesn't touch every device query.
func (d *DB) GetDeviceNotes(ctx context.Context, deviceID uuid.UUID) (string, error) {
	var notes string
	err := d.pool.QueryRow(ctx, `SELECT notes FROM devices WHERE id = $1`, deviceID).Scan(&notes)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return notes, err
}

// SetDeviceNotes stores the freeform operator notes for a device.
func (d *DB) SetDeviceNotes(ctx context.Context, deviceID uuid.UUID, notes string) error {
	_, err := d.pool.Exec(ctx, `UPDATE devices SET notes = $2 WHERE id = $1`, deviceID, notes)
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

// BuildChange is one point in time where a device's reported build_id differed from
// its previous check-in — i.e. it booted into a different build (OTA applied, rollback,
// reflash). Drawn as markers on the device vitals chart.
type BuildChange struct {
	At   time.Time `json:"at"`
	From string    `json:"from"`
	To   string    `json:"to"`
}

// GetBuildChanges returns a device's build transitions, oldest first, from
// device_build_history — written at check-in and backfilled by housekeeping
// (BackfillBuildHistoryDay), so this never touches check-in history.
func (d *DB) GetBuildChanges(ctx context.Context, deviceID uuid.UUID) ([]BuildChange, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT at, from_build, to_build FROM device_build_history
		WHERE device_id = $1 ORDER BY at
	`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BuildChange
	for rows.Next() {
		var c BuildChange
		if err := rows.Scan(&c.At, &c.From, &c.To); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// BackfillBuildHistoryDay derives build changes for one UTC day of check-ins and
// stores them in device_build_history (idempotent via the primary key). Two
// sources: changes between consecutive rows within the day (LAG, index-bounded
// by created_at), and the change between each device's first row of the day and
// its last row before the day (one index-backed lookup per device). Returns the
// rows inserted.
func (d *DB) BackfillBuildHistoryDay(ctx context.Context, day time.Time) (int64, error) {
	ds := day.Format("2006-01-02")
	t1, err := d.pool.Exec(ctx, `
		INSERT INTO device_build_history (device_id, at, from_build, to_build)
		SELECT device_id, created_at, prev, build_id FROM (
			SELECT device_id, created_at, build_id,
			       LAG(build_id) OVER (PARTITION BY device_id ORDER BY created_at) AS prev
			FROM checkins
			WHERE created_at >= $1::date AND created_at < ($1::date + INTERVAL '1 day')
			  AND build_id <> ''
		) t
		WHERE prev IS NOT NULL AND prev <> build_id
		ON CONFLICT DO NOTHING
	`, ds)
	if err != nil {
		return 0, err
	}
	t2, err := d.pool.Exec(ctx, `
		INSERT INTO device_build_history (device_id, at, from_build, to_build)
		SELECT f.device_id, f.created_at, p.build_id, f.build_id
		FROM (
			SELECT DISTINCT ON (device_id) device_id, created_at, build_id
			FROM checkins
			WHERE created_at >= $1::date AND created_at < ($1::date + INTERVAL '1 day')
			  AND build_id <> ''
			ORDER BY device_id, created_at
		) f
		JOIN LATERAL (
			SELECT build_id FROM checkins c
			WHERE c.device_id = f.device_id AND c.created_at < f.created_at AND c.build_id <> ''
			ORDER BY c.created_at DESC LIMIT 1
		) p ON true
		WHERE p.build_id <> f.build_id
		ON CONFLICT DO NOTHING
	`, ds)
	if err != nil {
		return 0, err
	}
	return t1.RowsAffected() + t2.RowsAffected(), nil
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
		ORDER BY created_at DESC, id DESC
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

// RenameGroup changes a group's name in place — a group has no other editable
// fields, so this is the whole "edit" for a group (used by the fleet toolbar).
func (d *DB) RenameGroup(ctx context.Context, id uuid.UUID, name string) error {
	_, err := d.pool.Exec(ctx, `UPDATE groups SET name = $2 WHERE id = $1`, id, name)
	return err
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

// RemoveDevicesFromGroup removes many devices from a group in one statement, so the
// group members list can offer a bulk "Remove from group" instead of one-by-one.
func (d *DB) RemoveDevicesFromGroup(ctx context.Context, serials []string, groupID uuid.UUID) error {
	if len(serials) == 0 {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		DELETE FROM device_groups
		WHERE group_id = $2
		AND device_id IN (SELECT id FROM devices WHERE serial_number = ANY($1))
	`, serials, groupID)
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
		WHERE dg.group_id = $1 AND NOT d.hidden
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

// RenameRestaurant changes only a restaurant's display name — used by the fleet
// toolbar's inline rename, which must not clobber address/timezone/notes the way
// posting the full edit form with a blank rest would.
func (d *DB) RenameRestaurant(ctx context.Context, id uuid.UUID, name string) error {
	_, err := d.pool.Exec(ctx, `UPDATE restaurants SET name = $2 WHERE id = $1`, id, name)
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
func (d *DB) ListAssignableDevices(ctx context.Context, restaurantID uuid.UUID, query, status, battery string, limit int, connected []uuid.UUID) ([]Device, error) {
	if limit <= 0 {
		limit = 20
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
		// Online/offline is live WebSocket presence, not check-in recency.
		args = append(args, connected)
		op := "= ANY"
		if status == "offline" {
			op = "<> ALL"
		}
		q += fmt.Sprintf(" AND d.id %s($%d::uuid[])", op, len(args))
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
// GetRestaurantHealth rolls up per-restaurant health. offline_count is devices WITHOUT
// a live WebSocket (from the connected set), matching the WS-based online status.
func (d *DB) GetRestaurantHealth(ctx context.Context, connected []uuid.UUID, windowDays int) ([]GroupHealth, error) {
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
			WHERE s.day > CURRENT_DATE - $2::int AND d.restaurant_id IS NOT NULL AND NOT d.hidden
			GROUP BY d.restaurant_id
		),
		prior AS (
			SELECT d.restaurant_id, AVG(s.battery_max) AS battery_avg
			FROM device_daily_stats s
			JOIN devices d ON d.id = s.device_id
			WHERE s.day <= CURRENT_DATE - $2::int AND s.day > CURRENT_DATE - ($2::int * 2) AND d.restaurant_id IS NOT NULL AND NOT d.hidden
			GROUP BY d.restaurant_id
		),
		hottest AS (
			-- the single device that drove each restaurant's MAX(temp_max) in the window,
			-- so the report can name the unit instead of just citing the peak number.
			SELECT DISTINCT ON (d.restaurant_id) d.restaurant_id, d.serial_number AS hot_serial
			FROM device_daily_stats s
			JOIN devices d ON d.id = s.device_id
			WHERE s.day > CURRENT_DATE - $2::int AND d.restaurant_id IS NOT NULL AND s.temp_max IS NOT NULL AND NOT d.hidden
			ORDER BY d.restaurant_id, s.temp_max DESC
		),
		devs AS (
			SELECT d.restaurant_id,
				COUNT(*) AS device_count,
				COUNT(*) FILTER (WHERE d.id <> ALL($1::uuid[])) AS offline_count,
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
			WHERE a.status <> 'resolved' AND d.restaurant_id IS NOT NULL AND NOT d.hidden
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
		ORDER BY r.name`, connected, windowDays)
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

// ListProductions returns productions with per-batch counts. "online" is devices with
// a live WebSocket (from the connected set), matching the WS-based status elsewhere.
func (d *DB) ListProductions(ctx context.Context, connected []uuid.UUID) ([]Production, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT
			p.id, p.name, p.product_code, p.model_code, p.variant, p.sku, p.batch,
			p.batch_month, p.batch_year, p.start_sequence, p.end_sequence, p.notes, p.created_at,
			p.end_sequence - p.start_sequence + 1 AS total,
			COUNT(d.id) AS ever_connected,
			COUNT(d.id) FILTER (WHERE d.id = ANY($1::uuid[])) AS online
		FROM productions p
		LEFT JOIN devices d ON
			d.serial_number LIKE (p.product_code || p.model_code || p.variant || p.sku || p.batch || '%')
			AND LENGTH(d.serial_number) = 14
			AND SUBSTRING(d.serial_number FROM 10 FOR 5) ~ '^[0-9]+$'
			AND CAST(SUBSTRING(d.serial_number FROM 10 FOR 5) AS INT) BETWEEN p.start_sequence AND p.end_sequence
		GROUP BY p.id
		ORDER BY p.created_at DESC
	`, connected)
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

func (d *DB) GetProduction(ctx context.Context, id uuid.UUID, connected []uuid.UUID) (*Production, error) {
	var p Production
	err := d.pool.QueryRow(ctx, `
		SELECT
			p.id, p.name, p.product_code, p.model_code, p.variant, p.sku, p.batch,
			p.batch_month, p.batch_year, p.start_sequence, p.end_sequence, p.notes, p.created_at,
			p.end_sequence - p.start_sequence + 1 AS total,
			COUNT(d.id) AS ever_connected,
			COUNT(d.id) FILTER (WHERE d.id = ANY($2::uuid[])) AS online
		FROM productions p
		LEFT JOIN devices d ON
			d.serial_number LIKE (p.product_code || p.model_code || p.variant || p.sku || p.batch || '%')
			AND LENGTH(d.serial_number) = 14
			AND SUBSTRING(d.serial_number FROM 10 FOR 5) ~ '^[0-9]+$'
			AND CAST(SUBSTRING(d.serial_number FROM 10 FOR 5) AS INT) BETWEEN p.start_sequence AND p.end_sequence
		WHERE p.id = $1
		GROUP BY p.id
	`, id, connected).Scan(&p.ID, &p.Name, &p.ProductCode, &p.ModelCode, &p.Variant, &p.SKU, &p.Batch,
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
func (d *DB) GetProductionDevices(ctx context.Context, id uuid.UUID, connected []uuid.UUID) ([]ProductionDevice, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT
			d.serial_number,
			d.id,
			d.build_id,
			d.latest_battery_pct,
			d.last_seen_at,
			d.created_at,
			CASE
				WHEN d.id = ANY($2::uuid[]) THEN 'online'
				ELSE 'offline'
			END AS connection_status
		FROM productions p
		JOIN devices d ON
			d.serial_number LIKE (p.product_code || p.model_code || p.variant || p.sku || p.batch || '%')
			AND LENGTH(d.serial_number) = 14
			AND SUBSTRING(d.serial_number FROM 10 FOR 5) ~ '^[0-9]+$'
			AND CAST(SUBSTRING(d.serial_number FROM 10 FOR 5) AS INT) BETWEEN p.start_sequence AND p.end_sequence
		WHERE p.id = $1 AND NOT d.hidden
		ORDER BY d.serial_number
	`, id, connected)
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
	// Fuzzy serial match: the typed characters must appear in order anywhere in the
	// serial (subsequence), so "at866" or "a070b86" both find "AT070AABU00866".
	// Results are ranked so an exact serial, then a contiguous substring, sort above
	// scattered subsequence hits; shorter serials break ties. %/_/\ in the query are
	// escaped so they're matched literally rather than acting as ILIKE wildcards.
	var sub strings.Builder
	sub.WriteByte('%')
	for _, r := range query {
		if r == '%' || r == '_' || r == '\\' {
			sub.WriteByte('\\')
		}
		sub.WriteRune(r)
		sub.WriteByte('%')
	}
	subseq := sub.String()
	contig := "%" + strings.NewReplacer("\\", "\\\\", "%", "\\%", "_", "\\_").Replace(query) + "%"
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
		WHERE d.serial_number ILIKE $1 AND NOT d.hidden
		ORDER BY
			CASE WHEN lower(d.serial_number) = lower($2) THEN 0
			     WHEN d.serial_number ILIKE $3 THEN 1
			     ELSE 2 END,
			length(d.serial_number),
			d.serial_number
		LIMIT $4
	`, subseq, query, contig, limit)
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
		SELECT id FROM devices WHERE serial_number = ANY($1) AND NOT hidden
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

// GetDevicesByIDs fetches the serial, battery and last-seen for a set of device
// IDs (active devices only). Powers the Actions builder's impact preview, which
// summarises the resolved target set (online/offline split, low-battery risk).
func (d *DB) GetDevicesByIDs(ctx context.Context, ids []uuid.UUID) ([]Device, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT id, serial_number, COALESCE(build_id,''), last_seen_at, COALESCE(latest_battery_pct,0)
		FROM devices WHERE id = ANY($1) AND NOT hidden`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var dev Device
		if err := rows.Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.BatteryPct); err != nil {
			return nil, err
		}
		out = append(out, dev)
	}
	return out, rows.Err()
}

// CountDevicesWithPackage reports how many of the given devices currently report
// the named package installed. Used by the impact preview for Uninstall, which is
// a no-op on devices that don't have the package.
func (d *DB) CountDevicesWithPackage(ctx context.Context, ids []uuid.UUID, pkg string) (int, error) {
	if len(ids) == 0 || pkg == "" {
		return 0, nil
	}
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT device_id) FROM device_packages
		WHERE device_id = ANY($1) AND package_name = $2`, ids, pkg).Scan(&n)
	return n, err
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

// ── Command recipes ─────────────────────────────────────────────────────────────

// Recipe is a saved action preset the Actions builder can replay in one click.
type Recipe struct {
	ID            uuid.UUID       `json:"id"`
	Name          string          `json:"name"`
	Type          string          `json:"type"`
	ApkURL        string          `json:"apk_url"`
	Payload       json.RawMessage `json:"payload"`
	TargetType    string          `json:"target_type"`
	TargetSerials []string        `json:"target_serials"`
	TargetGroups  []uuid.UUID     `json:"target_groups"`
	Scope         json.RawMessage `json:"scope"` // {mode,id,status,battery,kiosk} when target_type='scope'
	CreatedBy     string          `json:"created_by"`
	CreatedAt     time.Time       `json:"created_at"`
}

func (d *DB) CreateRecipe(ctx context.Context, rec Recipe) (*Recipe, error) {
	if rec.Payload == nil {
		rec.Payload = json.RawMessage("{}")
	}
	if rec.Scope == nil {
		rec.Scope = json.RawMessage("{}")
	}
	if rec.TargetSerials == nil {
		rec.TargetSerials = []string{}
	}
	if rec.TargetGroups == nil {
		rec.TargetGroups = []uuid.UUID{}
	}
	err := d.pool.QueryRow(ctx, `
		INSERT INTO command_recipes (name, type, apk_url, payload, target_type, target_serials, target_groups, scope, created_by)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9)
		RETURNING id, created_at`,
		rec.Name, rec.Type, rec.ApkURL, rec.Payload, rec.TargetType, rec.TargetSerials, rec.TargetGroups, rec.Scope, rec.CreatedBy,
	).Scan(&rec.ID, &rec.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &rec, nil
}

const recipeCols = `id, name, type, apk_url, payload, target_type, target_serials, target_groups, scope, created_by, created_at`

func scanRecipe(row interface{ Scan(...any) error }, rec *Recipe) error {
	return row.Scan(&rec.ID, &rec.Name, &rec.Type, &rec.ApkURL, &rec.Payload, &rec.TargetType, &rec.TargetSerials, &rec.TargetGroups, &rec.Scope, &rec.CreatedBy, &rec.CreatedAt)
}

func (d *DB) ListRecipes(ctx context.Context) ([]Recipe, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+recipeCols+` FROM command_recipes ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Recipe
	for rows.Next() {
		var rec Recipe
		if err := scanRecipe(rows, &rec); err != nil {
			return nil, err
		}
		out = append(out, rec)
	}
	return out, rows.Err()
}

// GetRecipe fetches one recipe by id.
func (d *DB) GetRecipe(ctx context.Context, id uuid.UUID) (*Recipe, error) {
	var rec Recipe
	if err := scanRecipe(d.pool.QueryRow(ctx, `SELECT `+recipeCols+` FROM command_recipes WHERE id = $1`, id), &rec); err != nil {
		return nil, err
	}
	return &rec, nil
}

// ResolveTargetIDs resolves a stored target spec to current device IDs (request-free),
// used by the scheduler. all → whole fleet, devices → serials, groups → current members,
// scope → the DeviceFilter that the scope rail builds (re-resolved now).
func (d *DB) ResolveTargetIDs(ctx context.Context, targetType string, serials []string, groups []uuid.UUID, scope json.RawMessage) ([]uuid.UUID, error) {
	switch targetType {
	case "devices":
		return d.GetDeviceIDsBySerials(ctx, serials)
	case "groups":
		return d.GetDeviceIDsInGroups(ctx, groups)
	case "scope":
		var s struct {
			Mode, ID, Status, Battery, Kiosk string
		}
		_ = json.Unmarshal(scope, &s)
		f := DeviceFilter{ActiveThresholdSecs: 180, Online: s.Status, Battery: s.Battery, Kiosk: s.Kiosk}
		switch s.Mode {
		case "restaurant":
			if id, err := uuid.Parse(s.ID); err == nil {
				f.RestaurantID = id
			}
		case "group":
			if id, err := uuid.Parse(s.ID); err == nil {
				f.GroupID = id
			}
		case "build":
			f.BuildID = s.ID
		}
		devs, err := d.ListDevices(ctx, f, 0, 20000, "serial", "asc")
		if err != nil {
			return nil, err
		}
		ids := make([]uuid.UUID, len(devs))
		for i := range devs {
			ids[i] = devs[i].ID
		}
		return ids, nil
	default: // "all"
		return d.GetAllDeviceIDs(ctx)
	}
}

func (d *DB) DeleteRecipe(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM command_recipes WHERE id = $1`, id)
	return err
}

// ── Scheduled recipes ────────────────────────────────────────────────────────────

// ScheduledRecipe runs a recipe on a cron; RecipeName/RecipeType are joined for display.
type ScheduledRecipe struct {
	ID         uuid.UUID  `json:"id"`
	RecipeID   uuid.UUID  `json:"recipe_id"`
	RecipeName string     `json:"recipe_name"`
	RecipeType string     `json:"recipe_type"`
	CronExpr   string     `json:"cron_expr"`
	Enabled    bool       `json:"enabled"`
	RunOnce    bool       `json:"run_once"`
	NextRunAt  *time.Time `json:"next_run_at"`
	LastRunAt  *time.Time `json:"last_run_at"`
	CreatedBy  string     `json:"created_by"`
	CreatedAt  time.Time  `json:"created_at"`
}

func (d *DB) CreateScheduledRecipe(ctx context.Context, recipeID uuid.UUID, cronExpr string, runOnce bool, nextRun time.Time, createdBy string) (*ScheduledRecipe, error) {
	var s ScheduledRecipe
	err := d.pool.QueryRow(ctx, `
		INSERT INTO scheduled_recipes (recipe_id, cron_expr, run_once, next_run_at, created_by)
		VALUES ($1,$2,$3,$4,$5) RETURNING id, created_at`,
		recipeID, cronExpr, runOnce, nextRun, createdBy).Scan(&s.ID, &s.CreatedAt)
	if err != nil {
		return nil, err
	}
	s.RecipeID, s.CronExpr, s.RunOnce, s.Enabled = recipeID, cronExpr, runOnce, true
	s.NextRunAt = &nextRun
	return &s, nil
}

const schedCols = `s.id, s.recipe_id, r.name, r.type, s.cron_expr, s.enabled, s.run_once, s.next_run_at, s.last_run_at, s.created_by, s.created_at`

func scanSchedule(row interface{ Scan(...any) error }, s *ScheduledRecipe) error {
	return row.Scan(&s.ID, &s.RecipeID, &s.RecipeName, &s.RecipeType, &s.CronExpr, &s.Enabled, &s.RunOnce, &s.NextRunAt, &s.LastRunAt, &s.CreatedBy, &s.CreatedAt)
}

func (d *DB) ListScheduledRecipes(ctx context.Context) ([]ScheduledRecipe, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+schedCols+`
		FROM scheduled_recipes s JOIN command_recipes r ON r.id = s.recipe_id
		ORDER BY s.enabled DESC, s.next_run_at NULLS LAST, s.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduledRecipe
	for rows.Next() {
		var s ScheduledRecipe
		if err := scanSchedule(rows, &s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ListDueScheduledRecipes returns enabled schedules whose next run has arrived.
func (d *DB) ListDueScheduledRecipes(ctx context.Context, now time.Time) ([]ScheduledRecipe, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+schedCols+`
		FROM scheduled_recipes s JOIN command_recipes r ON r.id = s.recipe_id
		WHERE s.enabled AND s.next_run_at IS NOT NULL AND s.next_run_at <= $1`, now)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ScheduledRecipe
	for rows.Next() {
		var s ScheduledRecipe
		if err := scanSchedule(rows, &s); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

func (d *DB) DeleteScheduledRecipe(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM scheduled_recipes WHERE id = $1`, id)
	return err
}

// SetScheduleEnabled toggles a schedule; nextRun is set when enabling (nil to clear).
func (d *DB) SetScheduleEnabled(ctx context.Context, id uuid.UUID, enabled bool, nextRun *time.Time) error {
	_, err := d.pool.Exec(ctx, `UPDATE scheduled_recipes SET enabled=$2, next_run_at=$3 WHERE id=$1`, id, enabled, nextRun)
	return err
}

// MarkScheduleFired records a run: set last_run and either the next run time or, for a
// run-once schedule, disable it (next_run NULL).
func (d *DB) MarkScheduleFired(ctx context.Context, id uuid.UUID, ranAt time.Time, nextRun *time.Time, disable bool) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE scheduled_recipes SET last_run_at=$2, next_run_at=$3, enabled = enabled AND NOT $4 WHERE id=$1`,
		id, ranAt, nextRun, disable)
	return err
}

// ── Needs-attention dismissals ──────────────────────────────────────────────────

// ListDismissedCommandIDs returns the set of commands cleared from the
// Needs-attention list. Used by the triage classifier to skip them.
func (d *DB) ListDismissedCommandIDs(ctx context.Context) (map[uuid.UUID]bool, error) {
	rows, err := d.pool.Query(ctx, `SELECT command_id FROM dismissed_commands`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]bool)
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// DismissCommands marks commands as cleared from Needs-attention (idempotent).
// It does not delete or alter the commands themselves.
func (d *DB) DismissCommands(ctx context.Context, ids []uuid.UUID, by string) error {
	if len(ids) == 0 {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO dismissed_commands (command_id, dismissed_by)
		SELECT UNNEST($1::uuid[]), $2
		ON CONFLICT (command_id) DO NOTHING`, ids, by)
	return err
}

// ── Commands ──────────────────────────────────────────────────────────────────

// DeviceCommandType maps a stored command type to what the device agent expects on
// the wire. "query" is a dashboard-only label for an admin-vetted read-only
// diagnostic; the device runs it as an ordinary shell command, so it needs no new
// type. Every place that serializes a command to a device must route through this.
func DeviceCommandType(t string) string {
	if t == "query" {
		return "shell"
	}
	return t
}

// CreateCommand creates a command. For target_type "devices", targetIDs are device UUIDs.
// For "groups", they are group UUIDs. For "all", targetIDs is empty.
// CreateCommand issues a command with no attributed user (system/scheduler-initiated,
// e.g. auto-reboot or a redrive tick). Prefer CreateCommandBy for anything triggered
// from the dashboard or API by a logged-in operator.
func (d *DB) CreateCommand(ctx context.Context, cmdType, apkURL string, payload json.RawMessage, targetType string, targetIDs []uuid.UUID) (*Command, error) {
	return d.CreateCommandBy(ctx, cmdType, apkURL, payload, targetType, targetIDs, "")
}

// CreateCommandBy is CreateCommand plus createdBy, a display name/email snapshotted
// at creation time so "who ran this" survives even if the user account is later
// renamed or deleted. Pass "" for system-initiated commands.
func (d *DB) CreateCommandBy(ctx context.Context, cmdType, apkURL string, payload json.RawMessage, targetType string, targetIDs []uuid.UUID, createdBy string) (*Command, error) {
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
		INSERT INTO commands (type, apk_url, payload, target_type, created_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, type, apk_url, payload, target_type, created_by, created_at
	`, cmdType, apkURL, payload, targetType, createdBy).Scan(&cmd.ID, &cmd.Type, &cmd.ApkURL, &cmd.Payload, &cmd.TargetType, &cmd.CreatedBy, &cmd.CreatedAt)
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
	return d.ListCommandsSince(ctx, 0)
}

// ListCommandsSince returns commands newest-first. sinceDays > 0 limits to the
// recent window (the Actions page uses this — its triage buckets only need recent
// commands, and full history has its own paginated view); 0 returns all.
func (d *DB) ListCommandsSince(ctx context.Context, sinceDays int) ([]Command, error) {
	// update_splash (boot logo) is managed on its own config page (/boot-logo) and
	// deliberately excluded from the Actions and history lists — it's a fleet config
	// action, not a tracked one-off command.
	q := `SELECT id, type, apk_url, payload, target_type, created_by, created_at FROM commands WHERE type != 'update_splash'`
	if sinceDays > 0 {
		q += fmt.Sprintf(" AND created_at >= NOW() - INTERVAL '%d days'", sinceDays)
		// The day window alone doesn't bound the row count — a busy fleet issuing
		// many commands/day can still return a large result set within 30 days. Cap
		// it as a backstop; the Actions page's triage buckets never need more than
		// this. sinceDays == 0 is CommandHistory's real unbounded, paginated view —
		// capping that too would silently truncate it once a fleet passes 2000
		// total commands, so the cap only applies to the windowed call.
		q += " ORDER BY created_at DESC LIMIT 2000"
	} else {
		q += " ORDER BY created_at DESC"
	}
	rows, err := d.pool.Query(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var cmds []Command
	for rows.Next() {
		var c Command
		if err := rows.Scan(&c.ID, &c.Type, &c.ApkURL, &c.Payload, &c.TargetType, &c.CreatedBy, &c.CreatedAt); err != nil {
			return nil, err
		}
		cmds = append(cmds, c)
	}
	return cmds, rows.Err()
}

// GetLatestCommandByType returns the most recent command of the given type, or
// (nil, nil) if none exists. Used by the Boot logo config page to show the status
// of the last applied splash without surfacing it in the command history lists.
func (d *DB) GetLatestCommandByType(ctx context.Context, cmdType string) (*Command, error) {
	var c Command
	err := d.pool.QueryRow(ctx, `
		SELECT id, type, apk_url, payload, target_type, created_at
		FROM commands WHERE type = $1 ORDER BY created_at DESC LIMIT 1
	`, cmdType).Scan(&c.ID, &c.Type, &c.ApkURL, &c.Payload, &c.TargetType, &c.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
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
		SELECT id, type, apk_url, payload, target_type, created_by, created_at
		FROM commands WHERE id = $1
	`, id).Scan(&c.ID, &c.Type, &c.ApkURL, &c.Payload, &c.TargetType, &c.CreatedBy, &c.CreatedAt)
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
	// Downloading/Installing are a phase breakdown of the in-flight (Delivered)
	// devices for install_apk commands, so the Actions in-progress row can show
	// "3 downloading · 1 installing" instead of a single lumped count. They are a
	// subset of Delivered, not additive to the totals.
	Downloading int
	Installing  int
}

// GetCommandDeliverySummaries returns per-command status rollups for the history
// list. It aggregates the real per-device status table (command_status) — the
// same source GetCommandDeliveries reads — and applies the same expiry rule, so a
// device stuck at pending/delivered past the timeout counts as expired rather than
// perpetually "in flight" (otherwise a day-old command still animates as live).
// (A device with no status row yet isn't counted here; the command detail page is
// the authoritative per-device view including those.)
// GetCommandDeliverySummaries rolls up per-command delivery status. sinceDays > 0
// scopes the (otherwise whole-table) scan to commands created within that window
// — the Actions page passes a window since its triage buckets only need recent
// commands; the full history view passes 0.
const cmdSummaryCacheTTL = 5 * time.Second

func (d *DB) GetCommandDeliverySummaries(ctx context.Context, expirySec, sinceDays int) (map[uuid.UUID]CommandDeliverySummary, error) {
	if expirySec <= 0 {
		expirySec = 300
	}
	installExpiry := expirySec * 3
	if installExpiry < 900 {
		installExpiry = 900
	}

	key := [2]int{expirySec, sinceDays}
	d.cmdSummaryMu.Lock()
	if e, ok := d.cmdSummaryCache[key]; ok && time.Since(e.at) < cmdSummaryCacheTTL {
		d.cmdSummaryMu.Unlock()
		return e.data, nil
	}
	d.cmdSummaryMu.Unlock()
	sinceClause := ""
	if sinceDays > 0 {
		sinceClause = fmt.Sprintf("WHERE c.created_at >= NOW() - INTERVAL '%d days'", sinceDays)
	}
	rows, err := d.pool.Query(ctx, fmt.Sprintf(`
		SELECT command_id, status, COUNT(*) FROM (
			SELECT cs.command_id,
				CASE
					WHEN c.type IN ('shell','screenshot','reboot')
						AND cs.status IN ('pending','delivered')
						AND c.created_at <= NOW() - INTERVAL '%d seconds'
					THEN 'expired'
					WHEN c.type = 'install_apk'
						AND cs.status IN ('pending','delivered')
						AND c.created_at <= NOW() - INTERVAL '%d seconds'
					THEN 'expired'
					ELSE cs.status
				END AS status
			FROM command_status cs
			JOIN commands c ON c.id = cs.command_id
			%s
		) t
		GROUP BY command_id, status
	`, expirySec, installExpiry, sinceClause))
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
			s.Pending += count
		// An install actively downloading/installing is in flight: count it as
		// delivered so the command stays in the Actions "In progress" bucket
		// (commandBucket keys "prog" off Pending/Delivered) instead of falling
		// through to "done" and disappearing while the app is still installing.
		// Downloading/Installing are also tracked separately (a subset of
		// Delivered) so the row can show a per-phase tally.
		case "delivered":
			s.Delivered += count
		case "downloading":
			s.Delivered += count
			s.Downloading += count
		case "installing":
			s.Delivered += count
			s.Installing += count
		case "installed", "completed":
			s.Completed += count
		case "failed", "expired":
			s.Failed += count
		}
		out[cid] = s
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	d.cmdSummaryMu.Lock()
	if d.cmdSummaryCache == nil {
		d.cmdSummaryCache = make(map[[2]int]cmdSummaryCacheEntry)
	}
	d.cmdSummaryCache[key] = cmdSummaryCacheEntry{at: time.Now(), data: out}
	d.cmdSummaryMu.Unlock()
	return out, nil
}

// GetCommandTargetSerialsBatch resolves target serials for many commands in one
// query, replacing the per-command N+1 on the Actions and history pages.
func (d *DB) GetCommandTargetSerialsBatch(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID][]string, error) {
	out := make(map[uuid.UUID][]string, len(ids))
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT ct.command_id, d.serial_number
		FROM command_targets ct
		JOIN devices d ON d.id = ct.target_id
		WHERE ct.command_id = ANY($1)
		ORDER BY d.serial_number
	`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var cid uuid.UUID
		var s string
		if err := rows.Scan(&cid, &s); err != nil {
			return nil, err
		}
		out[cid] = append(out[cid], s)
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
// GetCommandDeviceIDs resolves the device IDs a command reached: direct device
// targets, group expansion, and any device that already has a status row. Used to
// push a cancel to exactly those devices when the command is deleted, so a device
// mid-download aborts instead of finishing an install the operator just removed.
func (d *DB) GetCommandDeviceIDs(ctx context.Context, commandID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT ct.target_id FROM command_targets ct JOIN commands c ON c.id = ct.command_id
		  WHERE ct.command_id = $1 AND c.target_type = 'devices'
		UNION
		SELECT dg.device_id FROM command_targets ct JOIN commands c ON c.id = ct.command_id
		  JOIN device_groups dg ON dg.group_id = ct.target_id
		  WHERE ct.command_id = $1 AND c.target_type = 'groups'
		UNION
		SELECT cs.device_id FROM command_status cs WHERE cs.command_id = $1`, commandID)
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
			AND (
				cs.status IN ('downloading', 'installing', 'installed', 'failed', 'completed', 'cancelled', 'expired')
				-- A 'delivered' command means only that hub.Push enqueued the frame onto the
				-- socket — NOT that the device received it. A half-open socket (Wi-Fi dropped
				-- with no FIN) swallows the frame, so ANY command type can wedge at 'delivered'
				-- and never run. This query runs on a (re)connect flush / check-in, and a fresh
				-- connection is proof the PRIOR delivery is dead — so re-deliver immediately,
				-- with no staleness grace (that ~90s wait is what made a reconnect take minutes
				-- to actually run queued commands). received_at set = the device CONFIRMED
				-- receipt, so it has the command — never re-deliver (stops dup + spam); the
				-- client also dedups by id, so a re-push during an in-flight ack can't double-
				-- run. Reboot is exempt: it completes via CompleteDeliveredReboots, not a re-push.
				-- (The periodic RedriveStuckDeliveries keeps its own staleness window for a
				-- device that stays continuously connected.)
				OR (cs.status = 'delivered'
					AND (cs.received_at IS NOT NULL OR c.type = 'reboot'))
			)
		)
		-- Serialize the queue per device: deliver a command only if there is NO OTHER
		-- non-terminal command for this device created BEFORE it. So at any moment only the
		-- single oldest unfinished command is eligible — commands run strictly one at a time,
		-- FIFO, whether the device is online or offline. When the current one reaches a
		-- terminal state the next becomes eligible (the ack path flushes it immediately).
		-- Real-time control frames (remote-control capture, checkin_now, etc.) are sent
		-- directly over WS, not as command rows, so they are unaffected.
		--
		-- OTA is exempt from this gate entirely (both directions: c.type='ota' skips being
		-- gated, and c2.type<>'ota' below means an OTA never gates anything else) — a
		-- download+install can run minutes long, and blocking every other command on this
		-- device behind it for that whole time (e.g. a screenshot or reboot request just
		-- sitting there) is worse than the two racing. The client already serializes actually
		-- applying an OTA around other work on its own.
		AND (
			c.type = 'ota'
			OR NOT EXISTS (
				SELECT 1 FROM commands c2
				WHERE c2.id <> c.id AND c2.created_at < c.created_at AND c2.type <> 'ota'
				  AND (
					c2.target_type = 'all'
					OR (c2.target_type = 'devices' AND EXISTS (
						SELECT 1 FROM command_targets ct2 WHERE ct2.command_id = c2.id AND ct2.target_id = $1))
					OR (c2.target_type = 'groups' AND EXISTS (
						SELECT 1 FROM command_targets ct2 JOIN device_groups dg2 ON dg2.group_id = ct2.target_id
						WHERE ct2.command_id = c2.id AND dg2.device_id = $1))
				  )
				  AND NOT EXISTS (SELECT 1 FROM command_status s2 WHERE s2.command_id = c2.id AND s2.device_id = $1
					AND s2.status IN ('installed','failed','completed','cancelled','expired'))
				  -- A blocker must still be LIVE: a command past its delivery window (older than an hour
				  -- and not actively downloading/installing) never delivers, so it must not wedge the queue
				  -- behind it — otherwise stale pending commands (e.g. offline-device screenshots that pile
				  -- up) block every newer command forever.
				  AND (
						c2.created_at > NOW() - INTERVAL '1 hour'
						OR EXISTS (SELECT 1 FROM command_status s3 WHERE s3.command_id = c2.id AND s3.device_id = $1
							AND s3.status IN ('downloading','installing'))
				  )
			)
		)
		-- Only deliver commands that are still CURRENT: a command older than an hour is stale
		-- and must NOT suddenly run when a device reconnects (a day-old queued reboot firing
		-- on reconnect is exactly the surprise-reboot we must avoid). This matches the Queue
		-- tab's display window, so what you see queued is what will run. An install already
		-- actively downloading/installing stays deliverable regardless of age so it can finish.
		-- NOTE: this outer query has NO command_status join, so the "still running" exception
		-- must be a correlated subquery — referencing a bare cs.status here is a missing-FROM
		-- error that makes the WHOLE query fail (silently killing reconnect-flush + advance).
		AND (
			c.created_at > NOW() - INTERVAL '1 hour'
			OR EXISTS (
				SELECT 1 FROM command_status cs3
				WHERE cs3.command_id = c.id AND cs3.device_id = $1
				AND cs3.status IN ('downloading', 'installing')
			)
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

// StuckDelivery is one command wedged at 'delivered' for a specific device — the frame
// was enqueued onto the socket but the device never acknowledged progress or a result.
type StuckDelivery struct {
	CommandID uuid.UUID
	DeviceID  uuid.UUID
	Type      string
	ApkURL    string
	Payload   json.RawMessage
}

// ListStuckDeliveredCommands returns commands sitting at 'delivered' for a device with
// no progress for longer than staleSeconds — the half-open-socket wedge. The caller
// re-pushes them to devices that are currently connected (an offline device gets them
// on its next WS-connect flush instead). Reboot is excluded (it completes via
// CompleteDeliveredReboots, never a re-push), and short-TTL one-shots (shell/screenshot)
// are only re-driven while still within their 5-minute leash so an ancient one isn't
// resurrected. Applies to every other action type — install, uninstall, kiosk, boot
// logo, query, etc. — because any of them can wedge the same way.
func (d *DB) ListStuckDeliveredCommands(ctx context.Context, staleSeconds int) ([]StuckDelivery, error) {
	if staleSeconds <= 0 {
		staleSeconds = 90
	}
	rows, err := d.pool.Query(ctx, `
		SELECT c.id, cs.device_id, c.type, c.apk_url, c.payload
		FROM command_status cs
		JOIN commands c ON c.id = cs.command_id
		WHERE cs.status = 'delivered'
		  AND cs.received_at IS NULL          -- device never confirmed receipt; a confirmed one already has it
		  AND cs.updated_at <= NOW() - make_interval(secs => $1)
		  AND c.type <> 'reboot'
		  AND c.created_at > NOW() - INTERVAL '1 hour'   -- never re-push a stale command
		  AND (c.type NOT IN ('shell', 'screenshot') OR c.created_at > NOW() - INTERVAL '5 minutes')
	`, staleSeconds)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StuckDelivery
	for rows.Next() {
		var s StuckDelivery
		if err := rows.Scan(&s.CommandID, &s.DeviceID, &s.Type, &s.ApkURL, &s.Payload); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// DevicesWithPendingInstall returns which of the given devices already have an
// in-flight install_apk for apkURL — a command targeting the device (directly, by
// group, or "all") whose per-device status is not yet terminal (pending or
// delivered, i.e. not installed/failed/completed). Used to stop piling up a second
// install of the same APK on a device that's already installing it.
func (d *DB) DevicesWithPendingInstall(ctx context.Context, apkURL string, deviceIDs []uuid.UUID, expirySec int) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool)
	if apkURL == "" || len(deviceIDs) == 0 {
		return out, nil
	}
	// Only a *recent* install counts as in-flight. An install that never completed
	// (device offline / command never delivered) leaves a non-terminal row forever;
	// without this bound it would block every future re-install of the same APK on
	// that device. Mirror the install-expiry window used elsewhere (expiry*3, ≥15m).
	if expirySec <= 0 {
		expirySec = 300
	}
	installExpiry := expirySec * 3
	if installExpiry < 900 {
		installExpiry = 900
	}
	rows, err := d.pool.Query(ctx, fmt.Sprintf(`
		SELECT dev.id
		FROM unnest($2::uuid[]) AS dev(id)
		WHERE EXISTS (
			SELECT 1 FROM commands c
			WHERE c.type = 'install_apk' AND c.apk_url = $1
			  AND c.created_at > NOW() - INTERVAL '%d seconds'
			  AND (
				c.target_type = 'all'
				OR (c.target_type = 'devices' AND EXISTS (
					SELECT 1 FROM command_targets ct WHERE ct.command_id = c.id AND ct.target_id = dev.id))
				OR (c.target_type = 'groups' AND EXISTS (
					SELECT 1 FROM command_targets ct JOIN device_groups dg ON dg.group_id = ct.target_id
					WHERE ct.command_id = c.id AND dg.device_id = dev.id))
			  )
			  AND NOT EXISTS (
				SELECT 1 FROM command_status cs
				WHERE cs.command_id = c.id AND cs.device_id = dev.id
				  AND cs.status IN ('installed','failed','completed'))
		)
	`, installExpiry), apkURL, deviceIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// MarkCommandsDelivered records that these commands were sent to the device.
// SetCommandStatusForDevices upserts one command's status for many devices in a
// single statement — batches the pushCommand delivery/ack writes (previously one
// query per online target). overwrite=true updates an existing row (ack), false
// leaves it (delivered). Callers pass already-targeted device IDs.
func (d *DB) SetCommandStatusForDevices(ctx context.Context, commandID uuid.UUID, deviceIDs []uuid.UUID, status string, overwrite bool) error {
	if len(deviceIDs) == 0 {
		return nil
	}
	conflict := "DO NOTHING"
	if overwrite {
		conflict = "DO UPDATE SET status = EXCLUDED.status, updated_at = NOW()"
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO command_status (command_id, device_id, status, updated_at)
		SELECT $1, dev, $3, NOW() FROM UNNEST($2::uuid[]) AS dev
		ON CONFLICT (command_id, device_id) `+conflict, commandID, deviceIDs, status)
	return err
}

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

// CompleteDeliveredReboots marks a device's outstanding reboot commands completed
// once the device reconnects / checks in again — the reconnection is the proof the
// reboot actually happened. Reboot is acked 'delivered' when sent (not 'completed'),
// because a successful WS push only means "queued to the socket", not "device
// rebooted": a stale/half-open connection, or a device that received the command but
// never rebooted, previously produced a false 'completed' (FW-2026-000033). Only rows
// still in 'delivered' move, so a routine reconnect can't resurrect a command that has
// already reached a terminal state.
func (d *DB) CompleteDeliveredReboots(ctx context.Context, deviceID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE command_status cs
		SET status = 'completed', updated_at = NOW()
		FROM commands c
		WHERE c.id = cs.command_id
		  AND cs.device_id = $1
		  AND c.type = 'reboot'
		  AND cs.status = 'delivered'
	`, deviceID)
	return err
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

	// Don't let a device move a command backwards out of a terminal state (e.g.
	// re-report 'downloading'/'failed' after 'installed') — that would corrupt OTA
	// and deployment rollups. Re-acking the same terminal status is a harmless no-op.
	_, err = d.pool.Exec(ctx, `
		INSERT INTO command_status (command_id, device_id, status, updated_at)
		VALUES ($1, $2, $3, NOW())
		ON CONFLICT (command_id, device_id) DO UPDATE
			SET status = EXCLUDED.status, progress = NULL, updated_at = NOW()
			WHERE command_status.status NOT IN ('installed', 'failed', 'completed')
	`, commandID, deviceID, status)
	return err
}

// MarkCommandReceived stamps received_at when a device confirms — via a 'received' ack —
// that it actually GOT the command, as opposed to 'delivered' which only means the frame
// was enqueued onto the socket. Only the first receipt is stamped and a terminal/progress
// row's status is never disturbed. Redrive and reconnect re-delivery key off
// received_at IS NULL, so this is exactly what stops a received command being re-pushed.
func (d *DB) MarkCommandReceived(ctx context.Context, commandID, deviceID uuid.UUID) error {
	targeted, err := d.commandTargetsDevice(ctx, commandID, deviceID)
	if err != nil {
		return err
	}
	if !targeted {
		return ErrCommandNotTargeted
	}
	_, err = d.pool.Exec(ctx, `
		INSERT INTO command_status (command_id, device_id, status, received_at, updated_at)
		VALUES ($1, $2, 'delivered', NOW(), NOW())
		ON CONFLICT (command_id, device_id) DO UPDATE
			SET received_at = COALESCE(command_status.received_at, NOW())
			WHERE command_status.received_at IS NULL
	`, commandID, deviceID)
	return err
}

// CommandBlocked reports whether a command must WAIT before being delivered to a device
// because an earlier (older) command for that device hasn't finished yet. Mirrors the
// per-device serialization gate in GetPendingCommandsForDevice; used to hold the immediate
// push at create time so no two commands run at once on a device (whole queue is FIFO).
func (d *DB) CommandBlocked(ctx context.Context, commandID, deviceID uuid.UUID) (bool, error) {
	var blocked bool
	err := d.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM commands c
			WHERE c.id = $1 AND EXISTS (
				SELECT 1 FROM commands c2
				WHERE c2.id <> c.id AND c2.created_at < c.created_at
				  AND (
					c2.target_type = 'all'
					OR (c2.target_type = 'devices' AND EXISTS (
						SELECT 1 FROM command_targets ct2 WHERE ct2.command_id = c2.id AND ct2.target_id = $2))
					OR (c2.target_type = 'groups' AND EXISTS (
						SELECT 1 FROM command_targets ct2 JOIN device_groups dg2 ON dg2.group_id = ct2.target_id
						WHERE ct2.command_id = c2.id AND dg2.device_id = $2))
				  )
				  AND NOT EXISTS (SELECT 1 FROM command_status s2 WHERE s2.command_id = c2.id AND s2.device_id = $2
					AND s2.status IN ('installed','failed','completed','cancelled','expired'))
				  AND (
						c2.created_at > NOW() - INTERVAL '1 hour'
						OR EXISTS (SELECT 1 FROM command_status s3 WHERE s3.command_id = c2.id AND s3.device_id = $2
							AND s3.status IN ('downloading','installing'))
				  )
			)
		)`, commandID, deviceID).Scan(&blocked)
	return blocked, err
}

// CancelDeviceCommand removes one command from a single device's queue by marking its
// per-device status 'cancelled' — never moving a row that already reached a terminal
// state. Per-device (upsert on command_status), so cancelling a group/all command on one
// device leaves the others untouched, and a not-yet-pushed 'pending' command with no
// status row is cancelled too.
func (d *DB) CancelDeviceCommand(ctx context.Context, commandID, deviceID uuid.UUID) error {
	targeted, err := d.commandTargetsDevice(ctx, commandID, deviceID)
	if err != nil {
		return err
	}
	if !targeted {
		return ErrCommandNotTargeted
	}
	_, err = d.pool.Exec(ctx, `
		INSERT INTO command_status (command_id, device_id, status, updated_at)
		VALUES ($1, $2, 'cancelled', NOW())
		ON CONFLICT (command_id, device_id) DO UPDATE
			SET status = 'cancelled', progress = NULL, updated_at = NOW()
			WHERE command_status.status NOT IN ('installed', 'failed', 'completed', 'cancelled', 'expired')
	`, commandID, deviceID)
	return err
}

// SetCommandProgress records an interim status ('downloading' or 'installing') for a
// command on a device, with an optional percent (0-100, meaningful while
// downloading). Terminal statuses go through AckCommand instead.
func (d *DB) SetCommandProgress(ctx context.Context, commandID, deviceID uuid.UUID, status string, progress *int) error {
	targeted, err := d.commandTargetsDevice(ctx, commandID, deviceID)
	if err != nil {
		return err
	}
	if !targeted {
		return ErrCommandNotTargeted
	}
	if progress != nil {
		p := *progress
		if p < 0 {
			p = 0
		} else if p > 100 {
			p = 100
		}
		progress = &p
	}
	// Interim progress must never overwrite a terminal state (a late 'downloading'
	// arriving after 'installed' would revert the row).
	_, err = d.pool.Exec(ctx, `
		INSERT INTO command_status (command_id, device_id, status, progress, updated_at)
		VALUES ($1, $2, $3, $4, NOW())
		ON CONFLICT (command_id, device_id) DO UPDATE
			SET status = EXCLUDED.status, progress = EXCLUDED.progress, updated_at = NOW()
			WHERE command_status.status NOT IN ('installed', 'failed', 'completed')
	`, commandID, deviceID, status, progress)
	return err
}

// ActiveOTAProgressRow is one device's persisted OTA progress, used to rehydrate
// shell.Manager's in-memory cache on process start — see ActiveOTAProgress.
type ActiveOTAProgressRow struct {
	DeviceID  uuid.UUID
	CommandID uuid.UUID
	Status    string // "downloading" or "installing" — see SetCommandProgress
	Progress  int
}

// ActiveOTAProgress returns the last-persisted progress for every OTA command
// still mid-flight (command_status.status IN downloading/installing). The live
// progress a device page/deployment page renders normally comes from
// shell.Manager's in-memory cache, which is empty right after a server
// restart — the caller rehydrates that cache from this on startup so an
// in-progress OTA doesn't show a blank "—" until the device happens to report
// fresh progress again.
func (d *DB) ActiveOTAProgress(ctx context.Context) ([]ActiveOTAProgressRow, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT cs.device_id, cs.command_id, cs.status, COALESCE(cs.progress, 0)
		FROM command_status cs
		JOIN commands c ON c.id = cs.command_id
		WHERE c.type = 'ota' AND cs.status IN ('downloading', 'installing')
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ActiveOTAProgressRow
	for rows.Next() {
		var r ActiveOTAProgressRow
		if err := rows.Scan(&r.DeviceID, &r.CommandID, &r.Status, &r.Progress); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// StalledInstall identifies an install delivery the stall sweep marked failed.
type StalledInstall struct {
	CommandID uuid.UUID
	DeviceID  uuid.UUID
}

// ExpireStalledInstalls marks install_apk deliveries that stopped reporting progress
// as 'failed'. An install stuck 'downloading'/'installing' whose last report is older
// than stallMinutes has lost its device — it gave up after exhausting retries, rebooted,
// or its terminal ack was lost during the same network outage that stalled the download.
// Without this the row sits "in flight" forever, because install commands are exempt
// from the short command TTL. Returns the affected rows so callers can publish updates.
func (d *DB) ExpireStalledInstalls(ctx context.Context, stallMinutes int) ([]StalledInstall, error) {
	rows, err := d.pool.Query(ctx, `
		UPDATE command_status cs
		SET status = 'failed', progress = NULL, updated_at = NOW()
		FROM commands c
		WHERE cs.command_id = c.id
		  AND c.type = 'install_apk'
		  AND cs.status IN ('downloading', 'installing')
		  AND cs.updated_at <= NOW() - make_interval(mins => $1)
		RETURNING cs.command_id, cs.device_id
	`, stallMinutes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StalledInstall
	for rows.Next() {
		var s StalledInstall
		if err := rows.Scan(&s.CommandID, &s.DeviceID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ExpireOverdueCommands marks 'expired' any command that has sat non-terminal past its
// per-type delivery deadline WITHOUT ever starting — i.e. still 'pending'/'delivered'
// (never progressed to downloading/installing and never got a terminal ack). This is the
// backstop that stops a command wedging forever: a device that never returns, or that got
// the command but never acted, no longer shows "in flight" indefinitely. Deadlines are
// kept in lock-step with the delivery TTL in GetPendingCommandsForDevice, so a command
// expires exactly when it stops being deliverable. Reboot is exempt (its lifecycle is
// owned by CompleteDeliveredReboots / RedriveStuckReboots). Actively-progressing installs
// ('downloading'/'installing') are left to ExpireStalledInstalls. 'expired' is not in the
// terminal guard set, so a device that acts late can still un-expire it with a real ack.
func (d *DB) ExpireOverdueCommands(ctx context.Context) ([]StalledInstall, error) {
	rows, err := d.pool.Query(ctx, `
		UPDATE command_status cs
		SET status = 'expired', progress = NULL, updated_at = NOW()
		FROM commands c
		WHERE cs.command_id = c.id
		  AND cs.status IN ('pending', 'delivered')
		  AND c.type <> 'reboot'
		  AND c.created_at <= NOW() - CASE
				WHEN c.type IN ('shell', 'screenshot', 'ping', 'checkin_now', 'query', 'get_app_inventory')
					THEN INTERVAL '5 minutes'
				ELSE INTERVAL '24 hours'
			END
		RETURNING cs.command_id, cs.device_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StalledInstall
	for rows.Next() {
		var s StalledInstall
		if err := rows.Scan(&s.CommandID, &s.DeviceID); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// LearnApkPackage records that apkURL installs packageName, so future installs of
// that APK can be reconciled against a device's reported package list.
func (d *DB) LearnApkPackage(ctx context.Context, apkURL, packageName string) error {
	if apkURL == "" || packageName == "" {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO apk_packages (apk_url, package_name, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (apk_url) DO UPDATE SET package_name = EXCLUDED.package_name, updated_at = NOW()
	`, apkURL, packageName)
	return err
}

// ReconcileInstalledCommands marks as 'installed' any in-flight install_apk command
// targeting this device whose APK's known package is already present in the device's
// reported packages. Recovers from a lost terminal ack: the "installing" row clears
// as soon as the device reports the app present. Returns the affected command IDs so
// the caller can publish live updates.
func (d *DB) ReconcileInstalledCommands(ctx context.Context, deviceID uuid.UUID) ([]uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `
		WITH affected AS (
			SELECT c.id
			FROM commands c
			-- Resolve the command's package from EITHER the learned apk_url->package map
			-- (apk_packages) OR the app-library row (apps.package_name, known at upload
			-- time). The library path matters for S3/library apps whose apk_packages entry
			-- may not be learned yet — without it a genuinely-installed library app (e.g.
			-- an install whose terminal ack was lost on a half-open WS) never reconciles.
			JOIN device_packages dp ON dp.device_id = $1 AND dp.package_name IN (
				SELECT ap.package_name FROM apk_packages ap WHERE ap.apk_url = c.apk_url
				UNION
				SELECT a.package_name FROM apps a WHERE a.apk_url = c.apk_url AND COALESCE(a.package_name,'') <> ''
			)
			WHERE c.type = 'install_apk'
			  AND (
				c.target_type = 'all'
				OR (c.target_type = 'devices' AND EXISTS (
					SELECT 1 FROM command_targets ct WHERE ct.command_id = c.id AND ct.target_id = $1))
				OR (c.target_type = 'groups' AND EXISTS (
					SELECT 1 FROM command_targets ct JOIN device_groups dg ON dg.group_id = ct.target_id
					WHERE ct.command_id = c.id AND dg.device_id = $1))
			  )
			  -- Terminal-correct states ('installed','completed') are left alone.
			  AND NOT EXISTS (
				SELECT 1 FROM command_status cs WHERE cs.command_id = c.id AND cs.device_id = $1
				  AND cs.status IN ('installed','completed'))
			  -- Only recover a LOST-ACK install (the stalled-install sweep failed it, but the
			  -- device never reported anything). A command_results row means the DEVICE
			  -- reported a real outcome — most importantly a genuine failure (downgrade,
			  -- signature mismatch, corrupt APK). Never flip such a device-reported failure
			  -- to 'installed' just because the device still has an OLDER version of the
			  -- package present; that would be a false success. Sweep-failed installs have no
			  -- result row, so they still reconcile.
			  AND NOT EXISTS (
				SELECT 1 FROM command_results cr WHERE cr.command_id = c.id AND cr.device_id = $1)
		), up AS (
			INSERT INTO command_status (command_id, device_id, status, progress, updated_at)
			SELECT id, $1, 'installed', NULL, NOW() FROM affected
			ON CONFLICT (command_id, device_id) DO UPDATE SET status = 'installed', progress = NULL, updated_at = NOW()
			RETURNING command_id
		)
		SELECT command_id FROM up
	`, deviceID)
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

// GetApkPackageMap returns the learned apk_url → package_name map for the given APK
// URLs (empty urls => whole table). Used to filter already-installed apps out of the
// pending-install rows at render time.
func (d *DB) GetApkPackageMap(ctx context.Context, apkURLs []string) (map[string]string, error) {
	out := make(map[string]string)
	var rows pgx.Rows
	var err error
	if len(apkURLs) == 0 {
		rows, err = d.pool.Query(ctx, `SELECT apk_url, package_name FROM apk_packages`)
	} else {
		rows, err = d.pool.Query(ctx, `SELECT apk_url, package_name FROM apk_packages WHERE apk_url = ANY($1)`, apkURLs)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var u, p string
		if err := rows.Scan(&u, &p); err != nil {
			return nil, err
		}
		out[u] = p
	}
	return out, rows.Err()
}

type DeviceCommand struct {
	ID         uuid.UUID       `json:"id"`
	Type       string          `json:"type"`
	ApkURL     string          `json:"apk_url"`
	Payload    json.RawMessage `json:"payload"`
	TargetType string          `json:"target_type"`
	CreatedBy  string          `json:"created_by"`
	CreatedAt  time.Time       `json:"created_at"`
	Status     string          `json:"status"`
	UpdatedAt  time.Time       `json:"updated_at"`
	Output     string          `json:"output"`
	Progress   *int            `json:"progress,omitempty"` // 0-100 while downloading, nil otherwise
}

// GetDeviceCommands returns all commands targeting a device with their status.
func (d *DB) GetDeviceCommands(ctx context.Context, deviceID uuid.UUID, expirySec int) ([]DeviceCommand, error) {
	if expirySec <= 0 {
		expirySec = 300
	}
	// install_apk gets a longer leash than shell/screenshot/reboot (a large APK can
	// take minutes to download + install), but a delivered/pending install that
	// hasn't acked within this window is dead — expire it so the device page stops
	// showing it as "installing" forever and duplicate clicks don't pile up.
	installExpiry := expirySec * 3
	if installExpiry < 900 {
		installExpiry = 900
	}
	rows, err := d.pool.Query(ctx, fmt.Sprintf(`
		SELECT c.id, c.type, c.apk_url, c.payload, c.target_type, c.created_by, c.created_at,
		       CASE
		         -- Expire an install by LAST ACTIVITY (updated_at), not creation time, so a
		         -- large APK that legitimately takes a while to download isn't shown 'expired'
		         -- while it's actively progressing. This matches ExpireStalledInstalls, which
		         -- keys the actual write on updated_at (the device page previously disagreed
		         -- with the sweep and could prompt an operator to cancel a healthy install).
		         WHEN c.type = 'install_apk'
		              AND COALESCE(cs.status, 'pending') IN ('pending', 'delivered', 'downloading', 'installing')
		              AND COALESCE(cs.updated_at, c.created_at) <= NOW() - INTERVAL '%d seconds' THEN 'expired'
		         WHEN cs.status IS NOT NULL THEN cs.status
		         WHEN c.type IN ('shell', 'screenshot', 'reboot')
		              AND c.created_at <= NOW() - INTERVAL '%d seconds' THEN 'expired'
		         ELSE 'pending'
		       END AS status,
		       COALESCE(cs.updated_at, c.created_at) AS updated_at,
		       COALESCE(cr.output, '') AS output,
		       cs.progress
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
		-- Boot logo is a config action managed on /boot-logo, not part of the
		-- device's command history.
		AND c.type != 'update_splash'
		ORDER BY c.created_at DESC
	`, installExpiry, expirySec), deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DeviceCommand
	for rows.Next() {
		var dc DeviceCommand
		if err := rows.Scan(&dc.ID, &dc.Type, &dc.ApkURL, &dc.Payload, &dc.TargetType, &dc.CreatedBy, &dc.CreatedAt, &dc.Status, &dc.UpdatedAt, &dc.Output, &dc.Progress); err != nil {
			return nil, err
		}
		out = append(out, dc)
	}
	return out, rows.Err()
}

// GetDeviceQueue returns a device's QUEUE — its non-terminal commands, oldest-first
// (FIFO). Raw status with no display-expiry (the queue does not expire): 'pending' (not
// yet sent), 'delivered' (sent, awaiting the device), 'downloading', 'installing'.
// update_splash (boot logo) is excluded, matching GetDeviceCommands.
func (d *DB) GetDeviceQueue(ctx context.Context, deviceID uuid.UUID) ([]DeviceCommand, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT c.id, c.type, c.apk_url, c.payload, c.target_type, c.created_by, c.created_at,
		       COALESCE(cs.status, 'pending') AS status,
		       COALESCE(cs.updated_at, c.created_at) AS updated_at,
		       '' AS output, cs.progress
		FROM commands c
		LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = $1
		WHERE (
			c.target_type = 'all'
			OR (c.target_type = 'devices' AND EXISTS (
				SELECT 1 FROM command_targets ct WHERE ct.command_id = c.id AND ct.target_id = $1))
			OR (c.target_type = 'groups' AND EXISTS (
				SELECT 1 FROM command_targets ct JOIN device_groups dg ON dg.group_id = ct.target_id
				WHERE ct.command_id = c.id AND dg.device_id = $1))
		)
		AND c.type != 'update_splash'
		AND (
			COALESCE(cs.status, 'pending') IN ('pending', 'delivered', 'downloading', 'installing')
			-- A command that just reached a terminal state lingers here for 5 minutes
			-- instead of vanishing the instant it finishes — so the operator actually
			-- sees "Done"/"Failed" rather than the row disappearing out from under them.
			OR (COALESCE(cs.status, 'pending') IN ('installed', 'completed', 'failed', 'cancelled')
				AND cs.updated_at > NOW() - INTERVAL '5 minutes')
		)
		-- Only show commands that will actually run now — the freshly queued ones. Old stuck
		-- commands (a long-ago delivered/pending that the device never acted on) are not shown
		-- as "queued". An actively downloading/installing command always shows, whatever its
		-- age, since it IS running right now.
		AND (
			c.created_at > NOW() - INTERVAL '1 hour'
			OR COALESCE(cs.status, 'pending') IN ('downloading', 'installing', 'installed', 'completed', 'failed', 'cancelled')
		)
		ORDER BY c.created_at ASC
	`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceCommand
	for rows.Next() {
		var dc DeviceCommand
		if err := rows.Scan(&dc.ID, &dc.Type, &dc.ApkURL, &dc.Payload, &dc.TargetType, &dc.CreatedBy, &dc.CreatedAt, &dc.Status, &dc.UpdatedAt, &dc.Output, &dc.Progress); err != nil {
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
	installExpiry := expirySec * 3
	if installExpiry < 900 {
		installExpiry = 900
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
		         WHEN c.type = 'install_apk'
		              AND COALESCE(cs.status, 'pending') IN ('pending', 'delivered')
		              AND c.created_at <= NOW() - INTERVAL '%d seconds'
		           THEN 'expired'
		         ELSE COALESCE(cs.status, 'pending')
		       END AS status,
		       cs.progress,
		       COALESCE(cs.updated_at, c.created_at) AS updated_at,
		       COALESCE(cr.output, '') AS output,
		       d.last_seen_at
		FROM target_devices td
		JOIN devices d ON d.id = td.device_id
		JOIN commands c ON c.id = $1
		LEFT JOIN command_status cs ON cs.command_id = $1 AND cs.device_id = td.device_id
		LEFT JOIN command_results cr ON cr.command_id = $1 AND cr.device_id = td.device_id
		ORDER BY COALESCE(cs.updated_at, c.created_at) DESC, d.serial_number
	`, expirySec, installExpiry), commandID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []CommandDelivery
	for rows.Next() {
		var cd CommandDelivery
		if err := rows.Scan(&cd.DeviceID, &cd.SerialNumber, &cd.Status, &cd.Progress, &cd.UpdatedAt, &cd.Output, &cd.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, cd)
	}
	return out, rows.Err()
}

// ── Apps ──────────────────────────────────────────────────────────────────────

type App struct {
	ID          uuid.UUID `json:"id"`
	Name        string    `json:"name"`
	ApkURL      string    `json:"apk_url"`
	PackageName string    `json:"package_name"`
	VersionName string    `json:"version_name,omitempty"` // parsed from the APK manifest at upload, "" if unknown
	S3Key       string    `json:"s3_key,omitempty"`       // set for S3-hosted uploads; empty for URL apps
	Icon        string    `json:"icon,omitempty"`         // resolved from app_icons on the read path
	CreatedAt   time.Time `json:"created_at"`
}

func (d *DB) ListApps(ctx context.Context) ([]App, error) {
	// Resolve each app's icon from the shared app_icons index (populated when an APK is
	// uploaded and parsed) so the library can render real icons.
	rows, err := d.pool.Query(ctx, `
		SELECT a.id, a.name, a.apk_url, a.package_name, a.version_name, a.created_at, COALESCE(ai.icon, '')
		FROM apps a
		LEFT JOIN app_icons ai ON ai.package_name = a.package_name
		ORDER BY a.name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []App
	for rows.Next() {
		var a App
		if err := rows.Scan(&a.ID, &a.Name, &a.ApkURL, &a.PackageName, &a.VersionName, &a.CreatedAt, &a.Icon); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

func (d *DB) GetApp(ctx context.Context, id uuid.UUID) (*App, error) {
	var a App
	err := d.pool.QueryRow(ctx,
		`SELECT id, name, apk_url, package_name, version_name, COALESCE(s3_key, ''), created_at FROM apps WHERE id = $1`, id,
	).Scan(&a.ID, &a.Name, &a.ApkURL, &a.PackageName, &a.VersionName, &a.S3Key, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// CreateS3App inserts a repository app backed by an S3 object. The apk_url is the
// server's own stable proxy (baseURL + /apps/{id}/apk) that 302-redirects to a fresh
// presigned download URL, so the bucket stays private and the URL never expires. The
// device fetches apk_url directly, so baseURL must be the device-reachable origin.
func (d *DB) CreateS3App(ctx context.Context, name, packageName, versionName, s3Key, baseURL string) (*App, error) {
	id := uuid.New()
	apkURL := fmt.Sprintf("%s/apps/%s/apk", strings.TrimRight(baseURL, "/"), id)
	var a App
	err := d.pool.QueryRow(ctx,
		`INSERT INTO apps (id, name, apk_url, package_name, version_name, s3_key) VALUES ($1, $2, $3, $4, $5, $6)
		 RETURNING id, name, apk_url, package_name, version_name, COALESCE(s3_key, ''), created_at`,
		id, name, apkURL, packageName, versionName, s3Key,
	).Scan(&a.ID, &a.Name, &a.ApkURL, &a.PackageName, &a.VersionName, &a.S3Key, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &a, nil
}

// GetAppIconsByPackages resolves base64 PNG icons for a set of package names from the
// shared app_icons index in one query, for pages (e.g. Alerts) that show several
// crash/ANR cards at once and want each app's real icon instead of a generic glyph.
// Packages with no known icon are simply absent from the returned map.
func (d *DB) GetAppIconsByPackages(ctx context.Context, packages []string) (map[string]string, error) {
	out := map[string]string{}
	if len(packages) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx,
		`SELECT package_name, icon FROM app_icons WHERE package_name = ANY($1)`, packages)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var pkg, icon string
		if err := rows.Scan(&pkg, &icon); err != nil {
			return nil, err
		}
		out[pkg] = icon
	}
	return out, rows.Err()
}

// UpsertAppIcon stores one package's launcher icon in the shared app_icons index, so
// it shows across all devices immediately (same index the device-reported icons use).
func (d *DB) UpsertAppIcon(ctx context.Context, packageName, iconB64 string) error {
	if packageName == "" || iconB64 == "" {
		return nil
	}
	_, err := d.pool.Exec(ctx,
		`INSERT INTO app_icons (package_name, icon) VALUES ($1, $2)
		 ON CONFLICT (package_name) DO UPDATE SET icon = EXCLUDED.icon, updated_at = NOW()`,
		packageName, iconB64)
	return err
}

func (d *DB) CreateApp(ctx context.Context, name, apkURL, packageName string) (*App, error) {
	var a App
	err := d.pool.QueryRow(ctx,
		`INSERT INTO apps (name, apk_url, package_name) VALUES ($1, $2, $3) RETURNING id, name, apk_url, package_name, created_at`,
		name, apkURL, packageName,
	).Scan(&a.ID, &a.Name, &a.ApkURL, &a.PackageName, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	// A known package lets installs dedup against already-installed devices from
	// the start, rather than waiting for a first install to learn the mapping.
	if packageName != "" {
		_ = d.LearnApkPackage(ctx, apkURL, packageName)
	}
	return &a, err
}

// UpdateApp edits an app's display name, APK URL and package name. When a package
// name is set it re-seeds the apk_url → package map so dedup stays accurate.
func (d *DB) UpdateApp(ctx context.Context, id uuid.UUID, name, apkURL, packageName string) (*App, error) {
	var a App
	err := d.pool.QueryRow(ctx,
		`UPDATE apps SET name = $2, apk_url = $3, package_name = $4 WHERE id = $1
		 RETURNING id, name, apk_url, package_name, created_at`,
		id, name, apkURL, packageName,
	).Scan(&a.ID, &a.Name, &a.ApkURL, &a.PackageName, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	if packageName != "" {
		_ = d.LearnApkPackage(ctx, apkURL, packageName)
	}
	return &a, nil
}

func (d *DB) DeleteApp(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM apps WHERE id = $1`, id)
	return err
}

// DevicesWithPackageInstalled returns which of deviceIDs already report the app's
// package present. The package is resolved from the learned apk_url → package map
// (seeded when an app has a package name, or learned from a prior install), so an
// install can skip devices that already have the app instead of re-downloading it.
func (d *DB) DevicesWithPackageInstalled(ctx context.Context, apkURL string, deviceIDs []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := make(map[uuid.UUID]bool)
	if apkURL == "" || len(deviceIDs) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT dp.device_id
		FROM apk_packages ap
		JOIN device_packages dp ON dp.package_name = ap.package_name
		WHERE ap.apk_url = $1 AND dp.device_id = ANY($2)
	`, apkURL, deviceIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
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

// ── Device Packages ───────────────────────────────────────────────────────────

type DevicePackage struct {
	PackageName string `json:"package_name"`
	AppName     string `json:"app_name"`
	VersionName string `json:"version_name"`
	// IsSystem is the client-reported system flag: nil when the device's client is
	// too old to report it (so the admin override decides). Used on the write path.
	IsSystem *bool `json:"is_system,omitempty"`
	// EffectiveSystem is the resolved classification (client report if present, else
	// admin override, else false). Populated on the read path; gates uninstall.
	EffectiveSystem bool      `json:"effective_system"`
	UpdatedAt       time.Time `json:"updated_at"`
	// Icon is a base64-encoded PNG launcher icon. On the write path it's the value the
	// device reported (may be empty); on the read path it's resolved from the shared
	// app_icons index keyed by package_name, so any device — even an old client that
	// never reports icons — shows an icon once ANY device has reported one for it.
	Icon string `json:"icon,omitempty"`
}

// AdminPackage is a fleet-wide package row for the admin classification page.
type AdminPackage struct {
	PackageName     string `json:"package_name"`
	AppName         string `json:"app_name"`
	DeviceCount     int    `json:"device_count"`
	ReportedSystem  int    `json:"reported_system"`  // devices whose client reported is_system=true
	ReportedUser    int    `json:"reported_user"`    // devices whose client reported is_system=false
	ReportedUnknown int    `json:"reported_unknown"` // devices whose client didn't report
	AdminFlagged    bool   `json:"admin_flagged"`    // an admin override row exists
	EffectiveSystem bool   `json:"effective_system"`
}

type FleetPackage struct {
	PackageName string `json:"package_name"`
	AppName     string `json:"app_name"`
	DeviceCount int    `json:"device_count"`
	Versions    string `json:"versions"`
	Icon        string `json:"icon"` // base64 PNG from the shared app_icons index, "" if none
}

// UpsertDevicePackages replaces all packages for a device atomically.
// devicePackagesHash fingerprints the (order-independent) installed-app set so an
// unchanged list can be detected cheaply.
func devicePackagesHash(packages []DevicePackage) string {
	sorted := make([]DevicePackage, len(packages))
	copy(sorted, packages)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].PackageName < sorted[j].PackageName })
	h := fnv.New64a()
	for _, p := range sorted {
		sys := "n"
		if p.IsSystem != nil {
			if *p.IsSystem {
				sys = "1"
			} else {
				sys = "0"
			}
		}
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\n", p.PackageName, p.AppName, p.VersionName, sys)
	}
	return strconv.FormatUint(h.Sum64(), 16)
}

func (d *DB) UpsertDevicePackages(ctx context.Context, deviceID uuid.UUID, packages []DevicePackage) error {
	// The installed-app list is re-sent on every check-in but changes very rarely.
	// A full delete+reinsert of ~200 rows per check-in was ~90% of all check-in DB
	// time (and a huge autovacuum generator). Skip it when the set is unchanged: a
	// single-column PK lookup replaces the whole rewrite on >99% of check-ins.
	newHash := devicePackagesHash(packages)
	var curHash string
	if err := d.pool.QueryRow(ctx, `SELECT COALESCE(packages_hash, '') FROM devices WHERE id = $1`, deviceID).Scan(&curHash); err == nil {
		if curHash == newHash {
			return nil
		}
	}

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
		systems := make([]*bool, len(packages))
		for i, p := range packages {
			names[i] = p.PackageName
			appNames[i] = p.AppName
			versions[i] = p.VersionName
			systems[i] = p.IsSystem // *bool → NULL when the client didn't report
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO device_packages (device_id, package_name, app_name, version_name, is_system)
			SELECT $1, unnest($2::text[]), unnest($3::text[]), unnest($4::text[]), unnest($5::bool[])
			ON CONFLICT (device_id, package_name) DO UPDATE SET app_name = EXCLUDED.app_name, version_name = EXCLUDED.version_name, is_system = EXCLUDED.is_system, updated_at = NOW()
		`, deviceID, names, appNames, versions, systems); err != nil {
			return err
		}

		// Populate the shared package→icon index from whatever icons this device sent.
		// Non-empty only; keyed by package_name so all devices benefit. Last write wins.
		var iconPkgs, iconVals []string
		for _, p := range packages {
			if p.Icon != "" {
				iconPkgs = append(iconPkgs, p.PackageName)
				iconVals = append(iconVals, p.Icon)
			}
		}
		if len(iconPkgs) > 0 {
			if _, err := tx.Exec(ctx, `
				INSERT INTO app_icons (package_name, icon)
				SELECT unnest($1::text[]), unnest($2::text[])
				ON CONFLICT (package_name) DO UPDATE SET icon = EXCLUDED.icon, updated_at = NOW()
			`, iconPkgs, iconVals); err != nil {
				return err
			}
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE devices SET packages_hash = $2 WHERE id = $1`, deviceID, newHash); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

func (d *DB) GetDevicePackages(ctx context.Context, deviceID uuid.UUID) ([]DevicePackage, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT dp.package_name, dp.app_name, dp.version_name,
		       (COALESCE(dp.is_system, true) OR ov.package_name IS NOT NULL) AS effective_system,
		       dp.updated_at, COALESCE(ai.icon, '')
		FROM device_packages dp
		LEFT JOIN app_system_overrides ov ON ov.package_name = dp.package_name
		-- Icons come from the shared index, so a device shows an icon for a package even
		-- if this particular device never reported one.
		LEFT JOIN app_icons ai ON ai.package_name = dp.package_name
		WHERE dp.device_id = $1
		-- User apps first, then system apps; alphabetical within each group.
		ORDER BY effective_system, COALESCE(NULLIF(dp.app_name, ''), dp.package_name), dp.package_name
	`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []DevicePackage
	for rows.Next() {
		var p DevicePackage
		if err := rows.Scan(&p.PackageName, &p.AppName, &p.VersionName, &p.EffectiveSystem, &p.UpdatedAt, &p.Icon); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// notSystemHeuristicSQL further excludes obvious AOSP/vendor system packages from
// the uninstall picker. Unknown packages (is_system NULL, e.g. an old client that
// didn't report the flag) already default to system and are treated as
// non-uninstallable; this name filter is a secondary belt against vendor packages
// that are somehow reported as user. Admins can still fine-tune via app_system_overrides.
const notSystemHeuristicSQL = `
			  AND dp.package_name <> 'android'
			  AND dp.package_name NOT LIKE 'android.%'
			  AND dp.package_name NOT LIKE 'com.android.%'
			  AND dp.package_name NOT LIKE 'com.google.android.%'
			  AND dp.package_name NOT LIKE 'com.qualcomm.%'
			  AND dp.package_name NOT LIKE 'com.qti.%'
			  AND dp.package_name NOT LIKE 'com.mediatek.%'
			  AND dp.package_name NOT LIKE 'org.chromium.%'`

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
				string_agg(DISTINCT dp.version_name, ', ' ORDER BY dp.version_name) AS versions,
				COALESCE(MAX(ai.icon), '') AS icon
			FROM device_packages dp
			LEFT JOIN app_system_overrides ov ON ov.package_name = dp.package_name
			LEFT JOIN app_icons ai ON ai.package_name = dp.package_name
			WHERE (dp.package_name ILIKE $1 OR dp.app_name ILIKE $1)
			  AND NOT (COALESCE(dp.is_system, true) OR ov.package_name IS NOT NULL)`+notSystemHeuristicSQL+`
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
				string_agg(DISTINCT dp.version_name, ', ' ORDER BY dp.version_name) AS versions,
				COALESCE(MAX(ai.icon), '') AS icon
			FROM device_packages dp
			LEFT JOIN app_system_overrides ov ON ov.package_name = dp.package_name
			LEFT JOIN app_icons ai ON ai.package_name = dp.package_name
			WHERE NOT (COALESCE(dp.is_system, true) OR ov.package_name IS NOT NULL)`+notSystemHeuristicSQL+`
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
		if err := rows.Scan(&p.PackageName, &p.AppName, &p.DeviceCount, &p.Versions, &p.Icon); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// KioskAppsForDevices returns the distinct non-system packages installed across a
// specific set of devices, each with the count of those devices that have it. The
// bulk-kiosk picker uses device_count vs len(deviceIDs) to mark an app as common to
// all selected devices or only present on some.
// PackagesForDevices returns the uninstallable (non-system) packages present on the
// given device set, with per-package device counts — the target-scoped variant of
// SearchFleetPackages that backs the Actions uninstall picker once a target is chosen,
// so the list shows only apps actually installed on the devices that would receive the
// command. Applies the same system/override/heuristic filtering as the fleet picker.
func (d *DB) PackagesForDevices(ctx context.Context, deviceIDs []uuid.UUID) ([]FleetPackage, error) {
	if len(deviceIDs) == 0 {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT
			dp.package_name,
			COALESCE(MAX(dp.app_name), '') AS app_name,
			COUNT(DISTINCT dp.device_id) AS device_count,
			string_agg(DISTINCT dp.version_name, ', ' ORDER BY dp.version_name) AS versions,
			COALESCE(MAX(ai.icon), '') AS icon
		FROM device_packages dp
		LEFT JOIN app_system_overrides ov ON ov.package_name = dp.package_name
		LEFT JOIN app_icons ai ON ai.package_name = dp.package_name
		WHERE dp.device_id = ANY($1)
		  AND NOT (COALESCE(dp.is_system, true) OR ov.package_name IS NOT NULL)`+notSystemHeuristicSQL+`
		GROUP BY dp.package_name
		ORDER BY device_count DESC, dp.package_name
		LIMIT 200
	`, deviceIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FleetPackage
	for rows.Next() {
		var p FleetPackage
		if err := rows.Scan(&p.PackageName, &p.AppName, &p.DeviceCount, &p.Versions, &p.Icon); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *DB) KioskAppsForDevices(ctx context.Context, deviceIDs []uuid.UUID) ([]FleetPackage, error) {
	if len(deviceIDs) == 0 {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT
			dp.package_name,
			COALESCE(MAX(dp.app_name), '') AS app_name,
			COUNT(DISTINCT dp.device_id) AS device_count
		FROM device_packages dp
		LEFT JOIN app_system_overrides ov ON ov.package_name = dp.package_name
		WHERE dp.device_id = ANY($1)
		  AND NOT (COALESCE(dp.is_system, true) OR ov.package_name IS NOT NULL)
		GROUP BY dp.package_name
		ORDER BY device_count DESC, app_name, dp.package_name
	`, deviceIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FleetPackage
	for rows.Next() {
		var p FleetPackage
		if err := rows.Scan(&p.PackageName, &p.AppName, &p.DeviceCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListPackagesAdmin returns every distinct package across the fleet with how each
// device's client classified it (system / user / not-reported) and whether an admin
// override exists — backing the admin app-classification page. Optional name filter.
func (d *DB) ListPackagesAdmin(ctx context.Context, query string) ([]AdminPackage, error) {
	base := `
		SELECT
			dp.package_name,
			COALESCE(MAX(dp.app_name), '') AS app_name,
			COUNT(DISTINCT dp.device_id) AS device_count,
			COUNT(*) FILTER (WHERE dp.is_system IS TRUE)  AS rep_system,
			COUNT(*) FILTER (WHERE dp.is_system IS FALSE) AS rep_user,
			COUNT(*) FILTER (WHERE dp.is_system IS NULL)  AS rep_unknown,
			bool_or(ov.package_name IS NOT NULL) AS admin_flagged
		FROM device_packages dp
		LEFT JOIN app_system_overrides ov ON ov.package_name = dp.package_name`
	var rows pgx.Rows
	var err error
	if query != "" {
		base += `
		WHERE dp.package_name ILIKE $1 OR dp.app_name ILIKE $1
		GROUP BY dp.package_name
		ORDER BY dp.package_name
		LIMIT 1000`
		rows, err = d.pool.Query(ctx, base, "%"+query+"%")
	} else {
		base += `
		GROUP BY dp.package_name
		ORDER BY dp.package_name
		LIMIT 1000`
		rows, err = d.pool.Query(ctx, base)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AdminPackage
	for rows.Next() {
		var p AdminPackage
		if err := rows.Scan(&p.PackageName, &p.AppName, &p.DeviceCount,
			&p.ReportedSystem, &p.ReportedUser, &p.ReportedUnknown, &p.AdminFlagged); err != nil {
			return nil, err
		}
		// Effective classification. The admin override forces system and overrides
		// whatever devices reported (matching the consumer query
		// `COALESCE(is_system,true) OR override`). Otherwise a client system report
		// wins, then a client user report, then unknown defaults to system.
		switch {
		case p.AdminFlagged || p.ReportedSystem > 0:
			p.EffectiveSystem = true
		case p.ReportedUser > 0:
			p.EffectiveSystem = false
		default:
			p.EffectiveSystem = true // all-unknown → treated as system (COALESCE default)
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPackageSystemOverride adds (flagged) or removes (unflagged) an admin
// system-app override for a package name.
func (d *DB) SetPackageSystemOverride(ctx context.Context, pkg string, flagged bool) error {
	if flagged {
		_, err := d.pool.Exec(ctx,
			`INSERT INTO app_system_overrides (package_name) VALUES ($1) ON CONFLICT DO NOTHING`, pkg)
		return err
	}
	_, err := d.pool.Exec(ctx, `DELETE FROM app_system_overrides WHERE package_name = $1`, pkg)
	return err
}

// ── Device Config / Kiosk ─────────────────────────────────────────────────────

// GetDeviceConfigUpdatedAtMap returns device_id -> updated_at for every device with a
// config row, so the Manage page can show "last changed" per device without an N+1.
// Not kiosk-specific — device_config's updated_at bumps on any field in that row
// (kiosk, WLC charging, offline-unlock rotation) — so it's a config-touched proxy,
// not a guarantee the kiosk fields themselves were what last changed.
func (d *DB) GetDeviceConfigUpdatedAtMap(ctx context.Context) (map[uuid.UUID]time.Time, error) {
	rows, err := d.pool.Query(ctx, `SELECT device_id, updated_at FROM device_config`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]time.Time)
	for rows.Next() {
		var id uuid.UUID
		var t time.Time
		if err := rows.Scan(&id, &t); err != nil {
			return nil, err
		}
		out[id] = t
	}
	return out, rows.Err()
}

func (d *DB) GetOrCreateDeviceConfig(ctx context.Context, deviceID uuid.UUID) (*DeviceConfig, error) {
	// Read first — this runs on every check-in and the row exists after the device's
	// first one, so the INSERT below (a wasted write attempt at 900 dev × every 60s)
	// only ever fires once per device.
	const sel = `SELECT device_id, kiosk_enabled, kiosk_package, kiosk_features,
		offline_exit_enabled, offline_exit_seed, offline_exit_relock, wlc_charging_enabled, updated_at
		FROM device_config WHERE device_id = $1`
	var cfg DeviceConfig
	scan := func(row pgx.Row) error {
		return row.Scan(&cfg.DeviceID, &cfg.KioskEnabled, &cfg.KioskPackage, &cfg.KioskFeatures,
			&cfg.OfflineExitEnabled, &cfg.OfflineExitSeed, &cfg.OfflineExitRelock, &cfg.WlcChargingEnabled, &cfg.UpdatedAt)
	}
	err := scan(d.pool.QueryRow(ctx, sel, deviceID))
	if err == nil {
		d.ensureOfflineExitSeed(ctx, &cfg)
		return &cfg, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return nil, err
	}
	if _, err := d.pool.Exec(ctx, `INSERT INTO device_config (device_id) VALUES ($1) ON CONFLICT (device_id) DO NOTHING`, deviceID); err != nil {
		return nil, err
	}
	if err := scan(d.pool.QueryRow(ctx, sel, deviceID)); err != nil {
		return nil, err
	}
	d.ensureOfflineExitSeed(ctx, &cfg)
	return &cfg, nil
}

// ensureOfflineExitSeed lazily provisions a TOTP seed the first time a device is read.
// Offline exit is part of kiosk (not a separate opt-in), so every device gets a seed;
// the code is only ever usable while the device is in kiosk. Updates cfg in place.
func (d *DB) ensureOfflineExitSeed(ctx context.Context, cfg *DeviceConfig) {
	if cfg.OfflineExitSeed != "" {
		return
	}
	seed := newBase32Seed()
	if _, err := d.pool.Exec(ctx,
		`UPDATE device_config SET offline_exit_seed=$2 WHERE device_id=$1 AND offline_exit_seed=''`,
		cfg.DeviceID, seed); err == nil {
		cfg.OfflineExitSeed = seed
	}
}

// RecordOfflineExit logs a device_events row when a device reports it was taken out of
// kiosk mode offline. Idempotent per (device, kind, occurred_at) so repeated reports
// (the client resends until acked) don't create duplicates.
func (d *DB) RecordOfflineExit(ctx context.Context, deviceID uuid.UUID, atEpoch int64) {
	_, _ = d.pool.Exec(ctx, `
		INSERT INTO device_events (device_id, kind, summary, occurred_at)
		SELECT $1, 'kiosk_exit_offline', 'Kiosk exited offline (unlock code)', to_timestamp($2)
		WHERE NOT EXISTS (
			SELECT 1 FROM device_events
			WHERE device_id = $1 AND kind = 'kiosk_exit_offline' AND occurred_at = to_timestamp($2))`,
		deviceID, atEpoch)
}

// newBase32Seed returns a 20-byte (160-bit) random TOTP seed, RFC 4648 base32
// (no padding), matching the on-device Totp verifier.
func newBase32Seed() string {
	b := make([]byte, 20)
	_, _ = rand.Read(b)
	return strings.TrimRight(base32.StdEncoding.EncodeToString(b), "=")
}

// SetOfflineExit enables/disables offline kiosk exit for a device. On the first
// enable it generates a random base32 TOTP seed; disabling keeps the seed so a
// re-enable reuses it (rotate by passing rotate=true).
func (d *DB) SetOfflineExit(ctx context.Context, deviceID uuid.UUID, enabled bool, relock string, rotate bool) (string, error) {
	if relock == "" {
		relock = "reboot"
	}
	// Ensure the row exists.
	if _, err := d.pool.Exec(ctx, `INSERT INTO device_config (device_id) VALUES ($1) ON CONFLICT (device_id) DO NOTHING`, deviceID); err != nil {
		return "", err
	}
	var seed string
	if err := d.pool.QueryRow(ctx, `SELECT offline_exit_seed FROM device_config WHERE device_id=$1`, deviceID).Scan(&seed); err != nil {
		return "", err
	}
	if (enabled && seed == "") || rotate {
		seed = newBase32Seed()
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE device_config
		   SET offline_exit_enabled=$2, offline_exit_seed=$3, offline_exit_relock=$4, updated_at=NOW()
		 WHERE device_id=$1`, deviceID, enabled, seed, relock)
	return seed, err
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

// SetWlcCharging sets the wireless-charging enable flag for a device.
func (d *DB) SetWlcCharging(ctx context.Context, deviceID uuid.UUID, enabled bool) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_config (device_id, wlc_charging_enabled, updated_at)
		VALUES ($1, $2, NOW())
		ON CONFLICT (device_id) DO UPDATE
			SET wlc_charging_enabled = EXCLUDED.wlc_charging_enabled,
			    updated_at           = NOW()
	`, deviceID, enabled)
	return err
}

// CountUnlockedDevices returns the number of non-hidden devices with no kiosk lock
// applied (either no device_config row at all, or one with kiosk_enabled=false) —
// the Manage page's "Default" bucket.
func (d *DB) CountUnlockedDevices(ctx context.Context) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM devices d
		LEFT JOIN device_config dc ON dc.device_id = d.id
		WHERE NOT d.hidden AND COALESCE(dc.kiosk_enabled, false) = false
	`).Scan(&n)
	return n, err
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

// KioskPolicy is a named, standing kiosk-lock policy (see the kiosk_policies table
// comment). TargetID is nil for target_type "all" or "device".
type KioskPolicy struct {
	ID           uuid.UUID  `json:"id"`
	Name         string     `json:"name"`
	KioskPackage string     `json:"kiosk_package"`
	TargetType   string     `json:"target_type"`
	TargetID     *uuid.UUID `json:"target_id,omitempty"`
	TargetSerial string     `json:"target_serial,omitempty"`
	CreatedAt    time.Time  `json:"created_at"`
	UpdatedAt    time.Time  `json:"updated_at"`
}

func (d *DB) ListKioskPolicies(ctx context.Context) ([]KioskPolicy, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, name, kiosk_package, target_type, target_id, target_serial, created_at, updated_at
		FROM kiosk_policies ORDER BY name ASC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []KioskPolicy
	for rows.Next() {
		var p KioskPolicy
		if err := rows.Scan(&p.ID, &p.Name, &p.KioskPackage, &p.TargetType, &p.TargetID, &p.TargetSerial, &p.CreatedAt, &p.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

func (d *DB) GetKioskPolicy(ctx context.Context, id uuid.UUID) (KioskPolicy, error) {
	var p KioskPolicy
	err := d.pool.QueryRow(ctx, `
		SELECT id, name, kiosk_package, target_type, target_id, target_serial, created_at, updated_at
		FROM kiosk_policies WHERE id = $1
	`, id).Scan(&p.ID, &p.Name, &p.KioskPackage, &p.TargetType, &p.TargetID, &p.TargetSerial, &p.CreatedAt, &p.UpdatedAt)
	return p, err
}

func (d *DB) CreateKioskPolicy(ctx context.Context, name, pkg, targetType string, targetID *uuid.UUID, targetSerial string) (uuid.UUID, error) {
	var id uuid.UUID
	err := d.pool.QueryRow(ctx, `
		INSERT INTO kiosk_policies (name, kiosk_package, target_type, target_id, target_serial)
		VALUES ($1, $2, $3, $4, $5) RETURNING id
	`, name, pkg, targetType, targetID, targetSerial).Scan(&id)
	return id, err
}

func (d *DB) UpdateKioskPolicy(ctx context.Context, id uuid.UUID, name, pkg, targetType string, targetID *uuid.UUID, targetSerial string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE kiosk_policies SET name = $2, kiosk_package = $3, target_type = $4, target_id = $5, target_serial = $6, updated_at = NOW()
		WHERE id = $1
	`, id, name, pkg, targetType, targetID, targetSerial)
	return err
}

func (d *DB) DeleteKioskPolicy(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM kiosk_policies WHERE id = $1`, id)
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

// GetGroupDeviceIDsBatch returns device IDs per group for every group in groupIDs, in
// one query — replaces calling GetDeviceIDsByGroupIDs once per group in a loop.
func (d *DB) GetGroupDeviceIDsBatch(ctx context.Context, groupIDs []uuid.UUID) (map[uuid.UUID][]uuid.UUID, error) {
	out := make(map[uuid.UUID][]uuid.UUID, len(groupIDs))
	if len(groupIDs) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT group_id, device_id FROM device_groups WHERE group_id = ANY($1)
	`, groupIDs)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var gid, did uuid.UUID
		if err := rows.Scan(&gid, &did); err != nil {
			return nil, err
		}
		out[gid] = append(out[gid], did)
	}
	return out, rows.Err()
}

// GetDeviceIDsByRestaurantIDs returns the distinct device IDs assigned to any of the
// given restaurants/venues.
func (d *DB) GetDeviceIDsByRestaurantIDs(ctx context.Context, restaurantIDs []uuid.UUID) ([]uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id FROM devices WHERE restaurant_id = ANY($1) AND NOT hidden
	`, restaurantIDs)
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
			`SELECT id, created_at, actor, action, target, detail FROM audit_log WHERE actor = $2 ORDER BY created_at DESC LIMIT $1`, limit, actor)
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
func (d *DB) ListAuditForActivity(ctx context.Context, excludeActor string, includeViews bool, limit int) ([]AuditEntry, error) {
	if limit <= 0 || limit > 1000 {
		limit = 200
	}
	rows, err := d.pool.Query(ctx, `
		SELECT id, created_at, actor, action, target, detail FROM audit_log
		WHERE ($2 = '' OR actor <> $2) AND ($3 OR action <> '`+PageViewAction+`')
		ORDER BY created_at DESC LIMIT $1`, limit, excludeActor, includeViews)
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

// PruneResolvedAlerts deletes resolved alerts older than `days` days so the table
// doesn't grow without bound (open/acknowledged alerts are always kept).
func (d *DB) PruneResolvedAlerts(ctx context.Context, days int) (int64, error) {
	if days <= 0 {
		return 0, nil
	}
	tag, err := d.pool.Exec(ctx, fmt.Sprintf(
		`DELETE FROM alerts WHERE status = 'resolved' AND resolved_at < NOW() - INTERVAL '%d days'`, days))
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

// ResolveAlertsForHiddenDevices resolves any open/acknowledged alert belonging to a
// device that is now inactive (hidden), so an auto-inactivated unit's alerts clear
// out instead of lingering. Runs after the stale-device sweep.
func (d *DB) ResolveAlertsForHiddenDevices(ctx context.Context) (int64, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE alerts SET status = 'resolved', resolved_at = NOW(), updated_at = NOW()
		WHERE status <> 'resolved'
		  AND device_id IN (SELECT id FROM devices WHERE hidden)`)
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
					- c.created_at)), 600), 0) AS w,
				LAG(c.battery_pct) OVER (PARTITION BY c.device_id ORDER BY c.created_at) AS prev_batt
			FROM checkins c
			WHERE c.created_at >= $1::date AND c.created_at < ($1::date + INTERVAL '1 day')
		)
		INSERT INTO device_daily_stats AS s (
			device_id, day, checkin_count, battery_min, battery_max, battery_avg,
			temp_max, ram_pct_peak, charging_frac, online_minutes, build_id,
			first_seen_at, last_seen_at,
			wlc_guest_frac, pad_readable, storage_free_last_gb, discharge_pct, computed_at)
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
			-- Readable means the pad answered: 0 or 1. Excludes -1 (sysfs read failed) and
			-- 2 (no pad attached) — 2 is a successful read of a floating pin, so treating
			-- it as readable would report a healthy pad on a unit that has none.
			bool_or(extra->>'wlc_status' IN ('0', '1')),
			(ARRAY_AGG((extra->>'storage_free_gb')::numeric ORDER BY created_at DESC))[1]::real,
			-- True discharge: sum of per-step battery DROPS (charge climbs contribute 0,
			-- so a device that only charges accrues 0). NULL prev_batt (day's first
			-- sample) contributes nothing, so a discharge spanning midnight isn't split.
			COALESCE(SUM(GREATEST(0, prev_batt - battery_pct)), 0)::real,
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
			discharge_pct  = EXCLUDED.discharge_pct,
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

// WrappedStat is one "superlative" in the Fleet Wrapped review: a device (or
// venue) that tops some metric, with the value and a bit of context.
type WrappedStat struct {
	Serial     string  `json:"serial"`
	Restaurant string  `json:"restaurant"`
	Value      float64 `json:"value"`
	Extra      string  `json:"extra"` // e.g. the date it happened
}

// FleetWrapped is the "Spotify Wrapped for devices" review — playful all-time
// superlatives and totals for the whole fleet, computed from the rolled-up daily
// stats plus lifetime device counters, commands, and alerts.
type FleetWrapped struct {
	HasData         bool
	FirstDay        time.Time
	LastDay         time.Time
	DaysCovered     int
	DeviceCount     int
	RestaurantCount int

	TotalCheckins int64
	OnlineMinutes int64
	TotalCycles   float64 // fleet-wide lifetime full battery cycles (discharge/100)
	PeakRAM       int     // highest RAM pressure ever seen, %

	TotalCommands  int64
	TopCommandType string
	TotalAlerts    int64
	TopAlertType   string

	HardestWorker WrappedStat // most online minutes, all-time
	Busiest       WrappedStat // most check-ins, all-time
	Hottest       WrappedStat // highest temperature ever recorded
	MostWorn      WrappedStat // most lifetime battery cycles
	LongestDay    WrappedStat // most online minutes in a single day
	PadLover      WrappedStat // highest average charging coverage
	Veteran       WrappedStat // earliest device in the fleet
	BusiestVenue  WrappedStat // restaurant with the most check-ins (Serial holds the name)
}

// GetFleetWrapped computes the all-time Fleet Wrapped review. Every query excludes
// hidden (retired) devices. Superlatives left at their zero value simply don't
// render — the page guards on HasData for the overall empty case.
func (d *DB) GetFleetWrapped(ctx context.Context) (FleetWrapped, error) {
	var w FleetWrapped

	// Fleet-wide totals + span from the daily rollups.
	var first, last *time.Time
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(DISTINCT s.day), MIN(s.day), MAX(s.day),
		       COALESCE(SUM(s.checkin_count),0), COALESCE(SUM(s.online_minutes),0),
		       COALESCE(MAX(s.ram_pct_peak),0)
		FROM device_daily_stats s JOIN devices d ON d.id = s.device_id
		WHERE NOT d.hidden`).
		Scan(&w.DaysCovered, &first, &last, &w.TotalCheckins, &w.OnlineMinutes, &w.PeakRAM)
	if err != nil {
		return w, err
	}
	if first != nil {
		w.FirstDay = *first
	}
	if last != nil {
		w.LastDay = *last
	}
	w.HasData = w.TotalCheckins > 0

	// The 15 queries below (2 counts, 1 sum, 2 group-bys, 10 superlative "scanStat"
	// lookups) are each a single independent aggregate over a different slice of
	// the fleet's history — none depends on another's result. Firing them
	// sequentially meant this page's latency was the sum of 15 round trips. Run
	// them concurrently and write into local vars; results are only copied into `w`
	// after every goroutine has finished, so there's no concurrent access to it.
	var totalDischarge, totalCommands, totalAlerts int64
	var deviceCount, restaurantCount int
	var topCommandType, topAlertType string
	var hardestWorker, busiest, hottest, longestDay, padLover, mostWorn, veteran, busiestVenue WrappedStat

	// scanStat runs a superlative query returning (serial, restaurant, value, extra)
	// and tolerates no rows (leaves the stat zero).
	scanStat := func(q string) WrappedStat {
		var s WrappedStat
		_ = d.pool.QueryRow(ctx, q).Scan(&s.Serial, &s.Restaurant, &s.Value, &s.Extra)
		return s
	}

	var wg sync.WaitGroup
	run := func(f func()) {
		wg.Add(1)
		go func() { defer wg.Done(); f() }()
	}

	run(func() { _ = d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM devices WHERE NOT hidden`).Scan(&deviceCount) })
	run(func() { _ = d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM restaurants`).Scan(&restaurantCount) })
	run(func() {
		_ = d.pool.QueryRow(ctx, `SELECT COALESCE(SUM(discharge_total_pct),0) FROM devices WHERE NOT hidden`).Scan(&totalDischarge)
	})
	run(func() { _ = d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM commands`).Scan(&totalCommands) })
	run(func() {
		_ = d.pool.QueryRow(ctx, `SELECT type FROM commands GROUP BY type ORDER BY COUNT(*) DESC, type LIMIT 1`).Scan(&topCommandType)
	})
	run(func() { _ = d.pool.QueryRow(ctx, `SELECT COUNT(*) FROM alerts`).Scan(&totalAlerts) })
	run(func() {
		_ = d.pool.QueryRow(ctx, `SELECT type FROM alerts GROUP BY type ORDER BY COUNT(*) DESC, type LIMIT 1`).Scan(&topAlertType)
	})
	run(func() {
		hardestWorker = scanStat(`
			SELECT d.serial_number, COALESCE(r.name,''), SUM(s.online_minutes)::float8, ''
			FROM device_daily_stats s JOIN devices d ON d.id = s.device_id
			LEFT JOIN restaurants r ON r.id = d.restaurant_id
			WHERE NOT d.hidden GROUP BY d.id, d.serial_number, r.name
			ORDER BY 3 DESC LIMIT 1`)
	})
	run(func() {
		busiest = scanStat(`
			SELECT d.serial_number, COALESCE(r.name,''), SUM(s.checkin_count)::float8, ''
			FROM device_daily_stats s JOIN devices d ON d.id = s.device_id
			LEFT JOIN restaurants r ON r.id = d.restaurant_id
			WHERE NOT d.hidden GROUP BY d.id, d.serial_number, r.name
			ORDER BY 3 DESC LIMIT 1`)
	})
	run(func() {
		hottest = scanStat(`
			SELECT d.serial_number, COALESCE(r.name,''), s.temp_max::float8, to_char(s.day,'Mon DD')
			FROM device_daily_stats s JOIN devices d ON d.id = s.device_id
			LEFT JOIN restaurants r ON r.id = d.restaurant_id
			WHERE NOT d.hidden AND s.temp_max IS NOT NULL
			ORDER BY s.temp_max DESC LIMIT 1`)
	})
	run(func() {
		longestDay = scanStat(`
			SELECT d.serial_number, COALESCE(r.name,''), s.online_minutes::float8, to_char(s.day,'Mon DD')
			FROM device_daily_stats s JOIN devices d ON d.id = s.device_id
			LEFT JOIN restaurants r ON r.id = d.restaurant_id
			WHERE NOT d.hidden ORDER BY s.online_minutes DESC LIMIT 1`)
	})
	run(func() {
		padLover = scanStat(`
			SELECT d.serial_number, COALESCE(r.name,''), (AVG(s.charging_frac)*100)::float8, ''
			FROM device_daily_stats s JOIN devices d ON d.id = s.device_id
			LEFT JOIN restaurants r ON r.id = d.restaurant_id
			WHERE NOT d.hidden AND s.charging_frac IS NOT NULL
			GROUP BY d.id, d.serial_number, r.name HAVING COUNT(*) >= 3
			ORDER BY 3 DESC LIMIT 1`)
	})
	run(func() {
		mostWorn = scanStat(`
			SELECT d.serial_number, COALESCE(r.name,''), (d.discharge_total_pct::float8/100), ''
			FROM devices d LEFT JOIN restaurants r ON r.id = d.restaurant_id
			WHERE NOT d.hidden ORDER BY d.discharge_total_pct DESC LIMIT 1`)
	})
	run(func() {
		veteran = scanStat(`
			SELECT d.serial_number, COALESCE(r.name,''), 0::float8, to_char(d.created_at,'Mon DD, YYYY')
			FROM devices d LEFT JOIN restaurants r ON r.id = d.restaurant_id
			WHERE NOT d.hidden ORDER BY d.created_at ASC LIMIT 1`)
	})
	run(func() {
		// Busiest venue: name lands in Serial (WrappedStat has no venue field), Value = check-ins.
		busiestVenue = scanStat(`
			SELECT r.name, '', SUM(s.checkin_count)::float8, COUNT(DISTINCT d.id)::text
			FROM restaurants r JOIN devices d ON d.restaurant_id = r.id AND NOT d.hidden
			JOIN device_daily_stats s ON s.device_id = d.id
			GROUP BY r.id, r.name ORDER BY 3 DESC LIMIT 1`)
	})
	wg.Wait()

	w.DeviceCount = deviceCount
	w.RestaurantCount = restaurantCount
	w.TotalCycles = float64(totalDischarge) / 100
	w.TotalCommands = totalCommands
	w.TopCommandType = topCommandType
	w.TotalAlerts = totalAlerts
	w.TopAlertType = topAlertType
	w.HardestWorker = hardestWorker
	w.Busiest = busiest
	w.Hottest = hottest
	w.LongestDay = longestDay
	w.PadLover = padLover
	w.MostWorn = mostWorn
	w.Veteran = veteran
	w.BusiestVenue = busiestVenue

	return w, nil
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
// GetGroupHealth rolls up per-group health. offline_count is devices WITHOUT a live
// WebSocket (from the connected set), matching the dashboard's WS-based online status.
func (d *DB) GetGroupHealth(ctx context.Context, connected []uuid.UUID) ([]GroupHealth, error) {
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
				COUNT(*) FILTER (WHERE d.id <> ALL($1::uuid[])) AS offline_count,
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
			JOIN devices d ON d.id = a.device_id AND NOT d.hidden
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
		ORDER BY g.name`, connected)
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
	// overheating is WLC-aware: temp_c off the pad, temp_c_wlc while wireless-charging.
	{"overheating", "Device overheating", `{"temp_c":45,"temp_c_wlc":65}`, "always", true},
	// Memory pressure gives the report a configurable RAM cutoff; off by default.
	{"memory_pressure", "Memory pressure", `{"ram_pct":85}`, "always", false},
	{"storage_filling", "Storage filling fast", `{"drop_gb":0.2}`, "always", true},
	{"wlc_dead", "Wireless charger not functional all day", `{}`, "always", true},
	// Recent-tier rules (T7 matrix).
	{"offline", "Device offline", `{"offline_minutes":5}`, "always", true},
	{"storage_low", "Storage critically low", `{"free_gb":1}`, "always", true},
	{"storage_warning", "Storage low", `{"free_gb":14}`, "always", true},
	{"temp_elevated", "Temperature elevated", `{"temp_min":38,"temp_max":45}`, "always", true},
	{"memory_low", "Memory low (available)", `{"avail_mb":400}`, "always", true},
	{"wifi_weak", "Weak Wi-Fi signal", `{"rssi_dbm":-75,"sustain_min":10}`, "always", true},
	{"battery_high_night", "Battery high overnight", `{"soc_pct":60}`, "overnight", true},
	{"wlc_continuous", "Continuous wireless charging", `{"sustain_min":60}`, "always", true},
	{"charger_flapping", "Charger flapping / faulty", `{"window_min":5,"flaps_per_min":10}`, "always", true},
	{"battery_low", "Battery low during peak", `{"soc_pct":20}`, "peak", true},
	{"offline_peak", "Offline during peak", `{"offline_minutes":5}`, "peak", true},
	// Client-telemetry rules (need the new charger/wifi/crash fields the client reports).
	{"wifi_unstable", "Frequent Wi-Fi disconnects", `{"disconnects":3}`, "always", true},
	{"device_crash", "Device crash / ANR", `{"window_min":15}`, "always", true},
	{"slow_charge_night", "Slow overnight charging", `{"max_gain_pct":15,"window_hours":2}`, "overnight", true},
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
	// While a condition holds continuously we refresh the row's summary/detail/last-seen
	// but do NOT bump occurrences — the evaluator runs every minute, so counting each pass
	// produced meaningless "×4000" badges for a single ongoing outage. The alert's age
	// (fired_at → now) already conveys "how long", which is the useful signal. The
	// (xmax = 0) flag is true only for a genuine INSERT, so callers still notify exactly
	// once per episode, not on every re-fire.
	var inserted bool
	err := d.pool.QueryRow(ctx, `
		INSERT INTO alerts (rule_id, type, device_id, severity, summary, detail)
		VALUES ($1, $2, $3, $4, $5, $6::jsonb)
		ON CONFLICT (type, device_id) WHERE status <> 'resolved'
		DO UPDATE SET last_seen_at  = NOW(),
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
		       a.occurrences, a.fired_at, a.last_seen_at, a.resolved_at, a.updated_at
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
			&a.Occurrences, &a.FiredAt, &a.LastSeenAt, &a.ResolvedAt, &a.UpdatedAt); err != nil {
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
		       a.occurrences, a.fired_at, a.last_seen_at, a.resolved_at, a.updated_at,
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
			&a.Occurrences, &a.FiredAt, &a.LastSeenAt, &a.ResolvedAt, &a.UpdatedAt, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, a)
	}
	return out, total, rows.Err()
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
	// Exclude alerts on inactive (hidden) devices so silenced units don't inflate the
	// count; fleet-level alerts (no device) always count.
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM alerts a
		LEFT JOIN devices d ON d.id = a.device_id
		WHERE a.status = 'open' AND (a.device_id IS NULL OR NOT d.hidden)`).Scan(&n)
	return n, err
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

// ── Learned WiFi AP → location index ────────────────────────────────────────

// WifiAPLoc is one learned access point: its BSSID and the geographic point it
// was last resolved to (via Google), with how many local lookups it has served.
type WifiAPLoc struct {
	BSSID    string    `json:"bssid"`
	Lat      float64   `json:"lat"`
	Lon      float64   `json:"lon"`
	Accuracy float64   `json:"accuracy"`
	Hits     int64     `json:"hits"`
	SeenAt   time.Time `json:"seen_at"`
}

// WifiAPStats summarizes the learned index for the admin panel.
type WifiAPStats struct {
	Total            int64     `json:"total"`             // learned APs (all)
	Fresh            int64     `json:"fresh"`             // APs within the freshness window
	DistinctPlaces   int64     `json:"distinct_places"`   // ~unique points (lat/lon rounded)
	ServedLookups    int64     `json:"served_lookups"`    // SUM(hits) — local lookups served
	LastLearnedAt    time.Time `json:"last_learned_at"`
}

// LookupWifiAPs returns learned locations for the given BSSIDs that are still
// fresh (seen_at >= fresherThan). Used to locate a device from a WiFi scan
// without calling Google.
func (d *DB) LookupWifiAPs(ctx context.Context, bssids []string, fresherThan time.Time) ([]WifiAPLoc, error) {
	if len(bssids) == 0 {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT bssid, lat, lon, accuracy, hits, seen_at
		FROM wifi_ap_locations
		WHERE bssid = ANY($1) AND seen_at >= $2`, bssids, fresherThan)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WifiAPLoc
	for rows.Next() {
		var a WifiAPLoc
		if err := rows.Scan(&a.BSSID, &a.Lat, &a.Lon, &a.Accuracy, &a.Hits, &a.SeenAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// LearnWifiAPs records/refreshes the resolved location for a set of BSSIDs (the
// scan Google just resolved). Upsert: existing APs are re-pointed and their
// seen_at refreshed; hits is preserved.
func (d *DB) LearnWifiAPs(ctx context.Context, bssids []string, lat, lon, accuracy float64) error {
	if len(bssids) == 0 {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO wifi_ap_locations (bssid, lat, lon, accuracy, seen_at)
		SELECT unnest($1::text[]), $2, $3, $4, NOW()
		ON CONFLICT (bssid) DO UPDATE SET
			lat = EXCLUDED.lat, lon = EXCLUDED.lon,
			accuracy = EXCLUDED.accuracy, seen_at = NOW()`,
		bssids, lat, lon, accuracy)
	return err
}

// BumpWifiAPHits increments the hit counter for the APs that just served a local
// lookup (best-effort — a failure here must not fail the lookup).
func (d *DB) BumpWifiAPHits(ctx context.Context, bssids []string) error {
	if len(bssids) == 0 {
		return nil
	}
	_, err := d.pool.Exec(ctx,
		`UPDATE wifi_ap_locations SET hits = hits + 1 WHERE bssid = ANY($1)`, bssids)
	return err
}

// GetWifiAPStats summarizes the learned index. freshWindow bounds the "fresh" count.
func (d *DB) GetWifiAPStats(ctx context.Context, freshWindow time.Duration) (WifiAPStats, error) {
	var s WifiAPStats
	cutoff := time.Now().Add(-freshWindow)
	err := d.pool.QueryRow(ctx, `
		SELECT
			count(*),
			count(*) FILTER (WHERE seen_at >= $1),
			count(DISTINCT (round(lat::numeric, 4)::text || ',' || round(lon::numeric, 4)::text)),
			COALESCE(SUM(hits), 0),
			COALESCE(MAX(seen_at), 'epoch'::timestamptz)
		FROM wifi_ap_locations`, cutoff).
		Scan(&s.Total, &s.Fresh, &s.DistinctPlaces, &s.ServedLookups, &s.LastLearnedAt)
	return s, err
}

// ListWifiAPsRecent returns the most-recently-learned APs (for the admin panel table).
func (d *DB) ListWifiAPsRecent(ctx context.Context, limit int) ([]WifiAPLoc, error) {
	if limit <= 0 || limit > 500 {
		limit = 50
	}
	rows, err := d.pool.Query(ctx, `
		SELECT bssid, lat, lon, accuracy, hits, seen_at
		FROM wifi_ap_locations
		ORDER BY seen_at DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WifiAPLoc
	for rows.Next() {
		var a WifiAPLoc
		if err := rows.Scan(&a.BSSID, &a.Lat, &a.Lon, &a.Accuracy, &a.Hits, &a.SeenAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
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

// GetUserLayout returns the saved layout JSON for one user + page, or nil when the
// user has never customised that page (caller falls back to the default layout).
func (d *DB) GetUserLayout(ctx context.Context, username, page string) ([]byte, error) {
	var raw []byte
	err := d.pool.QueryRow(ctx, `SELECT layout FROM user_layouts WHERE username = $1 AND page = $2`,
		username, page).Scan(&raw)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	return raw, err
}

// SetUserLayout upserts a user's layout JSON for a page.
func (d *DB) SetUserLayout(ctx context.Context, username, page string, layout []byte) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO user_layouts (username, page, layout, updated_at) VALUES ($1, $2, $3, NOW())
		ON CONFLICT (username, page) DO UPDATE SET layout = EXCLUDED.layout, updated_at = NOW()
	`, username, page, layout)
	return err
}

// DeleteUserLayout removes a user's saved layout so the page returns to its default.
func (d *DB) DeleteUserLayout(ctx context.Context, username, page string) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM user_layouts WHERE username = $1 AND page = $2`, username, page)
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
// recent window, off the raw check-in stream. A faulty charger/dock connection drops
// in and out, so `charging` oscillates true↔false check-in to check-in — a healthy
// unit shows 0–1 transitions over hours, a flapping one dozens per minute. Rows
// missing the field are skipped so a partial payload can't fake a transition.
const chargingFlapSQL = `
	WITH seq AS (
		SELECT device_id,
		       (extra->>'charging')::boolean AS charging,
		       LAG((extra->>'charging')::boolean) OVER (PARTITION BY device_id ORDER BY created_at) AS prev
		FROM checkins
		WHERE created_at > NOW() - ($1 * INTERVAL '1 minute')
		  AND extra->>'charging' IN ('true','false')
	)
	SELECT device_id, COUNT(*) AS flaps
	FROM seq
	WHERE prev IS NOT NULL AND charging <> prev
	GROUP BY device_id`

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
	rows, err := d.pool.Query(ctx, chargingFlapSQL+` HAVING COUNT(*) > $2`, windowMin, minCount)
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
	return out, rows.Err()
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
		WITH seq AS (
			SELECT (extra->>'charging')::boolean AS charging,
			       LAG((extra->>'charging')::boolean) OVER (ORDER BY created_at) AS prev
			FROM checkins
			WHERE device_id = $1 AND created_at > NOW() - ($2 * INTERVAL '1 minute')
			  AND extra->>'charging' IN ('true','false')
		)
		SELECT COUNT(*) FROM seq WHERE prev IS NOT NULL AND charging <> prev`,
		deviceID, windowMin).Scan(&n)
	if err != nil {
		return 0, err
	}
	return flapRate(n, windowMin), nil
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

// ListRestaurantServiceWindows returns every restaurant's own service window in one
// query (restaurants without a row are absent; the caller falls back to the fleet
// default). Replaces the per-restaurant N+1 on the Settings page.
func (d *DB) ListRestaurantServiceWindows(ctx context.Context) (map[uuid.UUID]ServiceWindow, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT restaurant_id, open_min, close_min, night_open_min, night_close_min, timezone
		FROM service_windows WHERE restaurant_id IS NOT NULL`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[uuid.UUID]ServiceWindow)
	for rows.Next() {
		var id uuid.UUID
		var w ServiceWindow
		if err := rows.Scan(&id, &w.OpenMin, &w.CloseMin, &w.NightOpenMin, &w.NightCloseMin, &w.TZ); err != nil {
			return nil, err
		}
		rid := id
		w.RestaurantID = &rid
		out[id] = w
	}
	return out, rows.Err()
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

// ── Peak windows (discrete busy periods, active_window='peak') ───────────────────

// PeakWindow is one busy period for a venue (or the fleet default when RestaurantID
// is nil), in local minutes past midnight. A range may wrap past midnight (End < Start).
type PeakWindow struct {
	ID           uuid.UUID  `json:"id"`
	RestaurantID *uuid.UUID `json:"restaurant_id"`
	StartMin     int        `json:"start_min"`
	EndMin       int        `json:"end_min"`
}

// inAnyPeak reports whether local time "now" (converted with tz) falls in any of the
// ranges. Empty ranges → never in peak.
func inAnyPeak(now time.Time, tz string, ranges []PeakWindow) bool {
	if len(ranges) == 0 {
		return false
	}
	lt := localTime(now, tz)
	m := lt.Hour()*60 + lt.Minute()
	for _, r := range ranges {
		if inMinWindow(m, r.StartMin, r.EndMin) {
			return true
		}
	}
	return false
}

// ListPeakWindows returns the fleet-default peak set (restaurant_id NULL) plus every
// per-restaurant override, for the settings UI.
func (d *DB) ListPeakWindows(ctx context.Context) ([]PeakWindow, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, restaurant_id, start_min, end_min FROM peak_windows
		ORDER BY restaurant_id NULLS FIRST, start_min`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PeakWindow
	for rows.Next() {
		var w PeakWindow
		if err := rows.Scan(&w.ID, &w.RestaurantID, &w.StartMin, &w.EndMin); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// GetPeakWindows returns the peak ranges stored for one scope (a restaurant, or the
// fleet default when restaurantID is nil) — the raw rows, without inheritance.
func (d *DB) GetPeakWindows(ctx context.Context, restaurantID *uuid.UUID) ([]PeakWindow, error) {
	var rows pgx.Rows
	var err error
	if restaurantID == nil {
		rows, err = d.pool.Query(ctx, `SELECT id, restaurant_id, start_min, end_min FROM peak_windows WHERE restaurant_id IS NULL ORDER BY start_min`)
	} else {
		rows, err = d.pool.Query(ctx, `SELECT id, restaurant_id, start_min, end_min FROM peak_windows WHERE restaurant_id = $1 ORDER BY start_min`, restaurantID)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PeakWindow
	for rows.Next() {
		var w PeakWindow
		if err := rows.Scan(&w.ID, &w.RestaurantID, &w.StartMin, &w.EndMin); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// SetPeakWindows replaces the full peak set for one scope (a restaurant, or the fleet
// default when restaurantID is nil) in a single transaction — delete-then-insert, so
// the caller passes the complete desired list of ranges.
func (d *DB) SetPeakWindows(ctx context.Context, restaurantID *uuid.UUID, ranges []PeakWindow) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if restaurantID == nil {
		_, err = tx.Exec(ctx, `DELETE FROM peak_windows WHERE restaurant_id IS NULL`)
	} else {
		_, err = tx.Exec(ctx, `DELETE FROM peak_windows WHERE restaurant_id = $1`, restaurantID)
	}
	if err != nil {
		return err
	}
	for _, r := range ranges {
		if _, err = tx.Exec(ctx, `
			INSERT INTO peak_windows (restaurant_id, start_min, end_min) VALUES ($1, $2, $3)`,
			restaurantID, r.StartMin, r.EndMin); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// effectivePeakWindows resolves the peak ranges for every non-hidden device: its
// restaurant's own set if it has any, otherwise the fleet-default set. Loaded once per
// evaluation pass (mirrors effectiveWindows).
func (d *DB) effectivePeakWindows(ctx context.Context) (map[uuid.UUID][]PeakWindow, error) {
	rows, err := d.pool.Query(ctx, `SELECT restaurant_id, start_min, end_min FROM peak_windows`)
	if err != nil {
		return nil, err
	}
	var fleet []PeakWindow
	byRest := make(map[uuid.UUID][]PeakWindow)
	for rows.Next() {
		var rid *uuid.UUID
		var w PeakWindow
		if err := rows.Scan(&rid, &w.StartMin, &w.EndMin); err != nil {
			rows.Close()
			return nil, err
		}
		if rid == nil {
			fleet = append(fleet, w)
		} else {
			byRest[*rid] = append(byRest[*rid], w)
		}
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	drows, err := d.pool.Query(ctx, `SELECT id, restaurant_id FROM devices WHERE NOT hidden`)
	if err != nil {
		return nil, err
	}
	defer drows.Close()
	out := make(map[uuid.UUID][]PeakWindow)
	for drows.Next() {
		var id uuid.UUID
		var rid *uuid.UUID
		if err := drows.Scan(&id, &rid); err != nil {
			return nil, err
		}
		if rid != nil {
			if r, ok := byRest[*rid]; ok {
				out[id] = r
				continue
			}
		}
		out[id] = fleet
	}
	return out, drows.Err()
}

// EvaluateAlerts runs every enabled fleet-scoped rule against device_daily_stats,
// creating alerts for violators (deduped) and resolving alerts whose condition has
// cleared. Returns counts of created and resolved alerts. Called from housekeeping.
func (d *DB) EvaluateAlerts(ctx context.Context, connected []uuid.UUID) (created []AlertNotification, resolved int, err error) {
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
		hits, severity, e := d.detectRule(ctx, r.Type, p, connected)
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
				created = append(created, AlertNotification{Type: r.Type, Severity: severity, Summary: h.Summary, Serial: h.Serial, DeviceID: h.DeviceID, EventAt: at, Timezone: tz})
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
func (d *DB) detectRule(ctx context.Context, typ string, p map[string]float64, connected []uuid.UUID) ([]alertHit, string, error) {
	switch typ {
	case "offline":
		mins := int(param(p, "offline_minutes", 30))
		qs := int(param(p, "quiet_start", 0))
		qe := int(param(p, "quiet_end", 6))
		// A device with a live WebSocket is not offline even if last_seen_at is stale —
		// exclude the connected set so this matches the recent-tier rule and the
		// dashboard's WS-based online indicator (was inconsistent before).
		rows, err := d.pool.Query(ctx, `
			SELECT id, serial_number, last_seen_at, COALESCE(latest_extra->>'timezone', '')
			FROM devices
			WHERE NOT hidden AND last_seen_at < NOW() - ($1 * INTERVAL '1 minute')
			  AND id <> ALL($2::uuid[])`, mins, connected)
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
		// One offline alert per device at warning severity — a plain outage isn't a
		// page. The genuinely urgent case (offline during peak service) is a separate
		// critical alert (offline_peak). offline_long is retired (redundant).
		return hits, "warning", rows.Err()

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
		dropGB := param(p, "drop_gb", 0.2)
		floorGB := d.criticalStorageFloor(ctx)
		// Pure rate rule: free storage fell by more than drop_gb since yesterday. "How low"
		// is owned by storage_low / storage_warning; this one only watches how FAST it drops.
		// Skip devices already at/below the critical floor — storage_low owns those, so a
		// fast-dropping unit doesn't double-fire once it's also critically low.
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
			WHERE t.today IS NOT NULL AND t.today >= $1
			  AND t.yday IS NOT NULL AND (t.yday - t.today) > $2`, floorGB, dropGB)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var today, yday float64
			if err := rows.Scan(&id, &serial, &today, &yday); err != nil {
				return nil, "warning", err
			}
			drop := yday - today
			summary := fmt.Sprintf("Storage dropped %.1f→%.1f GB in 24h", yday, today)
			// Store the measured 24h drop (not the threshold) so the humanized card shows
			// the real number, plus today's free level for context.
			hits = append(hits, alertHit{id, serial, summary,
				map[string]any{"today_gb": today, "drop_gb": drop}})
		}
		return hits, "warning", rows.Err()

	case "wlc_dead":
		// The wireless charger was never readable for the whole of the last complete day
		// (pad_readable=false), on a deployed unit whose pad WAS working within the prior
		// week — i.e. a genuine regression, not a device that simply has no pad. Daily tier.
		rows, err := d.pool.Query(ctx, `
			SELECT s.device_id, dv.serial_number
			FROM device_daily_stats s JOIN devices dv ON dv.id = s.device_id
			WHERE s.day = CURRENT_DATE - 1 AND s.pad_readable = false
			  AND NOT dv.hidden AND dv.restaurant_id IS NOT NULL
			  AND EXISTS (
			    SELECT 1 FROM device_daily_stats p
			    WHERE p.device_id = s.device_id
			      AND p.day >= CURRENT_DATE - 8 AND p.day < CURRENT_DATE - 1
			      AND p.pad_readable = true)`)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			if err := rows.Scan(&id, &serial); err != nil {
				return nil, "warning", err
			}
			hits = append(hits, alertHit{id, serial,
				"Wireless charger not functional all day (pad never readable)",
				map[string]any{"day": "yesterday"}})
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
	"offline":            true,
	"offline_long":       true,
	"offline_peak":       true,
	"overheating":        true,
	"battery_low":        true,
	"storage_low":        true,
	"storage_warning":    true,
	"temp_elevated":      true,
	"memory_low":         true,
	"wifi_weak":          true,
	"wifi_unstable":      true,
	"battery_high_night": true,
	"wlc_continuous":     true,
	"charger_flapping":   true,
	"device_crash":       true,
	"slow_charge_night":  true,
}

func isRecentType(typ string) bool { return recentRuleTypes[typ] }

// defaultActiveWindow is the window a recent rule is gated to when its active_window
// column is unset. Most rules are fleet-wide ("always"); a few are inherently tied to a
// time-of-day window (e.g. the overnight battery-high check, peak-hours offline).
func defaultActiveWindow(typ string) string {
	switch typ {
	case "battery_high_night", "slow_charge_night":
		return "overnight"
	case "offline_peak", "battery_low":
		return "peak"
	}
	return "always"
}

// EvaluateRecentAlerts runs every enabled recent-tier rule against current telemetry,
// gating each hit to the rule's active window (per-venue service hours), then creating
// and auto-resolving alerts exactly like EvaluateAlerts. Called from the 1-minute ticker.
// EvaluateRecentAlerts runs the recent-tier rules. `connected` is the set of
// device ids with a live WebSocket, used so the offline family doesn't page a
// device that is still connected (see offlineHitsQuery).
func (d *DB) EvaluateRecentAlerts(ctx context.Context, connected []uuid.UUID) (created []AlertNotification, resolved int, err error) {
	rules, err := d.ListAlertRules(ctx, true)
	if err != nil {
		return nil, 0, err
	}
	windows, err := d.effectiveWindows(ctx)
	if err != nil {
		return nil, 0, err
	}
	peaks, err := d.effectivePeakWindows(ctx)
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
		hits, severity, e := d.detectRecentRule(ctx, r.Type, p, connected)
		if e != nil {
			return created, resolved, e
		}
		aw := r.ActiveWindow
		if aw == "" {
			aw = defaultActiveWindow(r.Type)
		}
		windowed := aw == "service" || aw == "overnight" || aw == "peak"
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
			// Peak windows are a separate list of ranges; service/overnight use the span.
			var inWin bool
			if aw == "peak" {
				inWin = inAnyPeak(now, windowFor(windows, h.DeviceID).TZ, peaks[h.DeviceID])
			} else {
				inWin = inActiveWindow(now, "", windowFor(windows, h.DeviceID), aw)
			}
			if !inWin {
				continue
			}
			ids = append(ids, h.DeviceID)
			ok, e := d.CreateAlertIfAbsent(ctx, &ruleID, r.Type, h.DeviceID, severity, h.Summary, h.Detail)
			if e != nil {
				return created, resolved, e
			}
			if ok {
				at, tz := notifyTimeFrom(h.Detail)
				created = append(created, AlertNotification{Type: r.Type, Severity: severity, Summary: h.Summary, Serial: h.Serial, DeviceID: h.DeviceID, EventAt: at, Timezone: tz})
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

// offlineHitsQuery finds devices offline longer than $1 minutes for the offline-family
// rule $2, EXCLUDING those already alerted for the current offline stretch. A resolved
// alert whose fired_at is at/after the device's last_seen_at means we alerted since the
// device was last seen — so it must not re-fire (even if an operator resolved it while
// still offline). last_seen_at only advances when the device comes back online, so a
// genuine recover-then-drop resets this and a fresh alert can fire. An OPEN alert is not
// 'resolved', so it stays in the hit set (deduped by CreateAlertIfAbsent, not re-notified).
const offlineHitsQuery = `
	SELECT d.id, d.serial_number, d.last_seen_at FROM devices d
	WHERE NOT d.hidden AND d.last_seen_at < NOW() - ($1 * INTERVAL '1 minute')
	  -- A device with a live WebSocket is NOT offline, even if its last_seen_at has
	  -- gone stale — this keeps the alert consistent with the dashboard's WS-based
	  -- online indicator instead of paging a still-connected device. $3 is the set of
	  -- currently-connected device ids (empty array = nobody connected → excludes none).
	  AND d.id <> ALL($3::uuid[])
	  AND NOT EXISTS (
	    SELECT 1 FROM alerts a
	    WHERE a.device_id = d.id AND a.type = $2 AND a.status = 'resolved'
	      AND a.fired_at >= d.last_seen_at
	  )`

// detectRecentRule returns devices currently violating a recent-tier rule. Window gating
// is applied by the caller (EvaluateRecentAlerts).
func (d *DB) detectRecentRule(ctx context.Context, typ string, p map[string]float64, connected []uuid.UUID) ([]alertHit, string, error) {
	switch typ {
	case "offline":
		mins := int(param(p, "offline_minutes", 5))
		rows, err := d.pool.Query(ctx, offlineHitsQuery, mins, "offline", connected)
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
		// A plain outage is a warning, not a page; offline_peak stays critical.
		return hits, "warning", rows.Err()

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
		// WLC-aware dual threshold: a device actively wireless-charging (wlc_status='1')
		// runs hotter by design, so it only alerts at the higher limit; off the pad the
		// lower limit applies. Point-in-time on the latest reading, so the alert lands
		// within ~1 min of the spike and auto-resolves once it cools.
		limit := param(p, "temp_c", 45)        // off-pad limit
		limitWLC := param(p, "temp_c_wlc", 65) // on-pad (wireless charging) limit
		rows, err := d.pool.Query(ctx, `
			SELECT d.id, d.serial_number, (d.latest_extra->>'battery_temp_c')::numeric,
			       d.last_seen_at, d.latest_extra->>'timezone',
			       (d.latest_extra->>'wlc_status' = '1') AS on_wlc
			FROM devices d
			WHERE NOT d.hidden AND d.last_seen_at > NOW() - INTERVAL '`+recentReportingCutoff+`'
			  AND (
			    (d.latest_extra->>'wlc_status' = '1' AND (d.latest_extra->>'battery_temp_c')::numeric >= $2)
			    OR
			    (COALESCE(d.latest_extra->>'wlc_status','') <> '1' AND (d.latest_extra->>'battery_temp_c')::numeric >= $1)
			  )`, limit, limitWLC)
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
			var onWLC bool
			if err := rows.Scan(&id, &serial, &temp, &seen, &tz, &onWLC); err != nil {
				return nil, "critical", err
			}
			lim := limit
			if onWLC {
				lim = limitWLC
			}
			detail := map[string]any{"temp_c": temp, "limit_c": lim, "on_wlc": onWLC, "event_at": seen}
			if tz != nil && *tz != "" {
				detail["timezone"] = *tz
			}
			ctx := "off pad"
			if onWLC {
				ctx = "on wireless charger"
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Device temperature is %.0f°C %s (limit %.0f°C)", temp, ctx, lim),
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

	case "storage_warning":
		// Point-in-time warning band: free storage at/under warn_gb but still above the
		// critical floor, so a low disk warns once and only escalates to critical when it
		// gets dire. The floor is the critical storage_low rule's own trigger — the warning
		// hands off to it automatically, so there's no separate floor setting to keep in sync.
		warnGB := param(p, "free_gb", 14)
		floorGB := d.criticalStorageFloor(ctx)
		rows, err := d.pool.Query(ctx, `
			SELECT d.id, d.serial_number, (d.latest_extra->>'storage_free_gb')::numeric
			FROM devices d
			WHERE NOT d.hidden AND d.last_seen_at > NOW() - INTERVAL '`+recentReportingCutoff+`'
			  AND (d.latest_extra->>'storage_free_gb')::numeric <= $1
			  AND (d.latest_extra->>'storage_free_gb')::numeric >  $2`, warnGB, floorGB)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var free float64
			if err := rows.Scan(&id, &serial, &free); err != nil {
				return nil, "warning", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Storage low: %.1f GB free (warn ≤ %.0f GB)", free, warnGB),
				map[string]any{"storage_free_gb": free, "limit_gb": warnGB}})
		}
		return hits, "warning", rows.Err()

	case "charger_flapping":
		// Faulty charger/dock: the charging state oscillates true↔false as the
		// connection drops in and out. Flag devices toggling faster than the
		// per-minute rate over the window. Excludes hidden devices.
		windowMin := int(param(p, "window_min", 5))
		flapsPerMin := int(param(p, "flaps_per_min", 10))
		flapping, err := d.FlappingChargers(ctx, windowMin, flapsPerMin)
		if err != nil {
			return nil, "warning", err
		}
		if len(flapping) == 0 {
			return nil, "warning", nil
		}
		ids := make([]uuid.UUID, 0, len(flapping))
		for id := range flapping {
			ids = append(ids, id)
		}
		rows, err := d.pool.Query(ctx, `
			SELECT id, serial_number FROM devices WHERE id = ANY($1) AND NOT hidden`, ids)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			if err := rows.Scan(&id, &serial); err != nil {
				return nil, "warning", err
			}
			rate := flapping[id]
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Faulty charger — charging toggling ~%d×/min", rate),
				map[string]any{"rate": rate, "window_min": windowMin}})
		}
		return hits, "warning", rows.Err()

	case "battery_high_night":
		// Point-in-time battery SoC at/above the threshold. Overnight-windowed +
		// deployed-only (gated by the caller), so it flags venue units sitting high on the
		// charger overnight rather than cycling down.
		pct := param(p, "soc_pct", 60)
		rows, err := d.pool.Query(ctx, `
			SELECT d.id, d.serial_number, d.latest_battery_pct
			FROM devices d
			WHERE NOT d.hidden AND d.last_seen_at > NOW() - INTERVAL '`+recentReportingCutoff+`'
			  AND d.latest_battery_pct >= $1`, int(pct))
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var batt int
			if err := rows.Scan(&id, &serial, &batt); err != nil {
				return nil, "warning", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Battery at %d%% overnight (≥ %.0f%%)", batt, pct),
				map[string]any{"battery_pct": batt, "limit_pct": pct}})
		}
		return hits, "warning", rows.Err()

	case "wlc_continuous":
		// Sustained: every reading in the last ~(sustain+5) min is on the wireless
		// charger (wlc_status='1') and the run spans ≥ sustain_min — i.e. the device has
		// been continuously wireless-charging for over an hour (heat/battery stress). A
		// gap or any non-'1'/missing reading breaks continuity (bool_and over NULL fails).
		sustain := param(p, "sustain_min", 60)
		rows, err := d.pool.Query(ctx, `
			SELECT c.device_id, dv.serial_number,
			       EXTRACT(EPOCH FROM (MAX(c.created_at) - MIN(c.created_at)))/60 AS span_min
			FROM (
				SELECT device_id, (extra->>'wlc_status') AS wlc, created_at
				FROM checkins WHERE created_at > NOW() - (($1 + 5) * INTERVAL '1 minute')
			) c JOIN devices dv ON dv.id = c.device_id
			WHERE NOT dv.hidden
			GROUP BY c.device_id, dv.serial_number
			-- A missing wlc_status must break continuity too (the comment above says so);
			-- bool_and skips NULLs, so treat NULL as "not on the pad" explicitly.
			HAVING bool_and(c.wlc IS NOT NULL AND c.wlc = '1')
			   AND (MAX(c.created_at) - MIN(c.created_at)) >= ($1 * INTERVAL '1 minute')`, sustain)
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var span float64
			if err := rows.Scan(&id, &serial, &span); err != nil {
				return nil, "critical", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("On wireless charger continuously for %.0f min (≥ %.0f)", span, sustain),
				map[string]any{"span_min": span, "limit_min": sustain}})
		}
		return hits, "critical", rows.Err()

	case "offline_long":
		// Retired: it duplicated the `offline` alert (one device raised both). `offline`
		// is now the single per-device outage alert. Returning no hits means the eval loop
		// auto-resolves any lingering open offline_long alerts on the next pass.
		return nil, "warning", nil

	case "battery_low":
		// Point-in-time low SoC. Peak-windowed + deployed-only (gated by the caller), so
		// it flags a venue unit running low during the busy lunch/dinner rush.
		pct := param(p, "soc_pct", 20)
		rows, err := d.pool.Query(ctx, `
			SELECT d.id, d.serial_number, d.latest_battery_pct
			FROM devices d
			WHERE NOT d.hidden AND d.last_seen_at > NOW() - INTERVAL '`+recentReportingCutoff+`'
			  AND d.latest_battery_pct < $1`, int(pct))
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var batt int
			if err := rows.Scan(&id, &serial, &batt); err != nil {
				return nil, "critical", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Battery at %d%% during peak hours (< %.0f%%)", batt, pct),
				map[string]any{"battery_pct": batt, "limit_pct": pct}})
		}
		return hits, "critical", rows.Err()

	case "offline_peak":
		// Offline during peak hours (peak-windowed + deployed-only via the caller). Shorter
		// fuse than the anytime rules because an outage mid-service is urgent.
		mins := int(param(p, "offline_minutes", 5))
		rows, err := d.pool.Query(ctx, offlineHitsQuery, mins, "offline_peak", connected)
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
				fmt.Sprintf("Offline %dm during peak hours", down),
				map[string]any{"offline_minutes": down, "last_seen": last}})
		}
		return hits, "critical", rows.Err()

	case "wifi_unstable":
		// Point-in-time on the client-reported rolling count of Wi-Fi disconnects in the
		// last hour (extra.wifi_disconnects_1h).
		limit := param(p, "disconnects", 3)
		rows, err := d.pool.Query(ctx, `
			SELECT d.id, d.serial_number, (d.latest_extra->>'wifi_disconnects_1h')::numeric
			FROM devices d
			WHERE NOT d.hidden AND d.last_seen_at > NOW() - INTERVAL '`+recentReportingCutoff+`'
			  AND (d.latest_extra->>'wifi_disconnects_1h')::numeric >= $1`, limit)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var n float64
			if err := rows.Scan(&id, &serial, &n); err != nil {
				return nil, "warning", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("%.0f Wi-Fi disconnects in the last hour (≥ %.0f)", n, limit),
				map[string]any{"disconnects_1h": n, "limit": limit}})
		}
		return hits, "warning", rows.Err()

	case "device_crash":
		// A crash/ANR/tombstone was reported (device_events, from the client's DropBox
		// reader) within the window. Auto-resolves once no crash is seen for that long.
		mins := int(param(p, "window_min", 15))
		rows, err := d.pool.Query(ctx, `
			SELECT d.id, d.serial_number, COUNT(*), MAX(e.occurred_at),
			       (array_agg(e.kind ORDER BY e.occurred_at DESC))[1],
			       (array_agg(e.summary ORDER BY e.occurred_at DESC))[1]
			FROM devices d JOIN device_events e ON e.device_id = d.id
			WHERE NOT d.hidden AND e.kind <> 'reboot'
			  AND e.occurred_at > NOW() - ($1 * INTERVAL '1 minute')
			GROUP BY d.id, d.serial_number`, mins)
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial, kind, summary string
			var n int
			var at time.Time
			if err := rows.Scan(&id, &serial, &n, &at, &kind, &summary); err != nil {
				return nil, "critical", err
			}
			label := fmt.Sprintf("%d crash(es) in %dm — latest %s", n, mins, kind)
			hits = append(hits, alertHit{id, serial, label,
				map[string]any{"count": n, "kind": kind, "summary": summary, "event_at": at}})
		}
		return hits, "critical", rows.Err()

	case "slow_charge_night":
		// Over the last window_hours the device was continuously charging yet its
		// battery barely rose (≤ max_gain_pct) and isn't essentially full — a stalled
		// / trickle charge that won't be ready by morning. Overnight-windowed.
		maxGain := param(p, "max_gain_pct", 15)
		windowH := param(p, "window_hours", 2)
		rows, err := d.pool.Query(ctx, `
			SELECT s.device_id, dv.serial_number, s.first_batt, s.last_batt
			FROM (
				SELECT device_id,
				       (array_agg(battery_pct ORDER BY created_at ASC))[1]  AS first_batt,
				       (array_agg(battery_pct ORDER BY created_at DESC))[1] AS last_batt,
				       -- COALESCE missing charging to FALSE: a check-in that doesn't
				       -- confirm charging breaks "always charging" (bool_and silently
				       -- SKIPS NULLs, so without this an unplugged stretch that omitted
				       -- the field still counted as continuously charging).
				       bool_and(COALESCE((extra->>'charging')::boolean, false)) AS always_charging,
				       COUNT(*) AS n,
				       (MAX(created_at) - MIN(created_at)) AS span
				FROM checkins
				WHERE created_at > NOW() - ($1 * INTERVAL '1 hour')
				GROUP BY device_id
			) s JOIN devices dv ON dv.id = s.device_id
			WHERE NOT dv.hidden AND s.always_charging AND s.n >= 3
			  AND s.span >= (($1 - 0.25) * INTERVAL '1 hour')
			  AND (s.last_batt - s.first_batt) <= $2
			  AND s.last_batt < 95`, windowH, maxGain)
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var first, last int
			if err := rows.Scan(&id, &serial, &first, &last); err != nil {
				return nil, "critical", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Charging for %.0fh but battery only went %d%%→%d%%", windowH, first, last),
				map[string]any{"gain_pct": last - first, "first_pct": first, "last_pct": last}})
		}
		return hits, "critical", rows.Err()
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

-- Shared package_name → launcher-icon index. Any device that reports an icon populates
-- it; every device's app list then resolves icons from here by package_name, so old
-- clients that never send icons still render them. PRIMARY KEY is the lookup index.
CREATE TABLE IF NOT EXISTS app_icons (
	package_name TEXT PRIMARY KEY,
	icon         TEXT NOT NULL,
	updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE device_packages ADD COLUMN IF NOT EXISTS app_name TEXT NOT NULL DEFAULT '';
ALTER TABLE device_packages ADD COLUMN IF NOT EXISTS is_system BOOLEAN NOT NULL DEFAULT FALSE;
-- is_system becomes nullable: NULL means the device's client is too old to report it,
-- so the admin override (app_system_overrides) decides the classification instead.
ALTER TABLE device_packages ALTER COLUMN is_system DROP NOT NULL;
ALTER TABLE device_packages ALTER COLUMN is_system DROP DEFAULT;

-- Admin-managed list of package names flagged as system apps. Used as the fallback
-- classification when no client has reported is_system for a package.
CREATE TABLE IF NOT EXISTS app_system_overrides (
	package_name TEXT PRIMARY KEY,
	created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

ALTER TABLE devices ADD COLUMN IF NOT EXISTS poll_interval_ms INTEGER NOT NULL DEFAULT 30000;
ALTER TABLE devices ADD COLUMN IF NOT EXISTS latest_battery_pct SMALLINT NOT NULL DEFAULT 0;
ALTER TABLE devices ADD COLUMN IF NOT EXISTS latest_extra JSONB NOT NULL DEFAULT '{}';

-- Battery wear: running total of cumulative percent discharged, never reset.
-- UpsertCheckin adds each downward step in battery level (charging/flat frames add 0).
-- Equivalent full cycles = discharge_total_pct / 100 (250 => 2.5 cycles).
ALTER TABLE devices ADD COLUMN IF NOT EXISTS discharge_total_pct BIGINT NOT NULL DEFAULT 0;
-- Guards the ONE-TIME historical backfill (BackfillDischargeCycles, run in the
-- background at startup — NOT here; see the checkins-backfill warning above).
-- Existing rows start false (need seeding from history); new devices default true
-- since UpsertCheckin maintains their counter from the first check-in.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS discharge_backfilled BOOLEAN NOT NULL DEFAULT false;
ALTER TABLE devices ALTER COLUMN discharge_backfilled SET DEFAULT true;

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

-- Offline kiosk-exit: a provisioned TOTP seed lets a technician leave kiosk lock
-- mode on-device without server access. Enabled by default (opt-out per device);
-- the seed is generated lazily on first read (see GetOrCreateDeviceConfig).
ALTER TABLE device_config ADD COLUMN IF NOT EXISTS offline_exit_enabled BOOLEAN NOT NULL DEFAULT TRUE;
ALTER TABLE device_config ADD COLUMN IF NOT EXISTS offline_exit_seed TEXT NOT NULL DEFAULT '';
ALTER TABLE device_config ADD COLUMN IF NOT EXISTS offline_exit_relock TEXT NOT NULL DEFAULT 'reboot';
-- Wireless-charging control: the client writes the customer_gpio line to enable/disable
-- charging on the pad. Default true (charging on); pushed to the device like kiosk config.
ALTER TABLE device_config ADD COLUMN IF NOT EXISTS wlc_charging_enabled BOOLEAN NOT NULL DEFAULT TRUE;

-- A named, standing kiosk-lock policy assigned to a restaurant/group/device/the whole
-- fleet — the Manage page's unit, not a one-shot command. Applying/editing/deleting a
-- policy resolves its target to the matching device IDs AT THAT MOMENT and writes
-- device_config directly (same mechanism as before policies existed); it is not a live
-- binding that tracks membership drift after the fact — a device added to a target group
-- later doesn't retroactively pick up the policy without a re-save. target_id holds a
-- restaurant_id or group_id depending on target_type; target_serial holds a device
-- serial for target_type='device'. Exactly one of them is set (or neither, for 'all').
CREATE TABLE IF NOT EXISTS kiosk_policies (
	id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	name          TEXT NOT NULL,
	kiosk_package TEXT NOT NULL,
	target_type   TEXT NOT NULL, -- 'all' | 'restaurant' | 'group' | 'device'
	target_id     UUID,
	target_serial TEXT NOT NULL DEFAULT '',
	created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
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
    role          TEXT NOT NULL CHECK (role IN ('viewer','operator','tester','user_manager','dev','admin')),
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

-- Learned WiFi access-point → location index. Populated from successful Google
-- Geolocation lookups (each contributing BSSID stamped with the scan's resolved
-- point). Lets the server locate a device from a WiFi scan that overlaps known
-- APs WITHOUT calling Google — the primary cost optimization. Entries expire by
-- seen_at (staleness), and hits counts how many times an AP helped serve a
-- lookup locally.
CREATE TABLE IF NOT EXISTS wifi_ap_locations (
    bssid      TEXT PRIMARY KEY,
    lat        DOUBLE PRECISION NOT NULL,
    lon        DOUBLE PRECISION NOT NULL,
    accuracy   DOUBLE PRECISION NOT NULL DEFAULT 0,
    hits       BIGINT      NOT NULL DEFAULT 0,
    seen_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_wifi_ap_seen ON wifi_ap_locations(seen_at DESC);

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
-- its incrementals) under a single lifecycle (draft -> published).
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

-- Per-product releases: a release targets one hardware product (t7/kiosk18/22/27), so a
-- device is only ever offered/sent a release matching its own product (enforced in
-- ResolveUpdateForDevice). Existing releases predate multi-product and were all T7, so
-- backfill + default to t7 — consistent with the devices empty->t7 rule.
-- Identity is (version, product), NOT version alone: the same version string can exist
-- for different products (a t7 "2026.06.01" and a kiosk22 "2026.06.01" are two distinct
-- releases). This must run BEFORE the backfill seed below, whose ON CONFLICT targets the
-- composite index.
ALTER TABLE releases ADD COLUMN IF NOT EXISTS product TEXT NOT NULL DEFAULT 't7';
ALTER TABLE releases DROP CONSTRAINT IF EXISTS releases_version_key;
CREATE UNIQUE INDEX IF NOT EXISTS uq_releases_version_product ON releases(version, product);

-- Backfill one release per existing target build (falling back to the legacy
-- build_id when target_build_id is blank). Existing packages are marked published
-- so deployments already in flight keep resolving; then link packages and updates.
INSERT INTO releases (version, status, changelog, created_at, published_at)
  SELECT COALESCE(NULLIF(target_build_id,''), build_id), 'published', MAX(changelog), MIN(created_at), MIN(created_at)
  FROM ota_packages
  GROUP BY COALESCE(NULLIF(target_build_id,''), build_id)
  ON CONFLICT (version, product) DO NOTHING;
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

-- Peak-hour windows: discrete busy periods (e.g. lunch 12:00–14:00, dinner 19:00–21:00);
-- a rule with active_window='peak' fires only inside one. Unlike service_windows (a single
-- open/close span) a venue can have MANY peak rows; restaurant_id NULL = the fleet-default
-- set, inherited by any restaurant with no rows of its own. (Defined here, after the
-- restaurants table it references.)
CREATE TABLE IF NOT EXISTS peak_windows (
    id            UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    restaurant_id UUID REFERENCES restaurants(id) ON DELETE CASCADE,  -- NULL = fleet default
    start_min     INTEGER NOT NULL,   -- local minutes past midnight
    end_min       INTEGER NOT NULL,   -- may wrap past midnight (end < start)
    created_at    TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_peak_windows_restaurant ON peak_windows(restaurant_id);
-- Seed the fleet-default peak set once: lunch 12:00–14:00 and dinner 19:00–21:00.
INSERT INTO peak_windows (restaurant_id, start_min, end_min)
SELECT NULL::uuid, v.s, v.e FROM (VALUES (720,840),(1140,1260)) v(s,e)
WHERE NOT EXISTS (SELECT 1 FROM peak_windows WHERE restaurant_id IS NULL);

-- Device events: crash/ANR/tombstone entries (from the client's DropBox reader) and
-- reboots (detected from a changed per-boot id). Deduped by (device, kind, occurred_at)
-- so the client can safely re-report the last hour on every check-in.
CREATE TABLE IF NOT EXISTS device_events (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    device_id   UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
    kind        TEXT NOT NULL,                 -- DropBox crash tag, or 'reboot'
    summary     TEXT NOT NULL DEFAULT '',
    occurred_at TIMESTAMPTZ NOT NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE UNIQUE INDEX IF NOT EXISTS idx_device_events_dedupe ON device_events(device_id, kind, occurred_at);
CREATE INDEX IF NOT EXISTS idx_device_events_device_time ON device_events(device_id, occurred_at DESC);
-- Last per-boot id seen, to detect reboots (a change = the device rebooted).
ALTER TABLE devices ADD COLUMN IF NOT EXISTS last_boot_id TEXT NOT NULL DEFAULT '';

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

-- Test team (QA): widen the user role check to allow a 'operator' account. Drop-then-add
-- keeps it idempotent across restarts (the inline constraint is auto-named users_role_check).
-- NOTE: this re-ADD re-validates every existing row on every boot, so its role list
-- must contain EVERY role that can exist in the table — including ones added by later
-- statements (e.g. 'dev' below). A narrower list here fails validation against a row a
-- later statement legitimately allows, crashing the migration. Keep this the full set.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD  CONSTRAINT users_role_check CHECK (role IN ('viewer','operator','tester','user_manager','dev','admin'));

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
-- this release's own cases); a row is written only once an operator records a status, so a
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
ALTER TABLE users ADD  CONSTRAINT users_role_check CHECK (role IN ('viewer','operator','tester','user_manager','dev','admin'));

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

-- When the reboot that applies an installed OTA was pushed to the device (manual
-- "Reboot to apply", bulk reboot-all, or the automatic/scheduled path). Shown on the
-- deployment page next to each installed device. Backfilled once from the reboot
-- command that followed the install for rows that predate the column.
ALTER TABLE update_devices ADD COLUMN IF NOT EXISTS reboot_sent_at TIMESTAMPTZ;
UPDATE update_devices ud SET reboot_sent_at = sub.at
FROM (
	SELECT ud2.update_id, ud2.device_id, MIN(c.created_at) AS at
	FROM update_devices ud2
	JOIN command_targets ct ON ct.target_id = ud2.device_id
	JOIN commands c ON c.id = ct.command_id AND c.type = 'reboot' AND c.target_type = 'devices'
	WHERE ud2.reboot_sent_at IS NULL
	  AND ud2.status IN ('reboot_sent', 'installed')
	  AND ud2.completed_at IS NOT NULL
	  AND c.created_at >= ud2.completed_at - INTERVAL '2 minutes'
	  AND c.created_at <= ud2.completed_at + INTERVAL '7 days'
	GROUP BY ud2.update_id, ud2.device_id
) sub
WHERE ud.update_id = sub.update_id AND ud.device_id = sub.device_id AND ud.reboot_sent_at IS NULL;

-- Per-user access policy (operators: which actions on which device groups /
-- restaurants; viewers: which devices they see). See db.AccessPolicy.
ALTER TABLE users ADD COLUMN IF NOT EXISTS access JSONB NOT NULL DEFAULT '{}';

-- One row per build change per device (from → to, when). Written at check-in when
-- the reported build differs from the stored one; backfilled from checkins by the
-- hourly housekeeping (newest day first). The device graph's build markers read
-- this instead of scanning check-in history.
CREATE TABLE IF NOT EXISTS device_build_history (
	device_id  UUID        NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	at         TIMESTAMPTZ NOT NULL,
	from_build TEXT        NOT NULL,
	to_build   TEXT        NOT NULL,
	PRIMARY KEY (device_id, at)
);

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

-- QFIL flashing packages. Each release can carry one or more QFIL bundles
-- (Qualcomm Flash Image Loader packages used to flash a device from scratch over
-- USB). A dev or admin attaches them per release; the test team reads them off the
-- release page when flashing units. The bundle itself lives at an external URL
-- (like an OTA package), not in the DB.
CREATE TABLE IF NOT EXISTS qfil_packages (
    id          SERIAL      PRIMARY KEY,
    release_id  INTEGER     NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
    label       TEXT        NOT NULL DEFAULT '',
    url         TEXT        NOT NULL,
    notes       TEXT        NOT NULL DEFAULT '',
    added_by    TEXT        NOT NULL DEFAULT '',
    status      TEXT        NOT NULL DEFAULT 'active',   -- active|removed
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_qfil_packages_release ON qfil_packages(release_id);

-- Command recipes: saved action presets (type + payload + target) that the
-- Actions builder can replay in one click. target_serials / target_groups
-- snapshot the intended target so a recipe survives group/device churn as a
-- best-effort prefill (unknown serials are simply dropped when applied).
CREATE TABLE IF NOT EXISTS command_recipes (
    id             UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    name           TEXT NOT NULL,
    type           TEXT NOT NULL,
    apk_url        TEXT NOT NULL DEFAULT '',
    payload        JSONB NOT NULL DEFAULT '{}',
    target_type    TEXT NOT NULL DEFAULT 'all',
    target_serials TEXT[] NOT NULL DEFAULT '{}',
    target_groups  UUID[] NOT NULL DEFAULT '{}',
    created_by     TEXT NOT NULL DEFAULT '',
    created_at     TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
-- scope target spec ({mode,id,status,battery,kiosk}) when target_type='scope', so a
-- scheduled recipe can re-resolve its scope-rail selection at run time.
ALTER TABLE command_recipes ADD COLUMN IF NOT EXISTS scope JSONB NOT NULL DEFAULT '{}';

-- Scheduled recipes: run a saved recipe on a cron; the target is re-resolved from the
-- recipe's spec each time it fires (so group/scope membership is current, not snapshot).
CREATE TABLE IF NOT EXISTS scheduled_recipes (
    id          UUID PRIMARY KEY DEFAULT gen_random_uuid(),
    recipe_id   UUID NOT NULL REFERENCES command_recipes(id) ON DELETE CASCADE,
    cron_expr   TEXT NOT NULL,
    enabled     BOOLEAN NOT NULL DEFAULT true,
    run_once    BOOLEAN NOT NULL DEFAULT false,  -- disable after the first successful run
    next_run_at TIMESTAMPTZ,                     -- NULL = not scheduled (paused/expired)
    last_run_at TIMESTAMPTZ,
    created_by  TEXT NOT NULL DEFAULT '',
    created_at  TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_scheduled_recipes_due ON scheduled_recipes(next_run_at) WHERE enabled;

-- Commands an operator has "cleared" from the Needs-attention triage list. This
-- only dismisses them from that view — the command and its delivery records are
-- untouched (it still appears in Completed / history). ON DELETE CASCADE keeps it
-- tidy if the command is ever actually deleted.
CREATE TABLE IF NOT EXISTS dismissed_commands (
    command_id   UUID PRIMARY KEY REFERENCES commands(id) ON DELETE CASCADE,
    dismissed_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    dismissed_by TEXT NOT NULL DEFAULT ''
);

-- Performance indexes (2026 audit). All idempotent. These back hot filters/joins/
-- sorts that previously fell back to sequential scans on larger fleets.
CREATE INDEX IF NOT EXISTS idx_command_status_device_id ON command_status(device_id);
CREATE INDEX IF NOT EXISTS idx_devices_build_id         ON devices(build_id);
CREATE INDEX IF NOT EXISTS idx_device_groups_device_id  ON device_groups(device_id);
CREATE INDEX IF NOT EXISTS idx_device_config_device_id  ON device_config(device_id);
CREATE INDEX IF NOT EXISTS idx_ota_packages_release     ON ota_packages(release_id);
CREATE INDEX IF NOT EXISTS idx_updates_release          ON updates(release_id);
CREATE INDEX IF NOT EXISTS idx_update_devices_device_id ON update_devices(device_id);
CREATE INDEX IF NOT EXISTS idx_commands_created_at      ON commands(created_at DESC);
-- ListCommandsSince filters "type != 'update_splash' AND created_at >= ..." together;
-- the single-column indexes above/below can only be used one at a time (or bitmap-AND'd,
-- weaker than a composite). Covers that filter directly once the table is large.
CREATE INDEX IF NOT EXISTS idx_commands_created_at_type  ON commands(created_at DESC, type);
CREATE INDEX IF NOT EXISTS idx_alerts_fired_at          ON alerts(fired_at DESC);
CREATE INDEX IF NOT EXISTS idx_device_daily_stats_device_day ON device_daily_stats(device_id, day DESC);

-- Interim install progress: a device reports 'downloading' (with a percent) then
-- 'installing' before the terminal 'installed'/'failed'. progress is 0-100 while
-- downloading, NULL otherwise.
ALTER TABLE command_status ADD COLUMN IF NOT EXISTS progress SMALLINT;

-- received_at is stamped when the DEVICE confirms it received the command (a client
-- 'received' ack), as opposed to 'delivered' which only means the frame was enqueued
-- onto the socket. A non-NULL received_at means re-delivery/redrive must stop — the
-- device has it. NULL on old clients (no receipt ack) → the redrive falls back to its
-- best-effort staleness heuristic.
ALTER TABLE command_status ADD COLUMN IF NOT EXISTS received_at TIMESTAMPTZ;

-- Learned APK-URL → package-name map. Populated when a device acks an install and
-- reports which package the APK produced. Lets the server reconcile a pending
-- install against a device's reported package list (clears a stuck "installing" the
-- moment the app is actually present, even if the terminal ack was lost).
CREATE TABLE IF NOT EXISTS apk_packages (
	apk_url      TEXT PRIMARY KEY,
	package_name TEXT NOT NULL,
	updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);

-- Package name (e.g. com.example.app) an admin records when adding a repository
-- app. When set, it seeds apk_packages immediately so an install can skip devices
-- that already have the app — no need to wait for a first install to "learn" it.
ALTER TABLE apps ADD COLUMN IF NOT EXISTS package_name TEXT NOT NULL DEFAULT '';
ALTER TABLE apps ADD COLUMN IF NOT EXISTS s3_key TEXT NOT NULL DEFAULT '';
-- versionName parsed from the APK manifest at upload — shown in the App Library and
-- everywhere the app picker appears (Actions, device page) so an operator can tell
-- which build they're about to install. Re-uploading the same package still creates a
-- new row (no version-aware replace flow yet), so this alone doesn't dedup.
ALTER TABLE apps ADD COLUMN IF NOT EXISTS version_name TEXT NOT NULL DEFAULT '';

-- Freeform operator notes on a device (e.g. "cracked screen", "reserved for QA").
ALTER TABLE devices ADD COLUMN IF NOT EXISTS notes TEXT NOT NULL DEFAULT '';

-- Links an auto-triggered logcat capture to the alert that requested it (e.g. a
-- device_crash alert grabs the device's error logs). NULL for manual captures.
ALTER TABLE logcat_requests ADD COLUMN IF NOT EXISTS alert_id UUID;

-- Structured problem reports filed by operators against a release. A problem is
-- release-scoped (auto-links the build), optionally references a specific test
-- case and the device it was seen on, carries a severity + lifecycle status, and
-- holds freeform detail (repro steps, pasted logs). This is the tracked record
-- behind a QA "fail" so problems can be handed off, fixed and re-verified.
CREATE TABLE IF NOT EXISTS release_problems (
	id           UUID PRIMARY KEY DEFAULT gen_random_uuid(),
	release_id   INTEGER NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
	test_case_id UUID REFERENCES test_cases(id) ON DELETE SET NULL,
	device_id    UUID REFERENCES devices(id) ON DELETE SET NULL,
	build_id     TEXT NOT NULL DEFAULT '',
	title        TEXT NOT NULL,
	description  TEXT NOT NULL DEFAULT '',
	severity     TEXT NOT NULL DEFAULT 'major',   -- blocker | major | minor
	status       TEXT NOT NULL DEFAULT 'open',    -- open | fixed | verified | wontfix
	reported_by  TEXT NOT NULL DEFAULT '',
	created_at   TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	updated_at   TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS idx_release_problems_release ON release_problems(release_id);
-- How a release problem was filed: 'manual' (operator filed it) or 'qa' (auto-created
-- when a QA test case was marked failed). One 'qa' problem per (release, test_case).
-- (Must run AFTER the CREATE TABLE above — the migration executes as one ordered
-- script, so a fresh database has no release_problems table until this point.)
ALTER TABLE release_problems ADD COLUMN IF NOT EXISTS source TEXT NOT NULL DEFAULT 'manual';

-- Full DropBox crash/ANR/tombstone body (the stack trace) for a crash event, so the
-- crash alert and release page can show the real diagnostic, not just the headline.
ALTER TABLE device_events ADD COLUMN IF NOT EXISTS detail TEXT NOT NULL DEFAULT '';
-- The build the device was running when it reported the event, so a crash is
-- attributed to the build it happened on (not whatever the device later updates
-- to). CrashesOnBuild filters on this. One-time backfill: attribute existing crash
-- events from the nearest prior check-in. Self-limiting (only un-attributed rows).
ALTER TABLE device_events ADD COLUMN IF NOT EXISTS build_id TEXT NOT NULL DEFAULT '';
UPDATE device_events e SET build_id = COALESCE((
        SELECT c.build_id FROM checkins c
        WHERE c.device_id = e.device_id AND c.created_at <= e.occurred_at AND c.build_id <> ''
        ORDER BY c.created_at DESC LIMIT 1), '')
WHERE e.build_id = '' AND e.kind <> 'reboot'
  AND EXISTS (SELECT 1 FROM checkins c WHERE c.device_id = e.device_id AND c.created_at <= e.occurred_at AND c.build_id <> '');

-- Release-problem continuity ("rides the release train"): a manual bug is a thread that
-- follows the releases forward until an operator verifies it fixed. fixed_in_release_id is
-- the build a dev claims the fix landed in; verified_in_release_id is the build an operator
-- confirmed it on. Origin stays in release_id ("reported in"). Prior/next ordering uses
-- releases.created_at chronology — the manual version_order table is display-only.
ALTER TABLE release_problems ADD COLUMN IF NOT EXISTS fixed_in_release_id    INTEGER REFERENCES releases(id) ON DELETE SET NULL;
ALTER TABLE release_problems ADD COLUMN IF NOT EXISTS verified_in_release_id INTEGER REFERENCES releases(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_release_problems_fixed_in ON release_problems(fixed_in_release_id);

-- Release lifecycle finish line: "testing done" moves a release from active to inactive.
-- A release is ACTIVE (under test — shown in the hub focus band) while testing_done_at is
-- NULL; marking testing complete stamps it, retiring the release from the active slot so
-- the next build takes over. This is the real end of the cycle — dev sign-off is only an
-- intermediate gate, not the finish.
ALTER TABLE releases ADD COLUMN IF NOT EXISTS testing_done_at TIMESTAMPTZ;
ALTER TABLE releases ADD COLUMN IF NOT EXISTS testing_done_by TEXT NOT NULL DEFAULT '';

-- Branch builds: an off-mainline, temporary test build forked from a release (like a git
-- branch off main). parent_release_id is what it forked from; is_branch marks it so it's
-- kept out of the mainline path — it never becomes the active release, never joins the
-- release train, and its problems are fully isolated (no carry-forward either direction,
-- absent from the cross-release board). It can still hold packages + deploy like any
-- release. Nested under its parent in the UI.
ALTER TABLE releases ADD COLUMN IF NOT EXISTS parent_release_id INTEGER REFERENCES releases(id) ON DELETE SET NULL;
ALTER TABLE releases ADD COLUMN IF NOT EXISTS is_branch BOOLEAN NOT NULL DEFAULT false;
CREATE INDEX IF NOT EXISTS idx_releases_parent ON releases(parent_release_id);

-- Merge a validated branch back onto the main line. Merging creates a NEW mainline release
-- (the next version) that carries the branch's delta; the branch is then closed out.
-- merged_from_release_id is set on the new mainline node (the branch it absorbed);
-- merged_into_release_id is set on the branch (where it landed). A branch with
-- merged_into_release_id set is terminal ("merged") and offers no further merge.
ALTER TABLE releases ADD COLUMN IF NOT EXISTS merged_from_release_id INTEGER REFERENCES releases(id) ON DELETE SET NULL;
ALTER TABLE releases ADD COLUMN IF NOT EXISTS merged_into_release_id INTEGER REFERENCES releases(id) ON DELETE SET NULL;
ALTER TABLE releases ADD COLUMN IF NOT EXISTS merged_at TIMESTAMPTZ;
ALTER TABLE releases ADD COLUMN IF NOT EXISTS merged_by TEXT NOT NULL DEFAULT '';

-- A test case can be tied to the release problem it verifies. When a dev marks a
-- carried-over bug "fixed here", a "Verify fix: …" case is added to the release's QA
-- checklist (problem_id set); passing that case verifies the linked problem. One such
-- case per (release, problem).
ALTER TABLE test_cases ADD COLUMN IF NOT EXISTS problem_id UUID REFERENCES release_problems(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_test_cases_problem ON test_cases(problem_id);

-- Retire the offline_long alert: it duplicated the offline alert on every offline
-- device. Drop its rule and resolve any open ones so they clear from the inbox.
DELETE FROM alert_rules WHERE type = 'offline_long';
UPDATE alerts SET status = 'resolved', resolved_at = NOW(), updated_at = NOW()
    WHERE type = 'offline_long' AND status <> 'resolved';

-- Perf: a fingerprint of the device's installed-app set. Lets UpsertDevicePackages
-- skip the (expensive) full delete+reinsert on the >99% of check-ins where the app
-- list is unchanged.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS packages_hash TEXT;

-- Perf: indexes for hot query paths found in the scale audit.
-- commands.type is filtered on every check-in (pending-command / OTA / install checks).
CREATE INDEX IF NOT EXISTS idx_commands_type ON commands(type);
-- command_targets is probed by target_id (device) alone; the PK leads with command_id.
CREATE INDEX IF NOT EXISTS idx_command_targets_target ON command_targets(target_id);
-- Retention prunes DELETE these by created_at; without an index they full-scan a wide table.
CREATE INDEX IF NOT EXISTS idx_logcat_results_created_at ON logcat_results(created_at);
CREATE INDEX IF NOT EXISTS idx_logcat_requests_created_at ON logcat_requests(created_at);
-- NOTE: JSONB expression indexes for the device-list RAM%/temperature sorts were
-- considered and rejected — at ~900 devices the planner picks a seq-scan+sort anyway
-- (measured), and the index would have to be maintained on latest_extra on EVERY
-- check-in. Revisit only if the fleet grows past several thousand devices.

-- Battery-cycle accuracy: per-day TRUE discharge (sum of battery DROPS only, ignoring
-- charge climbs), so cycles reflect actual wear rather than the day's SoC range (which
-- counted charging as wear). NULL on pre-existing days → the cycle recompute falls back
-- to the old max-min estimate for history and uses this exact value going forward.
ALTER TABLE device_daily_stats ADD COLUMN IF NOT EXISTS discharge_pct REAL;
-- The old range-based cumulative, kept as a separate "legacy" figure during the
-- transition so operators can compare the corrected number against the prior estimate.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS discharge_legacy_pct BIGINT NOT NULL DEFAULT 0;

-- Multi-product support: the hardware category the device reports on check-in
-- (e.g. 't7', 'kiosk27'). Empty means the device predates the field / didn't report;
-- capability resolution treats empty as the default product (T7). Drives capability
-- gating (wlc/charging) and dashboard grouping/filtering. See internal/product.
ALTER TABLE devices ADD COLUMN IF NOT EXISTS product TEXT NOT NULL DEFAULT '';
CREATE INDEX IF NOT EXISTS idx_devices_product ON devices(product);

-- (Historical) Operator role retired: folded any remaining operator accounts down
-- to viewer, back when 'operator' meant the OLD role being retired here. That
-- one-time job finished long ago. The statement is now REMOVED — 'operator' was
-- later reused as the renamed 'tester' role (see the tester->operator migration
-- below), and this unconditional UPDATE was unconditionally re-running on every
-- boot, silently demoting every real operator account back to viewer on each
-- redeploy. Do not re-add a bare "role='operator' -> role='viewer'" statement.

-- Device diagnostics catalog: admin-curated read-only device queries surfaced as
-- friendly "retrieve property" buttons (e.g. getprop ro.build.id, dumpsys battery).
-- Each runs as an ordinary shell command whose text comes only from this catalog,
-- so operators can run vetted queries without raw shell access.
CREATE TABLE IF NOT EXISTS device_queries (
	id          SERIAL PRIMARY KEY,
	label       TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	command     TEXT NOT NULL,
	category    TEXT NOT NULL DEFAULT 'General',
	sort        INTEGER NOT NULL DEFAULT 0,
	enabled     BOOLEAN NOT NULL DEFAULT TRUE,
	created_by  TEXT NOT NULL DEFAULT '',
	created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Seed a starter set, but only while the catalog is empty, so an admin's later
-- edits/deletes are never undone by a redeploy.
INSERT INTO device_queries (label, description, command, category, sort)
SELECT * FROM (VALUES
	('Build ID',        'Full build fingerprint id',        'getprop ro.build.id',                'System',  10),
	('Android version', 'Android release version',          'getprop ro.build.version.release',   'System',  20),
	('Model',           'Hardware model name',              'getprop ro.product.model',           'System',  30),
	('Serial number',   'Hardware serial',                  'getprop ro.serialno',                'System',  40),
	('Battery',         'Battery service dump',             'dumpsys battery',                    'Power',   50),
	('Storage (data)',  'Free space on /data',              'df /data',                           'Storage', 60),
	('Uptime',          'How long the device has been up',  'uptime',                             'System',  70),
	('Wi-Fi',           'Current Wi-Fi connection',         'dumpsys wifi | grep -i "mWifiInfo"', 'Network', 80)
) AS v(label, description, command, category, sort)
WHERE NOT EXISTS (SELECT 1 FROM device_queries);

-- Self-service sign-up + password reset: email identity/verification on users,
-- plus a generic single-use token table shared by both flows.
ALTER TABLE users ADD COLUMN IF NOT EXISTS email TEXT;
ALTER TABLE users ADD COLUMN IF NOT EXISTS email_verified_at TIMESTAMPTZ;
CREATE UNIQUE INDEX IF NOT EXISTS users_email_unique ON users (lower(email)) WHERE email IS NOT NULL;

-- Dev role retired: explicitly dropping existing dev accounts rather than folding
-- them into operator (matches how operator was retired above, but this time the
-- accounts themselves go, not just the role label).
DELETE FROM users WHERE role = 'dev';

-- NOTE: 'operator' is included here (unlike the original historical wording) even
-- though this statement's own job (dropping dev accounts) has nothing to do with
-- it — this ALTER re-validates every existing row on every boot, so, per the same
-- rule noted above, its list must contain every role that legitimately exists in
-- the table by this point, including 'operator' rows that persist from the
-- tester->operator rename further down. Omitting it here breaks every redeploy.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD  CONSTRAINT users_role_check CHECK (role IN ('viewer','operator','tester','user_manager','dev','admin'));

CREATE TABLE IF NOT EXISTS user_tokens (
    token      TEXT PRIMARY KEY,
    user_id    UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    purpose    TEXT NOT NULL CHECK (purpose IN ('verify','reset')),
    expires_at TIMESTAMPTZ NOT NULL,
    used_at    TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE INDEX IF NOT EXISTS user_tokens_user_id_idx ON user_tokens (user_id);

-- "Tester" role renamed to "operator": there's no separate testing workflow left
-- in MDM, and "operator" is already the term used everywhere else (canOperate,
-- requireOperatorOrAdmin) for this exact permission tier, so the role label was
-- just out of sync with the concept. Existing tester accounts keep their
-- permissions, just relabeled. The constraint must allow BOTH values while the
-- UPDATE is converting rows — the currently-active constraint at this point in
-- the script is (viewer,tester), which would reject the UPDATE outright.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD  CONSTRAINT users_role_check CHECK (role IN ('viewer','operator','tester','user_manager','dev','admin'));
UPDATE users SET role = 'operator' WHERE role = 'tester';
-- Role ladder: admin (super admin) → dev → user_manager → operator → viewer.
-- Admins and devs may be real accounts too (e.g. Microsoft sign-in), not only
-- the env login. This is the effective constraint; keep it last.
ALTER TABLE users DROP CONSTRAINT IF EXISTS users_role_check;
ALTER TABLE users ADD  CONSTRAINT users_role_check CHECK (role IN ('viewer','operator','tester','user_manager','dev','admin'));

-- First/last name, settable at sign-up and editable by an admin afterward so the
-- dashboard and audit trail can show a real name instead of a bare email address.
ALTER TABLE users ADD COLUMN IF NOT EXISTS first_name TEXT NOT NULL DEFAULT '';
ALTER TABLE users ADD COLUMN IF NOT EXISTS last_name  TEXT NOT NULL DEFAULT '';

-- Attribution: which user pushed a given command, shown on the Actions page and
-- a device's queue/history. A snapshot of the display name/email at creation time
-- (not a user_id FK) so it survives the user account later being renamed or deleted.
-- Empty for system/scheduler-initiated commands (auto-reboot, redrive).
ALTER TABLE commands ADD COLUMN IF NOT EXISTS created_by TEXT NOT NULL DEFAULT '';

-- Per-user dashboard layouts (widget order / hidden widgets / chosen preset), keyed
-- by username + page so a user's Overview arrangement follows them across devices.
-- Keyed by username rather than users.id because the built-in admin session has no
-- users row. Absent row = the page's default layout.
CREATE TABLE IF NOT EXISTS user_layouts (
    username   TEXT NOT NULL,
    page       TEXT NOT NULL,
    layout     JSONB NOT NULL,
    updated_at TIMESTAMPTZ NOT NULL DEFAULT NOW(),
    PRIMARY KEY (username, page)
);
`

// ── OTA Packages ──────────────────────────────────────────────────────────────

func (d *DB) CreateOTAPackage(ctx context.Context, typ, targetBuildID, sourceBuildID, updateURL, changelog, product string, releaseDate time.Time) (*OTAPackage, error) {
	rel, err := d.GetOrCreateRelease(ctx, targetBuildID, product)
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

// ── QFIL Packages ───────────────────────────────────────────────────────────────

// CreateQFILPackage attaches a QFIL flashing bundle to a release.
func (d *DB) CreateQFILPackage(ctx context.Context, releaseID int, label, url, notes, addedBy string) (*QFILPackage, error) {
	var p QFILPackage
	err := d.pool.QueryRow(ctx, `
		INSERT INTO qfil_packages (release_id, label, url, notes, added_by)
		VALUES ($1, $2, $3, $4, $5)
		RETURNING id, release_id, label, url, notes, added_by, status, created_at
	`, releaseID, label, url, notes, addedBy).
		Scan(&p.ID, &p.ReleaseID, &p.Label, &p.URL, &p.Notes, &p.AddedBy, &p.Status, &p.CreatedAt)
	return &p, err
}

// ListQFILPackagesByRelease returns the active QFIL bundles for a release, newest first.
func (d *DB) ListQFILPackagesByRelease(ctx context.Context, releaseID int) ([]QFILPackage, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, release_id, label, url, notes, added_by, status, created_at
		FROM qfil_packages
		WHERE release_id = $1 AND status = 'active'
		ORDER BY created_at DESC
	`, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QFILPackage
	for rows.Next() {
		var p QFILPackage
		if err := rows.Scan(&p.ID, &p.ReleaseID, &p.Label, &p.URL, &p.Notes, &p.AddedBy, &p.Status, &p.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// DeleteQFILPackage removes a QFIL bundle from its release.
func (d *DB) DeleteQFILPackage(ctx context.Context, id int) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM qfil_packages WHERE id = $1`, id)
	return err
}

// DeleteQFILPackagesByRelease clears every QFIL bundle on a release. A release
// carries at most one QFIL package, so adding a new one replaces the old.
func (d *DB) DeleteQFILPackagesByRelease(ctx context.Context, releaseID int) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM qfil_packages WHERE release_id = $1`, releaseID)
	return err
}

// ── Releases ──────────────────────────────────────────────────────────────────

// GetOrCreateRelease creates the release for a version (target build id) if absent,
// tagging a newly-created one with the given product (normalized; empty -> t7). An
// existing release keeps its product — this never re-tags one.
func (d *DB) GetOrCreateRelease(ctx context.Context, version, product string) (*Release, error) {
	// Resolve default-substitutes an empty/unknown product to t7, so a release always
	// carries a valid catalog key regardless of what the form/derivation supplied.
	p, _ := prod.Resolve(product)
	product = p.Key
	// Identity is (version, product) — the same version for a different product is a
	// distinct release, so conflict is only when BOTH match.
	if _, err := d.pool.Exec(ctx,
		`INSERT INTO releases (version, product) VALUES ($1, $2) ON CONFLICT (version, product) DO NOTHING`, version, product); err != nil {
		return nil, err
	}
	return d.GetReleaseByVersion(ctx, version, product)
}

// GetReleaseByVersion returns the release for a (version, product) pair. product is
// default-substituted (empty/legacy -> t7) so a device's raw product can be passed
// straight through.
func (d *DB) GetReleaseByVersion(ctx context.Context, version, product string) (*Release, error) {
	p, _ := prod.Resolve(product)
	var r Release
	err := d.pool.QueryRow(ctx, `
		SELECT id, version, product, name, changelog, status, created_at, published_at
		FROM releases WHERE version = $1 AND product = $2
	`, version, p.Key).Scan(&r.ID, &r.Version, &r.Product, &r.Name, &r.Changelog, &r.Status, &r.CreatedAt, &r.PublishedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (d *DB) GetRelease(ctx context.Context, id int) (*Release, error) {
	var r Release
	err := d.pool.QueryRow(ctx, `
		SELECT id, version, product, name, changelog, status, skip_base_tests, created_at, published_at,
		       signed_off_by, signed_off_at, testing_done_at, testing_done_by, parent_release_id, is_branch,
		       merged_from_release_id, merged_into_release_id, merged_at, merged_by
		FROM releases WHERE id = $1
	`, id).Scan(&r.ID, &r.Version, &r.Product, &r.Name, &r.Changelog, &r.Status, &r.SkipBaseTests, &r.CreatedAt, &r.PublishedAt,
		&r.SignedOffBy, &r.SignedOffAt, &r.TestingDoneAt, &r.TestingDoneBy, &r.ParentReleaseID, &r.IsBranch,
		&r.MergedFromReleaseID, &r.MergedIntoReleaseID, &r.MergedAt, &r.MergedBy)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (d *DB) ListReleases(ctx context.Context) ([]Release, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT r.id, r.version, r.product, r.name, r.changelog, r.status, r.hidden, r.created_at, r.published_at,
		       r.signed_off_by, r.signed_off_at, r.testing_done_at, r.testing_done_by,
		       r.parent_release_id, r.is_branch,
		       r.merged_from_release_id, r.merged_into_release_id, r.merged_at, r.merged_by,
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
		if err := rows.Scan(&r.ID, &r.Version, &r.Product, &r.Name, &r.Changelog, &r.Status, &r.Hidden, &r.CreatedAt, &r.PublishedAt, &r.SignedOffBy, &r.SignedOffAt, &r.TestingDoneAt, &r.TestingDoneBy, &r.ParentReleaseID, &r.IsBranch, &r.MergedFromReleaseID, &r.MergedIntoReleaseID, &r.MergedAt, &r.MergedBy, &r.PackageCount, &r.DeployCount); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ReleaseRailItem is a published release shown in the fleet collections rail, with the
// count of non-hidden devices currently reporting its version. Clicking it scopes the
// roster to that release (via the build_id filter), the same way groups/restaurants do.
type ReleaseRailItem struct {
	ID          int
	Version     string
	Name        string
	DeviceCount int
}

// ListPublishedReleasesForRail returns published, non-hidden releases newest-first, each
// with the number of non-hidden devices currently on that version.
func (d *DB) ListPublishedReleasesForRail(ctx context.Context) ([]ReleaseRailItem, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT r.id, r.version, r.name, COUNT(dev.build_id)::int AS device_count
		FROM releases r
		LEFT JOIN devices dev ON dev.build_id = r.version AND NOT dev.hidden
		    AND (CASE WHEN dev.product = '' THEN 't7' ELSE dev.product END) = r.product
		WHERE r.status = 'published' AND NOT r.hidden
		GROUP BY r.id, r.version, r.name, r.published_at, r.created_at
		ORDER BY r.published_at DESC NULLS LAST, r.created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReleaseRailItem
	for rows.Next() {
		var r ReleaseRailItem
		if err := rows.Scan(&r.ID, &r.Version, &r.Name, &r.DeviceCount); err != nil {
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
		       rel.id, COALESCE(rel.status, ''), COALESCE(rel.hidden, false),
		       COALESCE(rel.product, MAX(CASE WHEN d.product = '' THEN 't7' ELSE d.product END))
		FROM devices d
		-- Match a device to the release for ITS product (empty/legacy -> t7): a same-version
		-- release for another product is a different release and must not link here.
		LEFT JOIN releases rel ON rel.version = d.build_id
		    AND rel.product = CASE WHEN d.product = '' THEN 't7' ELSE d.product END
		WHERE NOT d.hidden AND d.build_id <> ''
		GROUP BY d.build_id, rel.id, rel.status, rel.hidden, rel.product
		ORDER BY COUNT(*) DESC, d.build_id
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FleetVersion
	for rows.Next() {
		var v FleetVersion
		if err := rows.Scan(&v.Version, &v.DeviceCount, &v.Serials, &v.ReleaseID, &v.ReleaseStatus, &v.ReleaseHidden, &v.Product); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// ProductForVersion returns the product of the devices currently reporting a build
// version (the most common one), so a release tracked from a live fleet version inherits
// the right product. Returns "" when no device reports it (caller defaults to t7).
func (d *DB) ProductForVersion(ctx context.Context, version string) (string, error) {
	var p string
	err := d.pool.QueryRow(ctx, `
		SELECT CASE WHEN product = '' THEN 't7' ELSE product END AS p
		FROM devices WHERE build_id = $1 AND NOT hidden
		GROUP BY p ORDER BY COUNT(*) DESC LIMIT 1
	`, version).Scan(&p)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", nil
	}
	return p, err
}

// FilterDeviceIDsByProduct returns the subset of ids whose device product matches the
// given product (an empty/legacy device product counts as t7). Used to keep a
// deployment's targets to the release's own product so a wrong-product device can't be
// rowed into a deployment in the first place.
func (d *DB) FilterDeviceIDsByProduct(ctx context.Context, ids []uuid.UUID, product string) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	p, _ := prod.Resolve(product)
	rows, err := d.pool.Query(ctx, `
		SELECT id FROM devices
		WHERE id = ANY($1) AND (CASE WHEN product = '' THEN 't7' ELSE product END) = $2`, ids, p.Key)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// OTAInProgress is one device currently taking an OTA, with its live download/install
// percent (from the device's last ota_progress telemetry). Powers the releases-page
// "OTA in progress" summary card.
type OTAInProgress struct {
	DeviceID      uuid.UUID
	Serial        string
	TargetVersion string
	Product       string
	Status        string // "pending" (awaiting check-in) | "downloading" (command sent)
	Percent       int    // from telemetry; the handler overlays the live shell value
	Phase         string // from telemetry; the handler overlays the live shell value
	UpdateID      int    // deployment (updates.id) this device's OTA belongs to
	ReleaseID     int    // release (releases.id) that deployment is for
}

// ActiveOTADevices lists every device in an in-flight OTA (pending/downloading on an
// active deployment), newest progress first, with the live percent/phase it last reported.
func (d *DB) ActiveOTADevices(ctx context.Context) ([]OTAInProgress, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.id, d.serial_number, COALESCE(rel.version, ''), COALESCE(rel.product, 't7'), ud.status,
		       COALESCE((d.latest_extra->'ota_progress'->>'percent')::int, 0),
		       COALESCE(d.latest_extra->'ota_progress'->>'phase', ''),
		       u.id, COALESCE(rel.id, 0)
		FROM update_devices ud
		JOIN updates u ON u.id = ud.update_id AND u.status = 'active'
		LEFT JOIN releases rel ON rel.id = u.release_id
		JOIN devices d ON d.id = ud.device_id
		WHERE ud.status IN ('pending', 'downloading')
		ORDER BY (d.latest_extra->'ota_progress'->>'percent')::int DESC NULLS LAST, d.serial_number`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OTAInProgress
	for rows.Next() {
		var o OTAInProgress
		if err := rows.Scan(&o.DeviceID, &o.Serial, &o.TargetVersion, &o.Product, &o.Status, &o.Percent, &o.Phase, &o.UpdateID, &o.ReleaseID); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SerialsUpdating returns serial_number -> target version for every device currently in an
// in-flight OTA (pending/downloading on an active deployment). A push picker uses this to
// mark such devices "updating" instead of offering them again — the device's build_id
// still shows the OLD version until it reboots, so a build_id check alone misses them.
func (d *DB) SerialsUpdating(ctx context.Context) (map[string]string, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.serial_number, COALESCE(rel.version, '')
		FROM update_devices ud
		JOIN updates u ON u.id = ud.update_id AND u.status = 'active'
		LEFT JOIN releases rel ON rel.id = u.release_id
		JOIN devices d ON d.id = ud.device_id
		WHERE ud.status IN ('pending', 'downloading')`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var s, v string
		if err := rows.Scan(&s, &v); err != nil {
			return nil, err
		}
		out[s] = v
	}
	return out, rows.Err()
}

// RemoveDowngradeTargets returns the subset of ids that may receive the given release —
// i.e. devices whose current build is NOT already a same-or-newer release of the same
// product. Prevents OTAing an older (or identical) release onto a device: without this a
// device on ota-test-3 could be sent ota-test-2 and sit pending forever. Ordering within a
// product is (created_at, id), both creation-monotonic. A device whose current build is not
// a managed release can't be proven a regression, so it is kept.
func (d *DB) RemoveDowngradeTargets(ctx context.Context, releaseID int, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return ids, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT dv.id
		FROM devices dv, releases tgt
		WHERE tgt.id = $1
		  AND dv.id = ANY($2)
		  AND NOT EXISTS (
		      SELECT 1 FROM releases cur
		      WHERE cur.product = tgt.product
		        AND cur.version = dv.build_id
		        AND (cur.created_at, cur.id) >= (tgt.created_at, tgt.id)
		  )`, releaseID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// RemoveInapplicableTargets drops devices that have NO applicable artifact for the
// release: a device is kept only if the release has an active full image (covers any
// device) OR the device's current build matches an active incremental's source_build_id.
// Without this, selecting a device on an unrelated build for an incremental-only release
// creates a pending row the resolver can never satisfy — it sits "pending" forever.
func (d *DB) RemoveInapplicableTargets(ctx context.Context, releaseID int, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return ids, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT dv.id
		FROM devices dv
		WHERE dv.id = ANY($2)
		  AND (
		      EXISTS (SELECT 1 FROM ota_packages p
		              WHERE p.release_id = $1 AND p.status = 'active' AND p.type = 'full')
		   OR EXISTS (SELECT 1 FROM ota_packages p
		              WHERE p.release_id = $1 AND p.status = 'active' AND p.type = 'incremental'
		                AND p.source_build_id = dv.build_id)
		  )`, releaseID, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// SerialsOnNewerRelease returns serial_number -> current build for every device whose
// current build is a STRICTLY newer release (same product) than the given release. A push
// picker uses this to disable such devices and label them "newer installed", since OTAing
// an older release onto them is not allowed (see RemoveDowngradeTargets / the resolver
// monotonicity gate). Devices already on the target version are handled separately as
// "up to date".
func (d *DB) SerialsOnNewerRelease(ctx context.Context, releaseID int) (map[string]string, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT dv.serial_number, dv.build_id
		FROM devices dv, releases tgt
		WHERE tgt.id = $1
		  AND EXISTS (
		      SELECT 1 FROM releases cur
		      WHERE cur.product = tgt.product
		        AND cur.version = dv.build_id
		        AND cur.version <> tgt.version
		        AND (cur.created_at, cur.id) > (tgt.created_at, tgt.id)
		  )`, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]string)
	for rows.Next() {
		var s, b string
		if err := rows.Scan(&s, &b); err != nil {
			return nil, err
		}
		out[s] = b
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

// SetReleaseTestingDone marks a release's testing complete — the finish line. This
// retires it from the active slot (ActiveRelease skips finished releases). by is the
// username who closed it out.
func (d *DB) SetReleaseTestingDone(ctx context.Context, id int, by string) error {
	_, err := d.pool.Exec(ctx, `UPDATE releases SET testing_done_at = NOW(), testing_done_by = $2 WHERE id = $1`, id, by)
	return err
}

// ClearReleaseTestingDone reopens a finished release, making it active again.
func (d *DB) ClearReleaseTestingDone(ctx context.Context, id int) error {
	_, err := d.pool.Exec(ctx, `UPDATE releases SET testing_done_at = NULL, testing_done_by = '' WHERE id = $1`, id)
	return err
}

// CreateBranchRelease forks an off-mainline branch build from parentID: a new release with
// is_branch + parent_release_id set. version must be unique. Returns the new release id.
func (d *DB) CreateBranchRelease(ctx context.Context, parentID int, version, name, changelog string) (int, error) {
	var id int
	err := d.pool.QueryRow(ctx, `
		INSERT INTO releases (version, name, changelog, is_branch, parent_release_id, product)
		VALUES ($1, $2, $3, true, $4, (SELECT product FROM releases WHERE id = $4)) RETURNING id`,
		version, name, changelog, parentID).Scan(&id)
	return id, err
}

// MergeBranch merges a validated branch onto the main line as a NEW mainline release at
// newVersion: a draft node that sits on main in the branch's lineage (its parent is what
// the branch forked from) and whose merged_from points back at the branch. The branch is
// then stamped merged into that node. Runs in one transaction; the WHERE guard on the base
// lookup rejects a non-branch or already-merged source. Returns the new release id.
//
// The merge carries the branch's delta onto the new release:
//   - QA ledger (always): the branch's results on shared BASE cases seed the new release's
//     QA, and its still-open problems carry over as open problems on main.
//   - Build (carryBuild): the branch's newest full OTA package + active QFIL bundles are
//     copied onto the new release, relabelled to newVersion. Opt-in, because a firmware
//     image usually must be rebuilt to report the new version rather than relabelled.
func (d *DB) MergeBranch(ctx context.Context, branchID int, branchVersion, newVersion, name, changelog, mergedBy string, carryBuild bool) (int, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	var baseID *int // what the branch forked from; the new node lands on that line
	// Enforce the "branch testing must be done" gate at the SQL level too (the handler
	// also checks it) so no other caller — or a race that clears testing_done between
	// the handler's read and here — can merge an untested branch. No row → ErrNoRows →
	// the merge aborts.
	if err := tx.QueryRow(ctx,
		`SELECT parent_release_id FROM releases WHERE id = $1 AND is_branch AND merged_into_release_id IS NULL AND testing_done_at IS NOT NULL`,
		branchID).Scan(&baseID); err != nil {
		return 0, err
	}
	var newID int
	if err := tx.QueryRow(ctx, `
		INSERT INTO releases (version, name, changelog, is_branch, parent_release_id, merged_from_release_id, product)
		VALUES ($1, $2, $3, false, $4, $5, (SELECT product FROM releases WHERE id = $5)) RETURNING id`,
		newVersion, name, changelog, baseID, branchID).Scan(&newID); err != nil {
		return 0, err
	}
	if _, err := tx.Exec(ctx,
		`UPDATE releases SET merged_into_release_id = $2, merged_at = NOW(), merged_by = $3 WHERE id = $1`,
		branchID, newID, mergedBy); err != nil {
		return 0, err
	}

	// Seed the new release's QA from the branch's results on shared BASE cases, so operators
	// pick up where the branch left off instead of a blank checklist.
	if _, err := tx.Exec(ctx, `
		INSERT INTO release_test_results (release_id, test_case_id, status, notes, tested_by, tested_at)
		SELECT $2, r.test_case_id, r.status, r.notes, r.tested_by, r.tested_at
		FROM release_test_results r JOIN test_cases c ON c.id = r.test_case_id
		WHERE r.release_id = $1 AND c.base
		ON CONFLICT (release_id, test_case_id) DO NOTHING`, branchID, newID); err != nil {
		return 0, err
	}
	// Carry the branch's still-open problems onto main as manual problems on the new
	// release (test_case link dropped — the branch's cases don't belong to the mainline
	// board). They land as 'open' regardless of their branch status: the branch's
	// fix trail (fixed_in/verified_in) is branch-scoped and not copied, so carrying a
	// 'fixed' status would leave an inconsistent 'fixed'-with-no-fix-build row that can
	// never be verified. On main they must be (re-)triaged and verified afresh.
	if _, err := tx.Exec(ctx, `
		INSERT INTO release_problems (release_id, build_id, title, description, severity, status, source, reported_by, created_at, updated_at)
		SELECT $2, $3, title,
		       CASE WHEN description = '' THEN '(carried from branch ' || $4 || ')'
		            ELSE description || E'\n\n(carried from branch ' || $4 || ')' END,
		       severity, 'open', 'manual', reported_by, NOW(), NOW()
		FROM release_problems
		WHERE release_id = $1 AND status NOT IN ('verified', 'wontfix')`,
		branchID, newID, newVersion, branchVersion); err != nil {
		return 0, err
	}

	if carryBuild {
		// Newest full OTA package, relabelled to the new version (one full per release, so
		// build_id = newVersion stays unique for the fresh node).
		if _, err := tx.Exec(ctx, `
			INSERT INTO ota_packages (release_id, type, target_build_id, source_build_id, build_id, update_url, changelog, status, release_date)
			SELECT $2, 'full', $3, '', $3, update_url, changelog, 'active', NOW()
			FROM ota_packages WHERE release_id = $1 AND status = 'active' AND type = 'full'
			ORDER BY created_at DESC LIMIT 1`, branchID, newID, newVersion); err != nil {
			return 0, err
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO qfil_packages (release_id, label, url, notes, added_by, status)
			SELECT $2, label, url, notes, added_by, 'active'
			FROM qfil_packages WHERE release_id = $1 AND status = 'active'`, branchID, newID); err != nil {
			return 0, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return newID, nil
}

// ListBranchReleases returns the branch builds forked from parentID, newest first, each
// with its open-problem count so the parent's Branches tab can summarise them.
func (d *DB) ListBranchReleases(ctx context.Context, parentID int) ([]Release, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, version, name, status, created_at, signed_off_by, testing_done_at
		FROM releases WHERE parent_release_id = $1 AND is_branch
		ORDER BY created_at DESC`, parentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		var r Release
		if err := rows.Scan(&r.ID, &r.Version, &r.Name, &r.Status, &r.CreatedAt, &r.SignedOffBy, &r.TestingDoneAt); err != nil {
			return nil, err
		}
		r.IsBranch = true
		pid := parentID
		r.ParentReleaseID = &pid
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetReleaseMeta updates the editable release fields (name, changelog).
func (d *DB) SetReleaseMeta(ctx context.Context, id int, name, changelog string) error {
	_, err := d.pool.Exec(ctx, `UPDATE releases SET name = $2, changelog = $3 WHERE id = $1`, id, name, changelog)
	return err
}

// SetReleaseName changes only a release's display name, leaving its changelog and
// other metadata untouched — the fleet toolbar's inline rename uses this so a quick
// rename can't blank the release notes.
func (d *DB) SetReleaseName(ctx context.Context, id int, name string) error {
	_, err := d.pool.Exec(ctx, `UPDATE releases SET name = $2 WHERE id = $1`, id, name)
	return err
}

// SetReleaseCreatedAt overrides a release's date. created_at is what the releases
// list orders by (newest first) and what the OTA resolver's newer/older ranking
// keys off, so editing it moves the release consistently in both.
func (d *DB) SetReleaseCreatedAt(ctx context.Context, id int, t time.Time) error {
	_, err := d.pool.Exec(ctx, `UPDATE releases SET created_at = $2 WHERE id = $1`, id, t)
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

// ListDeployments returns every deployment (newest first) with its release version
// and device counts — backs the global Updates hub.
func (d *DB) ListDeployments(ctx context.Context) ([]Update, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT u.id, COALESCE(u.ota_package_id, 0), COALESCE(u.release_id, 0), u.reboot_behavior,
		       u.scheduled_time, u.status, u.created_at, COALESCE(rel.version, ''), COALESCE(rel.product, 't7'),
		       COUNT(ud.device_id) AS device_total,
		       COUNT(CASE WHEN ud.status = 'installed' THEN 1 END) AS device_installed,
		       COUNT(CASE WHEN ud.status = 'downloading' THEN 1 END) AS device_downloading,
		       COUNT(CASE WHEN ud.status = 'failed' THEN 1 END) AS device_failed
		FROM updates u
		LEFT JOIN releases rel ON rel.id = u.release_id
		LEFT JOIN update_devices ud ON ud.update_id = u.id
		GROUP BY u.id, rel.version, rel.product
		ORDER BY u.created_at DESC
		LIMIT 200
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Update
	for rows.Next() {
		var u Update
		var version, product string
		if err := rows.Scan(&u.ID, &u.OtaPackageID, &u.ReleaseID, &u.RebootBehavior, &u.ScheduledTime,
			&u.Status, &u.CreatedAt, &version, &product, &u.DeviceTotal, &u.DeviceInstalled,
			&u.DeviceDownloading, &u.DeviceFailed); err != nil {
			return nil, err
		}
		u.Product = product
		u.Release = &Release{ID: u.ReleaseID, Version: version, Product: product}
		out = append(out, u)
	}
	return out, rows.Err()
}

// ListDeployableReleases returns published releases that have at least one active OTA
// package — the choices offered in the Updates-hub deploy composer.
func (d *DB) ListDeployableReleases(ctx context.Context) ([]Release, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT r.id, r.version, r.name, r.created_at
		FROM releases r
		JOIN ota_packages p ON p.release_id = r.id AND p.status = 'active'
		WHERE r.status = 'published' AND NOT r.hidden
		ORDER BY r.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		var rel Release
		if err := rows.Scan(&rel.ID, &rel.Version, &rel.Name, &rel.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, rel)
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
func (d *DB) SendUpdateToDevices(ctx context.Context, updateID int, deviceIDs []uuid.UUID, forceFull bool) error {
	// One transaction so target inserts and the activation commit together — a
	// mid-loop failure previously left the deployment armed with missing targets,
	// and the per-row insert error was silently discarded.
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	for _, did := range deviceIDs {
		// force_full pins these rows to the full image so ResolveUpdateForDevice never
		// offers a matching incremental — the operator explicitly chose full delivery.
		if _, err := tx.Exec(ctx, `
			INSERT INTO update_devices (update_id, device_id, status, force_full)
			VALUES ($1, $2, 'pending', $3)
			ON CONFLICT DO NOTHING
		`, updateID, did, forceFull); err != nil {
			return err
		}
		// A device only reaches here because it has no other active/incomplete
		// update (callers only pass already-eligible devices) — so the sole thing
		// that could still block this fresh command is a *stale* failed OTA from
		// an earlier, now-complete deployment: HasPendingOTACommand treats a
		// failure as "still pending" for an hour specifically to stop automatic
		// checkin-driven re-sends from hammering a device. Creating a brand-new
		// deployment is a deliberate, explicit retry, so clear that guard here —
		// otherwise the new row sits at "pending" with nothing ever actually sent
		// until the hour lapses, on both the immediate push and the later
		// checkin-driven resolve path.
		if _, err := tx.Exec(ctx, `
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
		`, did); err != nil {
			return err
		}
	}
	// Activate the deployment. Reactivate a 'complete' one too: adding targets to
	// a finished deployment must re-arm it, or the new pending rows are stranded
	// (ResolveUpdateForDevice only serves status='active').
	if _, err := tx.Exec(ctx, `UPDATE updates SET status = 'active' WHERE id = $1 AND status IN ('pending', 'complete')`, updateID); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
		  -- Per-product safety gate: never resolve a release whose product differs from the
		  -- device's own. A legacy/empty device product counts as t7 (the pre-product fleet).
		  AND rel.product = CASE WHEN d.product = '' THEN 't7' ELSE d.product END
		  -- Monotonicity gate: never OTA a release that is older than (or the same as) the
		  -- release the device is already running. Order within a product is (created_at, id),
		  -- both creation-monotonic. If the device's current build isn't a managed release we
		  -- can't prove a regression, so we allow it (NOT EXISTS is true).
		  AND NOT EXISTS (
		      SELECT 1 FROM releases cur
		      WHERE cur.product = rel.product
		        AND cur.version = d.build_id
		        AND (cur.created_at, cur.id) >= (rel.created_at, rel.id)
		  )
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

// ListAwaitingRebootForUpdate returns the device IDs of an update's targets that have
// installed to the inactive slot and are waiting for a reboot (status 'awaiting_reboot').
// Used by the deployment page's "reboot all installed" bulk action.
func (d *DB) ListAwaitingRebootForUpdate(ctx context.Context, updateID int) ([]uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT device_id FROM update_devices
		WHERE update_id = $1 AND status = 'awaiting_reboot'
	`, updateID)
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

// ListStaleRebootSent returns devices stuck at 'reboot_sent' on an active
// deployment for longer than staleMinutes — the reboot command was likely lost or
// declined, so it must be re-issued. Without this a device that took the OTA but
// never rebooted sits in limbo forever, keeping the deployment 'active'.
func (d *DB) ListStaleRebootSent(ctx context.Context, staleMinutes int) ([]DueReboot, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT ud.update_id, ud.device_id
		FROM update_devices ud
		JOIN updates u ON u.id = ud.update_id
		WHERE u.status = 'active'
		  AND ud.status = 'reboot_sent'
		  AND ud.updated_at < NOW() - ($1 * INTERVAL '1 minute')
	`, staleMinutes)
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
			completed_at = CASE WHEN $3 = 'installed'   AND completed_at IS NULL THEN NOW() ELSE completed_at END,
			reboot_sent_at = CASE WHEN $3 = 'reboot_sent' AND reboot_sent_at IS NULL THEN NOW() ELSE reboot_sent_at END
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

// OptimisticallyCompleteReboot marks one update_devices row installed and updates
// the device's tracked build_id to the update's resolved target build, the moment
// a reboot command is pushed for it — without waiting for a confirming checkin.
// The durable confirmation path (CompleteUpdatesAtTargetBuild, below) relies on the
// device checking back in against THIS server; in a multi-instance deployment where
// a device may check in against a different MDM instance after rebooting, that
// confirmation can structurally never arrive here, permanently stranding the row at
// awaiting_reboot/reboot_sent. Deliberate accepted tradeoff: if the reboot silently
// fails or an A/B slot rolls back, this row won't self-correct, since it's already
// terminal by the time any contradicting checkin could arrive.
//
// No-op (not an error) if the row isn't currently awaiting_reboot/reboot_sent, so a
// redrive of an already-completed row is safe to call again.
func (d *DB) OptimisticallyCompleteReboot(ctx context.Context, updateID int, deviceID uuid.UUID) error {
	// Same per-device package-selection rule as ResolveUpdateForDevice's LATERAL
	// join: prefer the incremental whose source build matches the device's current
	// build, else the full image.
	var targetBuildID string
	err := d.pool.QueryRow(ctx, `
		SELECT p.target_build_id
		FROM update_devices ud
		JOIN updates u ON u.id = ud.update_id
		JOIN devices dv ON dv.id = ud.device_id
		JOIN LATERAL (
			SELECT pk.target_build_id FROM ota_packages pk
			WHERE pk.release_id = u.release_id AND pk.status = 'active'
			  AND (pk.type = 'full'
			       OR (pk.type = 'incremental' AND NOT ud.force_full AND pk.source_build_id = dv.build_id))
			ORDER BY (pk.type = 'incremental' AND NOT ud.force_full AND pk.source_build_id = dv.build_id) DESC, pk.created_at DESC
			LIMIT 1
		) p ON true
		WHERE ud.update_id = $1 AND ud.device_id = $2
	`, updateID, deviceID).Scan(&targetBuildID)
	if err != nil {
		return err
	}

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	tag, err := tx.Exec(ctx, `
		UPDATE update_devices SET status = 'installed', updated_at = NOW()
		WHERE update_id = $1 AND device_id = $2 AND status IN ('awaiting_reboot', 'reboot_sent')
	`, updateID, deviceID)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return nil // already installed/failed/etc — nothing to do (safe re-drive no-op)
	}
	if _, err := tx.Exec(ctx, `UPDATE devices SET build_id = $2 WHERE id = $1`, deviceID, targetBuildID); err != nil {
		return err
	}
	return tx.Commit(ctx)
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
		  -- Include 'canceled' deployments so a device that was mid-flight when the
		  -- deployment was canceled still gets its row reconciled to 'installed' when
		  -- it lands on the target build (it was told to finish), rather than being
		  -- stranded at 'downloading'/'reboot_sent'.
		  AND u.status IN ('active', 'canceled')
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

// ClearUpdateDeviceForceFull un-pins a device from the full image, so
// ResolveUpdateForDevice is free to offer a matching incremental again. Used by a
// plain (non-"full image") retry — without this, a device that was ever force-fulled
// (an earlier full-image retry, or the pre-delivery-choice retry button) would stay
// pinned to full forever, since force_full is otherwise only ever set, never cleared.
func (d *DB) ClearUpdateDeviceForceFull(ctx context.Context, updateID int, deviceID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE update_devices SET force_full = false, updated_at = NOW()
		WHERE update_id = $1 AND device_id = $2
	`, updateID, deviceID)
	return err
}

// CheckAndCompleteUpdate marks an update as "complete" if all its targets are "installed".
func (d *DB) CheckAndCompleteUpdate(ctx context.Context, updateID int) error {
	// A deployment is complete once every device has reached a TERMINAL state.
	// 'installed', 'failed' and 'canceled' are all terminal for the device — using
	// only "!= 'installed'" left a deployment stuck 'active' forever the moment any
	// device failed or was canceled (there is no periodic sweep to unstick it).
	_, err := d.pool.Exec(ctx, `
		UPDATE updates SET status = 'complete'
		WHERE id = $1 AND status = 'active'
		AND NOT EXISTS (
			SELECT 1 FROM update_devices
			WHERE update_id = $1 AND status NOT IN ('installed', 'failed', 'canceled')
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
	// Atomic: cancel the pending device rows and the deployment together, so a
	// failure between the two can't leave devices canceled while the deployment
	// stays 'active' (a half-cancelled state).
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `
		UPDATE update_devices SET status = 'canceled', error_code = '', updated_at = NOW()
		WHERE update_id = $1 AND status = 'pending'
	`, updateID); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE updates SET status = 'canceled' WHERE id = $1 AND status = 'active'
	`, updateID); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// GetUpdateTargets returns the device targets for an update.
func (d *DB) GetUpdateTargets(ctx context.Context, updateID int) ([]UpdateTarget, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT ud.update_id, ud.device_id, d.serial_number, d.build_id, ud.status, ud.error_code, ud.updated_at,
		       ud.started_at, ud.completed_at, ud.reboot_sent_at
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
		if err := rows.Scan(&t.UpdateID, &t.DeviceID, &t.SerialNumber, &t.BuildID, &t.Status, &t.ErrorCode, &t.UpdatedAt, &t.StartedAt, &t.CompletedAt, &t.RebootSentAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// HasPendingOTACommand reports whether an OTA is already in flight for a device, so
// the check-in resolver doesn't create a second one on top of it. Two independent
// self-heal properties, both hardening against FW-2026-000XXX (a device stuck
// "downloading 0%" forever because an abandoned/superseded OTA command sat at
// 'delivered' and nothing ever recognized it as done):
//  1. 'expired'/'cancelled' count as terminal here, matching what
//     ExpireOverdueCommands (a 24h backstop for exactly this situation) actually
//     marks a stalled command as — previously this check didn't accept either, so
//     even after that sweep ran, a device stayed permanently blocked anyway.
//  2. A staleness cutoff on its own (30m — long enough that a normal check-in cadence
//     won't mistake genuine in-progress work for stuck, short enough that a truly
//     stuck device self-heals on close to its next check-in instead of staying
//     visibly "stuck" on the dashboard for hours).
// GetActiveOTACommandID returns the most recent non-terminal OTA command targeting
// this device, or ok=false if there is none — used to address a cancel_command frame
// at the right command id (the device dedups cancels by id, so this must match
// exactly what it's currently working on).
func (d *DB) GetActiveOTACommandID(ctx context.Context, deviceID uuid.UUID) (id uuid.UUID, ok bool, err error) {
	err = d.pool.QueryRow(ctx, `
		SELECT c.id FROM commands c
		JOIN command_targets ct ON ct.command_id = c.id AND ct.target_id = $1
		LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = $1
		WHERE c.type = 'ota' AND (cs.status IS NULL OR cs.status NOT IN ('installed', 'failed', 'completed', 'expired', 'cancelled'))
		ORDER BY c.created_at DESC LIMIT 1
	`, deviceID).Scan(&id)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, err
	}
	return id, true, nil
}

func (d *DB) HasPendingOTACommand(ctx context.Context, deviceID uuid.UUID) (bool, error) {
	var exists bool
	err := d.pool.QueryRow(ctx, `
		SELECT EXISTS (
			SELECT 1 FROM commands c
			JOIN command_targets ct ON ct.command_id = c.id AND ct.target_id = $1
			LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = $1
			WHERE c.type = 'ota'
			AND (
				(
					(cs.status IS NULL OR cs.status NOT IN ('installed', 'failed', 'completed', 'expired', 'cancelled'))
					AND COALESCE(cs.updated_at, c.created_at) > NOW() - INTERVAL '30 minutes'
				)
				OR (cs.status = 'failed' AND cs.updated_at > NOW() - INTERVAL '1 hour')
			)
		)
	`, deviceID).Scan(&exists)
	return exists, err
}

// CreateOTACommandIfNone atomically creates an OTA command for a device unless one
// is already in flight, returning nil when one already exists. A Postgres advisory
// lock keyed on the device serializes concurrent check-ins (a device using both
// HTTP check-in and WS telemetry, or two rapid check-ins) so they can't both pass
// the no-pending check and dispatch duplicate OTA downloads.
func (d *DB) CreateOTACommandIfNone(ctx context.Context, deviceID uuid.UUID, payload json.RawMessage) (*Command, error) {
	var cmd *Command
	err := d.withAdvisoryLock(ctx, advisoryKeyUUID(deviceID), func(ctx context.Context) error {
		hasPending, err := d.HasPendingOTACommand(ctx, deviceID)
		if err != nil {
			return err
		}
		if hasPending {
			return nil
		}
		cmd, err = d.CreateCommand(ctx, "ota", "", payload, "devices", []uuid.UUID{deviceID})
		return err
	})
	return cmd, err
}

// TryCreateOTACommand builds and inserts the OTA command for an already-resolved
// update/device pair, if applicable — an incremental package only applies when
// currentBuildID matches its source build (a full package always applies). Returns
// (nil, nil), not an error, when not applicable or one is already in flight
// (CreateOTACommandIfNone's own advisory-locked check). On success also flips the
// device's update_devices row to "downloading". Shared by the checkin resolver
// (internal/api) and the dashboard's deploy-time immediate push so the exact
// conditions for creating an OTA command can't drift between the two call sites.
func (d *DB) TryCreateOTACommand(ctx context.Context, upd *Update, deviceID uuid.UUID, currentBuildID string) (*Command, error) {
	pkg := upd.OtaPackage
	if pkg == nil {
		return nil, nil
	}
	if pkg.Type == "incremental" && pkg.SourceBuildID != currentBuildID {
		return nil, nil
	}
	p := map[string]any{
		"package_id":      pkg.ID,
		"build_id":        pkg.TargetBuildID,
		"update_url":      pkg.UpdateURL,
		"reboot_behavior": upd.RebootBehavior,
	}
	if upd.ScheduledTime != nil {
		p["scheduled_time"] = upd.ScheduledTime.UTC().Format(time.RFC3339)
	}
	payload, err := json.Marshal(p)
	if err != nil {
		return nil, err
	}
	cmd, err := d.CreateOTACommandIfNone(ctx, deviceID, payload)
	if err != nil || cmd == nil {
		return cmd, err
	}
	if err := d.SetUpdateDeviceStatus(ctx, upd.ID, deviceID, "downloading"); err != nil {
		return nil, err
	}
	return cmd, nil
}

// withAdvisoryLock runs fn while holding a Postgres session advisory lock for key,
// serializing callers so a check-then-insert cannot race into duplicate rows. The
// lock lives on one pooled connection and is released even if ctx is cancelled.
func (d *DB) withAdvisoryLock(ctx context.Context, key int64, fn func(context.Context) error) error {
	conn, err := d.pool.Acquire(ctx)
	if err != nil {
		return err
	}
	defer conn.Release()
	if _, err := conn.Exec(ctx, `SELECT pg_advisory_lock($1)`, key); err != nil {
		return err
	}
	defer conn.Exec(context.Background(), `SELECT pg_advisory_unlock($1)`, key)
	return fn(ctx)
}

// advisoryKeyUUID / advisoryKeyIntUUID derive a stable advisory-lock key from a
// uuid, or an (int, uuid) pair. Occasional key collisions only cause unrelated
// operations to serialize briefly, which is harmless.
func advisoryKeyUUID(id uuid.UUID) int64 { return int64(binary.BigEndian.Uint64(id[:8])) }
func advisoryKeyIntUUID(n int, id uuid.UUID) int64 {
	return int64(n)<<32 | int64(binary.BigEndian.Uint32(id[:4]))
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

// DeleteSessionsForUser removes every session belonging to one user — used after a
// password reset so a stolen/old session can't outlive the credential change.
func (d *DB) DeleteSessionsForUser(ctx context.Context, userID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM sessions WHERE user_id = $1`, userID)
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

// EnsureFixVerificationCase adds a "Verify fix: …" case to releaseID's checklist for the
// given problem (so the test team confirms the fix), unless one already exists for that
// (release, problem). Title/steps are taken from the problem. Called when a carried-over
// bug is marked "fixed here".
func (d *DB) EnsureFixVerificationCase(ctx context.Context, releaseID int, problemID uuid.UUID, createdBy string) error {
	// Advisory lock so two concurrent submits can't both pass NOT EXISTS and insert
	// duplicate "Verify fix" cases (there's no unique constraint to lean on).
	return d.withAdvisoryLock(ctx, advisoryKeyIntUUID(releaseID, problemID), func(ctx context.Context) error {
		_, err := d.pool.Exec(ctx, `
			INSERT INTO test_cases (title, area, steps, expected_result, base, release_id, created_by, problem_id)
			SELECT 'Verify fix: ' || p.title, 'Regression',
			       COALESCE(NULLIF(p.description, ''), 'Confirm the reported issue no longer occurs.'),
			       'Issue no longer reproduces.', false, $1, $2, p.id
			FROM release_problems p
			WHERE p.id = $3
			  AND NOT EXISTS (SELECT 1 FROM test_cases WHERE release_id = $1 AND problem_id = $3)`,
			releaseID, createdBy, problemID)
		return err
	})
}

// VerifyProblemForCase verifies the problem linked to a test case (if any) on releaseID —
// called when that "Verify fix" case is marked pass, closing the loop that a manual
// carried-over bug is fixed and confirmed. No-op when the case links no problem.
func (d *DB) VerifyProblemForCase(ctx context.Context, caseID uuid.UUID, releaseID int) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE release_problems
		SET status = 'verified', verified_in_release_id = $2,
		    fixed_in_release_id = COALESCE(fixed_in_release_id, $2), updated_at = NOW()
		WHERE id = (SELECT problem_id FROM test_cases WHERE id = $1 AND problem_id IS NOT NULL)`,
		caseID, releaseID)
	return err
}

// ReopenProblemForCase reopens the problem linked to a test case (if any) — called when a
// "Verify fix" case fails, meaning the fix didn't hold. The bug goes back to open but its
// fixed_in_release_id is KEPT (it records the dev's claim, which is now disputed), so the
// trail reads "reported v2.0.2 → dev says fixed in v2.0.3 → QA says still broken".
// The carry-forward query handles open bugs with a fixed_in set.
// Returns true when a linked problem was found and reopened.
func (d *DB) ReopenProblemForCase(ctx context.Context, caseID uuid.UUID) (bool, error) {
	tag, err := d.pool.Exec(ctx, `
		UPDATE release_problems
		SET status = 'open', updated_at = NOW()
		WHERE id = (SELECT problem_id FROM test_cases WHERE id = $1 AND problem_id IS NOT NULL)`,
		caseID)
	if err != nil {
		return false, err
	}
	return tag.RowsAffected() > 0, nil
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

// SetTestResult records (upserts) an operator's outcome for one case on one release.
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

// ── Release problem reports ───────────────────────────────────────────────────

// ReleaseProblem is a tracked problem an operator filed against a release. TestCase
// and Device are optional; the joined title/serial are populated for display.
type ReleaseProblem struct {
	ID            uuid.UUID
	ReleaseID     int
	TestCaseID    *uuid.UUID
	TestCaseTitle string
	DeviceID      *uuid.UUID
	DeviceSerial  string
	BuildID       string
	Title         string
	Description   string
	Severity      string // blocker | major | minor
	Status        string // open | fixed | verified | wontfix
	Source        string // manual | qa (auto-created from a failed QA case)
	ReportedBy    string
	CreatedAt     time.Time
	UpdatedAt     time.Time

	// Continuity ("rides the release train"): a manual bug reported on one build can be
	// fixed in a later build and verified there. OriginVersion is release_id's version.
	OriginVersion       string // version the problem was first reported on (= ReleaseID)
	FixedInReleaseID    *int   // build a dev claims the fix landed in; nil until claimed
	FixedInVersion      string // joined version of FixedInReleaseID, "" when unset
	VerifiedInReleaseID *int   // build an operator confirmed the fix on; nil until verified
	VerifiedInVersion   string // joined version of VerifiedInReleaseID, "" when unset
	Inherited           bool   // true when surfaced on a release later than its origin (carried forward)
}

// ProblemSummary is the per-release rollup used by badges: how many problems are
// still open, and how many of those are blockers.
type ProblemSummary struct {
	Open     int
	Blockers int
	Total    int
}

var validProblemSeverity = map[string]bool{"blocker": true, "major": true, "minor": true}
var validProblemStatus = map[string]bool{"open": true, "fixed": true, "verified": true, "wontfix": true}

// A problem still counts as "open" (needs attention) unless it's verified-fixed
// or explicitly won't-fix.
func problemIsOpen(status string) bool { return status != "verified" && status != "wontfix" }

// CreateReleaseProblem files a new problem. testCaseID/deviceID may be nil.
func (d *DB) CreateReleaseProblem(ctx context.Context, p ReleaseProblem) (uuid.UUID, error) {
	if !validProblemSeverity[p.Severity] {
		p.Severity = "major"
	}
	var id uuid.UUID
	err := d.pool.QueryRow(ctx, `
		INSERT INTO release_problems
			(release_id, test_case_id, device_id, build_id, title, description, severity, reported_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8) RETURNING id`,
		p.ReleaseID, p.TestCaseID, p.DeviceID, p.BuildID, p.Title, p.Description, p.Severity, p.ReportedBy,
	).Scan(&id)
	return id, err
}

// releaseProblemCols / releaseProblemJoins are the canonical column list and joins for
// selecting problems with their origin/fixed/verified release versions and the joined
// case title + device serial. Shared by ListReleaseProblems, CarriedForwardProblems and
// ListProblemBoard so the scan order stays in lock-step (see scanReleaseProblem).
const releaseProblemCols = `
	p.id, p.release_id, orel.version,
	p.test_case_id, COALESCE(tc.title, ''),
	p.device_id, COALESCE(d.serial_number, ''), p.build_id,
	p.title, p.description, p.severity, p.status, p.source, p.reported_by,
	p.fixed_in_release_id, COALESCE(fr.version, ''),
	p.verified_in_release_id, COALESCE(vr.version, ''),
	p.created_at, p.updated_at`

const releaseProblemJoins = `
	FROM release_problems p
	JOIN releases orel ON orel.id = p.release_id
	LEFT JOIN test_cases tc ON tc.id = p.test_case_id
	LEFT JOIN devices d ON d.id = p.device_id
	LEFT JOIN releases fr ON fr.id = p.fixed_in_release_id
	LEFT JOIN releases vr ON vr.id = p.verified_in_release_id`

// releaseProblemOrder sorts open-first, then blocker→minor, then newest.
const releaseProblemOrder = `
	ORDER BY
		(p.status NOT IN ('verified','wontfix')) DESC,
		CASE p.severity WHEN 'blocker' THEN 0 WHEN 'major' THEN 1 ELSE 2 END,
		p.created_at DESC`

// scanReleaseProblem scans one row selected with releaseProblemCols. It does NOT set
// Inherited — the caller sets that based on which release it queried for.
func scanReleaseProblem(rows pgx.Rows, p *ReleaseProblem) error {
	return rows.Scan(&p.ID, &p.ReleaseID, &p.OriginVersion,
		&p.TestCaseID, &p.TestCaseTitle,
		&p.DeviceID, &p.DeviceSerial, &p.BuildID,
		&p.Title, &p.Description, &p.Severity, &p.Status, &p.Source, &p.ReportedBy,
		&p.FixedInReleaseID, &p.FixedInVersion,
		&p.VerifiedInReleaseID, &p.VerifiedInVersion,
		&p.CreatedAt, &p.UpdatedAt)
}

// ListReleaseProblems returns a release's OWN (native) problems — those reported against
// it — open first then by severity (blocker → minor) and newest. Carried-over problems
// from earlier builds are returned separately by CarriedForwardProblems.
func (d *DB) ListReleaseProblems(ctx context.Context, releaseID int) ([]ReleaseProblem, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT`+releaseProblemCols+releaseProblemJoins+`
		WHERE p.release_id = $1`+releaseProblemOrder, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReleaseProblem
	for rows.Next() {
		var p ReleaseProblem
		if err := scanReleaseProblem(rows, &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// CarriedForwardProblems returns manual problems that should follow the release train
// onto releaseID but were NOT reported against it: (a) problems that were reported fixed
// in an EARLIER build but whose QA verification then FAILED (reopened to 'open' while
// keeping fixed_in_release_id) — these ride forward until a fix holds — and (b) problems
// whose fix is claimed to land in THIS build (awaiting an operator's verification here). A
// plain still-open bug that was never claimed fixed does NOT carry; it stays on its origin
// build. All rows are flagged Inherited. Chronology is releases.created_at.
func (d *DB) CarriedForwardProblems(ctx context.Context, releaseID int) ([]ReleaseProblem, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT`+releaseProblemCols+releaseProblemJoins+`
		WHERE p.release_id <> $1 AND p.source = 'manual'
		  AND NOT orel.is_branch                                        -- branch bugs never carry onto mainline
		  AND EXISTS (SELECT 1 FROM releases WHERE id = $1 AND NOT is_branch) -- branches inherit nothing
		  AND (
			p.fixed_in_release_id = $1                                  -- dev claimed fix in THIS build (verify here)
			OR (p.status = 'open' AND p.fixed_in_release_id IS NOT NULL
			    AND (SELECT created_at FROM releases WHERE id = p.fixed_in_release_id)
			        < (SELECT created_at FROM releases WHERE id = $1))
			    -- reported fixed in an earlier build but QA verification failed (reopened,
			    -- fixed_in kept); only rides onto builds created AFTER the claimed-fix
			    -- build — a build predating the fix claim can't have the fix pending
		)`+releaseProblemOrder, releaseID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReleaseProblem
	for rows.Next() {
		var p ReleaseProblem
		if err := scanReleaseProblem(rows, &p); err != nil {
			return nil, err
		}
		p.Inherited = true
		out = append(out, p)
	}
	return out, rows.Err()
}

// ListProblemBoard returns every problem across all releases (open first), for the hub's
// cross-release "all the problems at a glance" board. Each row is canonical (origin =
// ReleaseID / OriginVersion); Inherited is left false.
func (d *DB) ListProblemBoard(ctx context.Context) ([]ReleaseProblem, error) {
	rows, err := d.pool.Query(ctx,
		`SELECT`+releaseProblemCols+releaseProblemJoins+`
		WHERE NOT orel.is_branch`+releaseProblemOrder) // branch bugs are off-mainline, not on the board
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReleaseProblem
	for rows.Next() {
		var p ReleaseProblem
		if err := scanReleaseProblem(rows, &p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// UpsertQAProblem is called when a QA test case is marked failed: it opens a
// linked problem for (release, test_case) with source='qa', or reopens the
// existing QA problem if it had been resolved (a re-fail after a fix). Title comes
// from the case, build from the release; severity defaults to major.
func (d *DB) UpsertQAProblem(ctx context.Context, releaseID int, testCaseID uuid.UUID, notes, reportedBy string) error {
	if strings.TrimSpace(notes) == "" {
		notes = "Marked failed in QA."
	}
	// Advisory lock so two operators failing the same case at once can't both pass
	// NOT EXISTS and insert duplicate QA problems (no unique constraint exists).
	if err := d.withAdvisoryLock(ctx, advisoryKeyIntUUID(releaseID, testCaseID), func(ctx context.Context) error {
		_, err := d.pool.Exec(ctx, `
			INSERT INTO release_problems
				(release_id, test_case_id, build_id, title, description, severity, status, reported_by, source)
			SELECT $1, $2,
			       COALESCE((SELECT version FROM releases WHERE id = $1), ''),
			       'QA fail: ' || COALESCE((SELECT title FROM test_cases WHERE id = $2), 'test case'),
			       $3, 'major', 'open', $4, 'qa'
			WHERE NOT EXISTS (
				SELECT 1 FROM release_problems WHERE release_id = $1 AND test_case_id = $2 AND source = 'qa')
		`, releaseID, testCaseID, notes, reportedBy)
		return err
	}); err != nil {
		return err
	}
	// Reopen (and refresh notes on) an existing QA problem that had been closed.
	_, err := d.pool.Exec(ctx, `
		UPDATE release_problems SET status = 'open', description = $3, updated_at = NOW()
		WHERE release_id = $1 AND test_case_id = $2 AND source = 'qa'
		  AND status IN ('verified', 'wontfix')
	`, releaseID, testCaseID, notes)
	return err
}

// ResolveQAProblem is called when a QA case is marked passed: it marks the linked
// QA-sourced problem verified (two-way sync). No-op if there is none or it's
// already closed.
func (d *DB) ResolveQAProblem(ctx context.Context, releaseID int, testCaseID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE release_problems SET status = 'verified', updated_at = NOW()
		WHERE release_id = $1 AND test_case_id = $2 AND source = 'qa'
		  AND status NOT IN ('verified', 'wontfix')
	`, releaseID, testCaseID)
	return err
}

// UpdateReleaseProblem changes a problem's status and/or severity. Empty values
// leave the corresponding field unchanged.
func (d *DB) UpdateReleaseProblem(ctx context.Context, id uuid.UUID, status, severity string) error {
	if status != "" && !validProblemStatus[status] {
		return fmt.Errorf("invalid status %q", status)
	}
	if severity != "" && !validProblemSeverity[severity] {
		return fmt.Errorf("invalid severity %q", severity)
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE release_problems
		SET status   = COALESCE(NULLIF($2, ''), status),
		    severity = COALESCE(NULLIF($3, ''), severity),
		    updated_at = NOW()
		WHERE id = $1`, id, status, severity)
	return err
}

// DeleteReleaseProblem removes a problem outright (admin only).
func (d *DB) DeleteReleaseProblem(ctx context.Context, id uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM release_problems WHERE id = $1`, id)
	return err
}

// MarkProblemFixedIn records that a dev landed the fix for a problem in fixedInReleaseID
// and moves it to 'fixed' (awaiting an operator's verification on that build). Passing 0
// clears the fixed-in link and reopens the problem.
func (d *DB) MarkProblemFixedIn(ctx context.Context, id uuid.UUID, fixedInReleaseID int) error {
	if fixedInReleaseID <= 0 {
		// Reopen: clear the whole fix/verify trail so the bug rides the train again.
		_, err := d.pool.Exec(ctx, `
			UPDATE release_problems
			SET fixed_in_release_id = NULL, verified_in_release_id = NULL,
			    status = 'open', updated_at = NOW()
			WHERE id = $1`, id)
		return err
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE release_problems
		SET fixed_in_release_id = $2, status = 'fixed', updated_at = NOW()
		WHERE id = $1`, id, fixedInReleaseID)
	return err
}

// VerifyProblem marks a problem verified-fixed on verifiedInReleaseID (an operator confirmed
// the fix on that build). If no fixed-in build was recorded, the verifying build is taken
// as the fix build too.
func (d *DB) VerifyProblem(ctx context.Context, id uuid.UUID, verifiedInReleaseID int) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE release_problems
		SET verified_in_release_id = $2,
		    fixed_in_release_id    = COALESCE(fixed_in_release_id, $2),
		    status = 'verified', updated_at = NOW()
		WHERE id = $1`, id, verifiedInReleaseID)
	return err
}

// GlobalProblemSummary returns the open/blocker/total rollup across every release, for
// the hub's headline "all the problems at a glance" stat.
func (d *DB) GlobalProblemSummary(ctx context.Context) (ProblemSummary, error) {
	var s ProblemSummary
	err := d.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE status NOT IN ('verified','wontfix')),
			COUNT(*) FILTER (WHERE status NOT IN ('verified','wontfix') AND severity='blocker'),
			COUNT(*)
		FROM release_problems
		WHERE release_id IN (SELECT id FROM releases WHERE NOT is_branch)`).Scan(&s.Open, &s.Blockers, &s.Total)
	return s, err
}

// ActiveRelease returns the release currently "under test" — the newest non-hidden
// release whose testing is NOT yet done (testing_done_at IS NULL), preferring a draft.
// Marking testing done retires a release from this slot so the next build takes over. It
// anchors the hub focus band and is the default carry-forward target. Returns (nil, nil)
// when every release is hidden or finished.
func (d *DB) ActiveRelease(ctx context.Context) (*Release, error) {
	var r Release
	err := d.pool.QueryRow(ctx, `
		SELECT id, version, name, changelog, status, hidden, skip_base_tests, created_at, published_at,
		       signed_off_by, signed_off_at, testing_done_at, testing_done_by
		FROM releases
		WHERE NOT hidden AND testing_done_at IS NULL AND NOT is_branch
		ORDER BY (status = 'draft') DESC, created_at DESC
		LIMIT 1`).Scan(&r.ID, &r.Version, &r.Name, &r.Changelog, &r.Status, &r.Hidden, &r.SkipBaseTests,
		&r.CreatedAt, &r.PublishedAt, &r.SignedOffBy, &r.SignedOffAt, &r.TestingDoneAt, &r.TestingDoneBy)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return nil, nil
		}
		return nil, err
	}
	return &r, nil
}

// ReleaseProblemSummary returns the open/blocker/total counts for one release.
func (d *DB) ReleaseProblemSummary(ctx context.Context, releaseID int) (ProblemSummary, error) {
	var s ProblemSummary
	err := d.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (WHERE status NOT IN ('verified','wontfix')),
			COUNT(*) FILTER (WHERE status NOT IN ('verified','wontfix') AND severity='blocker'),
			COUNT(*)
		FROM release_problems WHERE release_id = $1`, releaseID).Scan(&s.Open, &s.Blockers, &s.Total)
	return s, err
}

// BuildCrash is a crash/ANR event observed on a device currently running a given
// build — surfaced on the release page so operators see regressions to investigate.
type BuildCrash struct {
	ID         uuid.UUID // device_events.id — lets an admin remove a specific crash
	Serial     string
	Kind       string
	Summary    string
	Detail     string // full DropBox trace (stack / ANR / tombstone), may be empty
	OccurredAt time.Time
}

// LatestCrashTrace returns the full stored trace of the most recent crash/ANR/
// tombstone event for a device (not a reboot), for the crash alert detail panel.
// ok is false when there is no event or the event carried no trace body.
func (d *DB) LatestCrashTrace(ctx context.Context, deviceID uuid.UUID) (string, bool, error) {
	var detail string
	err := d.pool.QueryRow(ctx, `
		SELECT detail FROM device_events
		WHERE device_id = $1 AND kind <> 'reboot'
		ORDER BY occurred_at DESC LIMIT 1`, deviceID).Scan(&detail)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return "", false, nil
		}
		return "", false, err
	}
	return detail, detail != "", nil
}

// CrashesOnBuild returns recent crash/ANR/tombstone events (not reboots) from
// devices currently reporting buildID, newest first.
func (d *DB) CrashesOnBuild(ctx context.Context, buildID string, limit int) ([]BuildCrash, error) {
	if buildID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 20
	}
	// Filter by the build the event was reported on (e.build_id), NOT the device's
	// current build (dv.build_id) — otherwise a device that has since updated drags
	// its old crashes onto the new release's page.
	rows, err := d.pool.Query(ctx, `
		SELECT e.id, dv.serial_number, e.kind, e.summary, e.detail, e.occurred_at
		FROM device_events e
		JOIN devices dv ON dv.id = e.device_id
		WHERE e.build_id = $1 AND e.kind <> 'reboot'
		ORDER BY e.occurred_at DESC
		LIMIT $2`, buildID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BuildCrash
	for rows.Next() {
		var c BuildCrash
		if err := rows.Scan(&c.ID, &c.Serial, &c.Kind, &c.Summary, &c.Detail, &c.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CrashEvent is one crash/ANR/tombstone event (a device_events row, not a reboot),
// with the device's serial/restaurant/build for listing on the alerts page and the
// per-device Alerts tab. Detail is the full DropBox trace (may be empty).
type CrashEvent struct {
	ID         uuid.UUID
	Serial     string
	Restaurant string
	Kind       string
	Summary    string
	Detail     string
	BuildID    string
	OccurredAt time.Time
}

// ListRecentCrashEvents returns crash/ANR/tombstone events (not reboots) across the
// whole fleet within the last sinceDays, newest first — the fleet crash feed folded
// into the alerts page. Events on hidden devices are excluded. limit/sinceDays <= 0
// fall back to 60 / 7.
func (d *DB) ListRecentCrashEvents(ctx context.Context, sinceDays, limit int) ([]CrashEvent, error) {
	if limit <= 0 {
		limit = 60
	}
	if sinceDays <= 0 {
		sinceDays = 7
	}
	rows, err := d.pool.Query(ctx, `
		SELECT e.id, dv.serial_number, COALESCE(r.name, ''), e.kind, e.summary, e.detail,
		       e.build_id, e.occurred_at
		FROM device_events e
		JOIN devices dv ON dv.id = e.device_id
		LEFT JOIN restaurants r ON r.id = dv.restaurant_id
		WHERE e.kind NOT IN ('reboot', 'kiosk_exit_offline') AND NOT dv.hidden
		  AND e.occurred_at > now() - make_interval(days => $1)
		ORDER BY e.occurred_at DESC
		LIMIT $2`, sinceDays, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CrashEvent
	for rows.Next() {
		var c CrashEvent
		if err := rows.Scan(&c.ID, &c.Serial, &c.Restaurant, &c.Kind, &c.Summary,
			&c.Detail, &c.BuildID, &c.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListDeviceCrashes returns crash/ANR/tombstone events (not reboots) for a single
// device, newest first — the crash feed for that device's Alerts tab. limit <= 0
// means 50.
// CountDeviceCrashes is the badge count for the device page's Alerts tab — same
// filter as ListDeviceCrashes, without pulling the (large) trace payloads.
func (d *DB) CountDeviceCrashes(ctx context.Context, deviceID uuid.UUID) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM device_events e
		WHERE e.device_id = $1 AND e.kind NOT IN ('reboot', 'kiosk_exit_offline')`, deviceID).Scan(&n)
	return n, err
}

func (d *DB) ListDeviceCrashes(ctx context.Context, deviceID uuid.UUID, limit int) ([]CrashEvent, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.pool.Query(ctx, `
		SELECT e.id, dv.serial_number, COALESCE(r.name, ''), e.kind, e.summary, e.detail,
		       e.build_id, e.occurred_at
		FROM device_events e
		JOIN devices dv ON dv.id = e.device_id
		LEFT JOIN restaurants r ON r.id = dv.restaurant_id
		WHERE e.device_id = $1 AND e.kind NOT IN ('reboot', 'kiosk_exit_offline')
		ORDER BY e.occurred_at DESC
		LIMIT $2`, deviceID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []CrashEvent
	for rows.Next() {
		var c CrashEvent
		if err := rows.Scan(&c.ID, &c.Serial, &c.Restaurant, &c.Kind, &c.Summary,
			&c.Detail, &c.BuildID, &c.OccurredAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ListRecentCrashEventsPage returns one page of fleet crash/ANR/tombstone events
// (not reboots) within the last sinceDays, newest first, plus the total number of
// matches (via COUNT(*) OVER()) for pagination. Events on hidden devices are excluded.
func (d *DB) ListRecentCrashEventsPage(ctx context.Context, sinceDays, limit, offset int) ([]CrashEvent, int, error) {
	if limit <= 0 {
		limit = 25
	}
	if offset < 0 {
		offset = 0
	}
	if sinceDays <= 0 {
		sinceDays = 7
	}
	rows, err := d.pool.Query(ctx, `
		SELECT e.id, dv.serial_number, COALESCE(r.name, ''), e.kind, e.summary, e.detail,
		       e.build_id, e.occurred_at, COUNT(*) OVER() AS total
		FROM device_events e
		JOIN devices dv ON dv.id = e.device_id
		LEFT JOIN restaurants r ON r.id = dv.restaurant_id
		WHERE e.kind NOT IN ('reboot', 'kiosk_exit_offline') AND NOT dv.hidden
		  AND e.occurred_at > now() - make_interval(days => $1)
		ORDER BY e.occurred_at DESC
		LIMIT $2 OFFSET $3`, sinceDays, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []CrashEvent
	total := 0
	for rows.Next() {
		var c CrashEvent
		if err := rows.Scan(&c.ID, &c.Serial, &c.Restaurant, &c.Kind, &c.Summary,
			&c.Detail, &c.BuildID, &c.OccurredAt, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// ListDeviceCrashesPage returns one page of a single device's crash/ANR/tombstone
// events (not reboots), newest first, plus the total match count for pagination.
func (d *DB) ListDeviceCrashesPage(ctx context.Context, deviceID uuid.UUID, limit, offset int) ([]CrashEvent, int, error) {
	if limit <= 0 {
		limit = 25
	}
	if offset < 0 {
		offset = 0
	}
	rows, err := d.pool.Query(ctx, `
		SELECT e.id, dv.serial_number, COALESCE(r.name, ''), e.kind, e.summary, e.detail,
		       e.build_id, e.occurred_at, COUNT(*) OVER() AS total
		FROM device_events e
		JOIN devices dv ON dv.id = e.device_id
		LEFT JOIN restaurants r ON r.id = dv.restaurant_id
		WHERE e.device_id = $1 AND e.kind NOT IN ('reboot', 'kiosk_exit_offline')
		ORDER BY e.occurred_at DESC
		LIMIT $2 OFFSET $3`, deviceID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []CrashEvent
	total := 0
	for rows.Next() {
		var c CrashEvent
		if err := rows.Scan(&c.ID, &c.Serial, &c.Restaurant, &c.Kind, &c.Summary,
			&c.Detail, &c.BuildID, &c.OccurredAt, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

// ListDeviceActiveAlerts returns the non-resolved alerts for a single device, worst
// severity first then newest — the alert feed for that device's Alerts tab.
func (d *DB) ListDeviceActiveAlerts(ctx context.Context, deviceID uuid.UUID, limit int) ([]Alert, error) {
	if limit <= 0 {
		limit = 50
	}
	rows, err := d.pool.Query(ctx, `
		SELECT a.id, a.rule_id, a.type, a.device_id, COALESCE(d.serial_number, ''),
		       COALESCE(r.name, ''), a.severity, a.status, a.summary, a.detail,
		       a.occurrences, a.fired_at, a.last_seen_at, a.resolved_at, a.updated_at
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
			&a.Occurrences, &a.FiredAt, &a.LastSeenAt, &a.ResolvedAt, &a.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// DeviceQuery is one admin-curated diagnostic: a friendly label + the shell command
// it runs. Surfaced on the device page as a "retrieve property" button. The command
// text originates only here (never user input), so lower roles may run it safely.
type DeviceQuery struct {
	ID          int
	Label       string
	Description string
	Command     string
	Category    string
	Sort        int
	Enabled     bool
	CreatedBy   string
	CreatedAt   time.Time
}

const deviceQueryCols = `id, label, description, command, category, sort, enabled, created_by, created_at`

func scanDeviceQuery(row interface{ Scan(...any) error }, q *DeviceQuery) error {
	return row.Scan(&q.ID, &q.Label, &q.Description, &q.Command, &q.Category, &q.Sort, &q.Enabled, &q.CreatedBy, &q.CreatedAt)
}

// ListDeviceQueries returns the whole catalog (admin view), ordered for display.
func (d *DB) ListDeviceQueries(ctx context.Context) ([]DeviceQuery, error) {
	rows, err := d.pool.Query(ctx, `SELECT `+deviceQueryCols+` FROM device_queries ORDER BY category, sort, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceQuery
	for rows.Next() {
		var q DeviceQuery
		if err := scanDeviceQuery(rows, &q); err != nil {
			return nil, err
		}
		out = append(out, q)
	}
	return out, rows.Err()
}

// ListEnabledDeviceQueries returns only enabled queries — what a run panel offers.
func (d *DB) ListEnabledDeviceQueries(ctx context.Context) ([]DeviceQuery, error) {
	all, err := d.ListDeviceQueries(ctx)
	if err != nil {
		return nil, err
	}
	out := all[:0]
	for _, q := range all {
		if q.Enabled {
			out = append(out, q)
		}
	}
	return out, nil
}

// GetDeviceQuery returns one query by id.
func (d *DB) GetDeviceQuery(ctx context.Context, id int) (DeviceQuery, error) {
	var q DeviceQuery
	err := scanDeviceQuery(d.pool.QueryRow(ctx, `SELECT `+deviceQueryCols+` FROM device_queries WHERE id = $1`, id), &q)
	return q, err
}

// CreateDeviceQuery inserts a new catalog entry and returns its id.
func (d *DB) CreateDeviceQuery(ctx context.Context, q DeviceQuery) (int, error) {
	var id int
	err := d.pool.QueryRow(ctx, `
		INSERT INTO device_queries (label, description, command, category, sort, enabled, created_by)
		VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING id`,
		q.Label, q.Description, q.Command, q.Category, q.Sort, q.Enabled, q.CreatedBy).Scan(&id)
	return id, err
}

// UpdateDeviceQuery edits an existing catalog entry.
func (d *DB) UpdateDeviceQuery(ctx context.Context, q DeviceQuery) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE device_queries SET label=$1, description=$2, command=$3, category=$4, sort=$5, enabled=$6 WHERE id=$7`,
		q.Label, q.Description, q.Command, q.Category, q.Sort, q.Enabled, q.ID)
	return err
}

// SetDeviceQueryEnabled flips a query's enabled flag.
func (d *DB) SetDeviceQueryEnabled(ctx context.Context, id int, enabled bool) error {
	_, err := d.pool.Exec(ctx, `UPDATE device_queries SET enabled=$1 WHERE id=$2`, enabled, id)
	return err
}

// DeleteDeviceQuery removes a catalog entry.
func (d *DB) DeleteDeviceQuery(ctx context.Context, id int) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM device_queries WHERE id = $1`, id)
	return err
}

// CrashGroup collapses identical crash/ANR events on a build into one row: same kind +
// normalized signature, counted across occurrences and distinct devices. The signature
// strips the volatile ", for safety source: X" tail so every variant of one underlying
// crash groups together. Sample* describe the newest occurrence (for the trace expander
// and the "Report" seed).
type CrashGroup struct {
	Kind         string
	Signature    string // normalized summary, shown as the group title
	Count        int    // total occurrences
	DeviceCount  int    // distinct devices affected
	LastOccurred time.Time
	SampleSerial string // newest occurrence's device serial
	SampleDetail string // newest non-empty trace, for the expander (may be empty)
}

// crashSig normalizes a crash summary column into a grouping signature: it strips the
// volatile ", for safety source: X" tail so all variants of one crash collapse into one
// group. col is the qualified summary column (e.g. "e.summary" or "summary").
func crashSig(col string) string {
	return `btrim(regexp_replace(` + col + `, ',?\s*for safety source:.*$', '', 'i'))`
}

// CrashGroupsOnBuild returns crash/ANR events on buildID collapsed into signature groups
// (busiest first), plus the total number of groups (for pagination). Pass limit<=0 for a
// default page size; offset pages through the groups. Scoped by e.build_id — the build the
// event was reported on — so a device that has since updated doesn't drag old crashes
// forward. See CrashGroup for the grouping rule.
func (d *DB) CrashGroupsOnBuild(ctx context.Context, buildID string, limit, offset int) ([]CrashGroup, int, error) {
	if buildID == "" {
		return nil, 0, nil
	}
	if limit <= 0 {
		limit = 20
	}
	if offset < 0 {
		offset = 0
	}
	sig := crashSig("e.summary")
	rows, err := d.pool.Query(ctx, `
		WITH grp AS (
			SELECT e.kind AS kind,
			       `+sig+` AS sig,
			       COUNT(*) AS cnt,
			       COUNT(DISTINCT e.device_id) AS devs,
			       MAX(e.occurred_at) AS last_at,
			       (array_agg(dv.serial_number ORDER BY e.occurred_at DESC))[1] AS sample_serial,
			       (array_agg(e.detail ORDER BY (e.detail <> '') DESC, e.occurred_at DESC))[1] AS sample_detail
			FROM device_events e
			JOIN devices dv ON dv.id = e.device_id
			WHERE e.build_id = $1 AND e.kind <> 'reboot'
			GROUP BY e.kind, `+sig+`
		)
		SELECT kind, sig, cnt, devs, last_at, sample_serial, sample_detail,
		       COUNT(*) OVER() AS total_groups
		FROM grp
		ORDER BY cnt DESC, last_at DESC
		LIMIT $2 OFFSET $3`, buildID, limit, offset)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []CrashGroup
	total := 0
	for rows.Next() {
		var g CrashGroup
		if err := rows.Scan(&g.Kind, &g.Signature, &g.Count, &g.DeviceCount, &g.LastOccurred,
			&g.SampleSerial, &g.SampleDetail, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, g)
	}
	return out, total, rows.Err()
}

// DeleteCrashGroup removes every crash event on buildID matching kind + the given
// normalized signature (an admin clearing a noisy group) and returns the distinct device
// IDs affected, so the caller can clear each device's crash alert too.
func (d *DB) DeleteCrashGroup(ctx context.Context, buildID, kind, signature string) ([]uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `
		DELETE FROM device_events
		WHERE build_id = $1 AND kind = $2 AND `+crashSig("summary")+` = $3
		RETURNING device_id`, buildID, kind, signature)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	seen := map[uuid.UUID]bool{}
	var out []uuid.UUID
	for rows.Next() {
		var id uuid.UUID
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// DeviceCrash is a per-device crash rollup for the Fleet Health page: how many
// crash/ANR/tombstone events a unit reported in the last 24h, its latest kind and
// build, when, and the restaurant it belongs to. Ordered worst-first by the query.
type DeviceCrash struct {
	DeviceID     uuid.UUID
	Serial       string
	RestaurantID *uuid.UUID
	Restaurant   string
	Kind         string
	Count        int
	BuildID      string
	LatestAt     time.Time
}

// FleetCrashStats aggregates recent crashes for the Fleet Health page: the 24h
// totals, the worst build, a 7-day daily sparkline, the worst devices, and a
// per-restaurant 24h count (so the triage list can show a crash chip).
type FleetCrashStats struct {
	Total24h     int
	Devices24h   int
	WorstBuild   string
	WorstBuildN  int
	Daily        []int             // crash count per day, oldest→newest, last 7 incl. today
	ByDevice     []DeviceCrash     // worst-first, capped at the caller's limit
	ByRestaurant map[uuid.UUID]int // restaurant_id → 24h crash count
}

// GetFleetCrashStats rolls up crash/ANR/tombstone events (not reboots) across the
// visible fleet for the health page. limit caps the worst-devices list.
func (d *DB) GetFleetCrashStats(ctx context.Context, limit int) (FleetCrashStats, error) {
	st := FleetCrashStats{ByRestaurant: map[uuid.UUID]int{}}
	if limit <= 0 {
		limit = 6
	}
	// Per-device 24h rollup — drives Total24h, Devices24h, ByDevice and ByRestaurant.
	rows, err := d.pool.Query(ctx, `
		SELECT d.id, d.serial_number, d.restaurant_id, COALESCE(r.name, ''),
		       COUNT(*),
		       (array_agg(e.kind ORDER BY e.occurred_at DESC))[1],
		       (array_agg(NULLIF(e.build_id, '') ORDER BY e.occurred_at DESC))[1],
		       MAX(e.occurred_at)
		FROM device_events e
		JOIN devices d ON d.id = e.device_id
		LEFT JOIN restaurants r ON r.id = d.restaurant_id
		WHERE e.kind <> 'reboot' AND NOT d.hidden
		  AND e.occurred_at > NOW() - INTERVAL '24 hours'
		GROUP BY d.id, d.serial_number, d.restaurant_id, r.name
		ORDER BY COUNT(*) DESC, MAX(e.occurred_at) DESC`)
	if err != nil {
		return st, err
	}
	defer rows.Close()
	for rows.Next() {
		var c DeviceCrash
		var build *string
		if err := rows.Scan(&c.DeviceID, &c.Serial, &c.RestaurantID, &c.Restaurant,
			&c.Count, &c.Kind, &build, &c.LatestAt); err != nil {
			return st, err
		}
		if build != nil {
			c.BuildID = *build
		}
		st.Total24h += c.Count
		st.Devices24h++
		if c.RestaurantID != nil {
			st.ByRestaurant[*c.RestaurantID] += c.Count
		}
		if len(st.ByDevice) < limit {
			st.ByDevice = append(st.ByDevice, c)
		}
	}
	if err := rows.Err(); err != nil {
		return st, err
	}

	// Worst build in the last 24h (most crashes). Ignored if there are none.
	_ = d.pool.QueryRow(ctx, `
		SELECT COALESCE(NULLIF(e.build_id, ''), 'unknown'), COUNT(*)
		FROM device_events e JOIN devices d ON d.id = e.device_id
		WHERE e.kind <> 'reboot' AND NOT d.hidden
		  AND e.occurred_at > NOW() - INTERVAL '24 hours'
		GROUP BY 1 ORDER BY 2 DESC, 1 LIMIT 1`).Scan(&st.WorstBuild, &st.WorstBuildN)

	// Crashes per day for the last 7 days (incl. today), zero-filled, UTC day buckets.
	st.Daily = make([]int, 7)
	drows, err := d.pool.Query(ctx, `
		SELECT COALESCE(x.c, 0)
		FROM generate_series((CURRENT_DATE - 6)::date, CURRENT_DATE::date, INTERVAL '1 day') g
		LEFT JOIN (
			SELECT date_trunc('day', e.occurred_at)::date AS day, COUNT(*) c
			FROM device_events e JOIN devices d ON d.id = e.device_id
			WHERE e.kind <> 'reboot' AND NOT d.hidden AND e.occurred_at >= CURRENT_DATE - 6
			GROUP BY 1
		) x ON x.day = g::date
		ORDER BY g`)
	if err != nil {
		return st, err
	}
	defer drows.Close()
	for i := 0; drows.Next() && i < 7; i++ {
		if err := drows.Scan(&st.Daily[i]); err != nil {
			return st, err
		}
	}
	return st, drows.Err()
}

// DeleteCrashEvent removes a single crash/ANR/tombstone event (admin cleanup of a
// noisy or irrelevant crash). Returns the device it belonged to so the caller can
// also clear that device's crash alert. ok is false if the event didn't exist.
func (d *DB) DeleteCrashEvent(ctx context.Context, eventID uuid.UUID) (deviceID uuid.UUID, ok bool, err error) {
	err = d.pool.QueryRow(ctx, `DELETE FROM device_events WHERE id = $1 RETURNING device_id`, eventID).Scan(&deviceID)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return uuid.Nil, false, nil
		}
		return uuid.Nil, false, err
	}
	return deviceID, true, nil
}

// DeleteDeviceCrashAlert removes the open device_crash alert for a device, so deleting
// a crash also clears it from the Alerts page. If real crashes remain, the minute-ly
// evaluator re-raises an accurate alert; if none remain, it stays gone.
func (d *DB) DeleteDeviceCrashAlert(ctx context.Context, deviceID uuid.UUID) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM alerts WHERE type = 'device_crash' AND device_id = $1`, deviceID)
	return err
}

// ProblemSummariesByRelease returns the per-release problem rollup for every
// release in one query, for the releases list badges (avoids an N+1).
func (d *DB) ProblemSummariesByRelease(ctx context.Context) (map[int]ProblemSummary, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT release_id,
			COUNT(*) FILTER (WHERE status NOT IN ('verified','wontfix')),
			COUNT(*) FILTER (WHERE status NOT IN ('verified','wontfix') AND severity='blocker'),
			COUNT(*)
		FROM release_problems GROUP BY release_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int]ProblemSummary)
	for rows.Next() {
		var rid int
		var s ProblemSummary
		if err := rows.Scan(&rid, &s.Open, &s.Blockers, &s.Total); err != nil {
			return nil, err
		}
		out[rid] = s
	}
	return out, rows.Err()
}

// CarriedProblemCountsByRelease returns, per non-branch release, the count of
// problems that CARRY onto it (mirrors the CarriedForwardProblems rule) — all
// carried problems are active/awaiting-verification, so they count as open. Used
// to make the release-list problem badge match the workspace board (native +
// carried) instead of counting only native problems.
func (d *DB) CarriedProblemCountsByRelease(ctx context.Context) (map[int]ProblemSummary, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT r.id, COUNT(*), COUNT(*) FILTER (WHERE p.severity = 'blocker')
		FROM releases r
		JOIN release_problems p ON p.source = 'manual' AND p.release_id <> r.id
		JOIN releases orel ON orel.id = p.release_id AND NOT orel.is_branch
		WHERE NOT r.is_branch
		  AND (
		     p.fixed_in_release_id = r.id
		     OR (p.status = 'open' AND p.fixed_in_release_id IS NOT NULL
		         AND (SELECT created_at FROM releases WHERE id = p.fixed_in_release_id) < r.created_at)
		  )
		GROUP BY r.id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[int]ProblemSummary)
	for rows.Next() {
		var rid int
		var s ProblemSummary
		if err := rows.Scan(&rid, &s.Open, &s.Blockers); err != nil {
			return nil, err
		}
		out[rid] = s
	}
	return out, rows.Err()
}
