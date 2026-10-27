package db

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	prod "mdm/internal/product"
)

// The role model (static/device-role-model-plan.html): every device has a role, every
// role runs one AIO app, and a device at a venue stays on that app's prod build without
// anyone pressing a button. Lab work happens in a timed lab session.
//
//   - role_apps        role → the app family's base package (TSAI → aio.app.nugget)
//   - app_channels     the build each package of that family is on: the base package is
//                      prod, every other package of the family (….internal, ….uatv2) is a
//                      lab channel of its own
//   - firmware_channels the prod firmware release for one hardware model (product key)
//   - product_names.firmware_mode  per model: prod | lab | off
//   - device_lab       a device's open lab session; while it runs, nothing here moves it
//   - lab_installs     packages installed during a session, removed when it ends
//
// Nothing in this file changes a device by itself. The minute jobs read it and act only
// where a pointer is set, so an empty table means "leave everything alone".
const roleModelSchema = `
CREATE TABLE IF NOT EXISTS role_apps (
	role         TEXT PRIMARY KEY,
	base_package TEXT NOT NULL,
	updated_by   TEXT NOT NULL DEFAULT '',
	updated_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- Lineup › Apps (A1, 23 Oct 2026): off stops RunRoleAppInstalls for the role.
ALTER TABLE role_apps ADD COLUMN IF NOT EXISTS auto_install BOOLEAN NOT NULL DEFAULT true;
-- Host app per product per venue (H1, 27 Oct 2026; user: "venue config for host app should
-- be per host app"): role → channel suffix, a missing role = prod. The one channel a venue
-- had for every product becomes that channel for each product with an app, once.
ALTER TABLE restaurants ADD COLUMN IF NOT EXISTS host_app_channels JSONB NOT NULL DEFAULT '{}';
UPDATE restaurants r
   SET host_app_channels = COALESCE((SELECT jsonb_object_agg(a.role, r.host_app_channel) FROM role_apps a WHERE a.base_package <> ''), '{}'),
       host_app_channel = ''
 WHERE r.host_app_channel <> '';
-- Auto-upgrade (27 Oct 2026): units on an older build than the library's newest are moved
-- to it, inside the update window. On by default (user: "keep the fleet on the newest").
ALTER TABLE role_apps ADD COLUMN IF NOT EXISTS auto_upgrade BOOLEAN NOT NULL DEFAULT true;
CREATE TABLE IF NOT EXISTS app_channels (
	role       TEXT NOT NULL,
	package    TEXT NOT NULL,
	app_id     UUID REFERENCES apps(id) ON DELETE SET NULL,
	set_by     TEXT NOT NULL DEFAULT '',
	set_at     TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (role, package)
);
CREATE TABLE IF NOT EXISTS app_channel_history (
	id       BIGSERIAL PRIMARY KEY,
	role     TEXT NOT NULL,
	package  TEXT NOT NULL,
	app_id   UUID REFERENCES apps(id) ON DELETE SET NULL,
	version  TEXT NOT NULL DEFAULT '',
	set_by   TEXT NOT NULL DEFAULT '',
	set_at   TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX IF NOT EXISTS idx_app_channel_history ON app_channel_history (role, package, set_at DESC);
CREATE TABLE IF NOT EXISTS firmware_channels (
	model         TEXT PRIMARY KEY,
	release_id    INTEGER REFERENCES releases(id) ON DELETE SET NULL,
	deployment_id INTEGER,
	set_by        TEXT NOT NULL DEFAULT '',
	set_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);
ALTER TABLE product_names ADD COLUMN IF NOT EXISTS firmware_mode TEXT NOT NULL DEFAULT '';
CREATE TABLE IF NOT EXISTS device_lab (
	device_id  UUID PRIMARY KEY REFERENCES devices(id) ON DELETE CASCADE,
	until      TIMESTAMPTZ NOT NULL,
	reason     TEXT NOT NULL DEFAULT '',
	opened_by  TEXT NOT NULL DEFAULT '',
	opened_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
	package    TEXT NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS lab_installs (
	device_id    UUID NOT NULL REFERENCES devices(id) ON DELETE CASCADE,
	package_name TEXT NOT NULL,
	installed_at TIMESTAMPTZ NOT NULL DEFAULT now(),
	PRIMARY KEY (device_id, package_name)
);
`

// Firmware modes for a hardware model (product key).
const (
	FirmwareProd = "prod" // deployed devices follow the model's prod firmware
	FirmwareLab  = "lab"  // builds go to lab devices only; deployed devices keep what they have
	FirmwareOff  = "off"  // not ours to update (stock hardware on the Standard client)
)

// DefaultFirmwareMode is the mode a model has before anyone sets one: our firmware
// products follow the decisions of 14 Oct (T7 prod, kiosks lab only); everything else
// is stock hardware and off.
func DefaultFirmwareMode(model string) string {
	model = prod.Normalize(model)
	p, ok := prod.Resolve(model)
	if !ok || p.Kind != prod.KindFirmware {
		return FirmwareOff
	}
	if p.Class == prod.ClassKiosk {
		return FirmwareLab
	}
	return FirmwareProd
}

// IsFirmwareMode reports whether s is one of the three modes.
func IsFirmwareMode(s string) bool {
	return s == FirmwareProd || s == FirmwareLab || s == FirmwareOff
}

// FirmwareModes returns the effective firmware mode of every model that has an explicit
// one; a model missing from the map takes DefaultFirmwareMode.
func (d *DB) FirmwareModes(ctx context.Context) (map[string]string, error) {
	rows, err := d.pool.Query(ctx, `SELECT product, firmware_mode FROM product_names WHERE firmware_mode <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, m string
		if err := rows.Scan(&k, &m); err != nil {
			return nil, err
		}
		out[k] = m
	}
	return out, rows.Err()
}

// FirmwareModeOf resolves one model's mode from the explicit map, else its default.
func FirmwareModeOf(modes map[string]string, model string) string {
	model = prod.Normalize(model)
	if model == "" {
		model = prod.DefaultKey
	}
	if m, ok := modes[model]; ok && IsFirmwareMode(m) {
		return m
	}
	return DefaultFirmwareMode(model)
}

// SetFirmwareMode records a model's firmware mode.
func (d *DB) SetFirmwareMode(ctx context.Context, model, mode, by string) error {
	if !IsFirmwareMode(mode) {
		return errors.New("unknown firmware mode")
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO product_names (product, name, firmware_mode, updated_by, updated_at) VALUES ($1, '', $2, $3, now())
		ON CONFLICT (product) DO UPDATE SET firmware_mode = EXCLUDED.firmware_mode, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		prod.Normalize(model), mode, by)
	return err
}

// ── role apps ────────────────────────────────────────────────────────────────

// RoleApp is the app a role runs, named by its family's base package.
type RoleApp struct {
	Role        string
	BasePackage string
	UpdatedBy   string
	UpdatedAt   time.Time
	AutoInstall bool // units missing the prod package get it (RunRoleAppInstalls)
	AutoUpgrade bool // units on an older build get the library's newest (same job)
}

func (d *DB) ListRoleApps(ctx context.Context) ([]RoleApp, error) {
	rows, err := d.pool.Query(ctx, `SELECT role, base_package, updated_by, updated_at, auto_install, auto_upgrade FROM role_apps ORDER BY role`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []RoleApp
	for rows.Next() {
		var a RoleApp
		if err := rows.Scan(&a.Role, &a.BasePackage, &a.UpdatedBy, &a.UpdatedAt, &a.AutoInstall, &a.AutoUpgrade); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// SetRoleApp names (or, with an empty package, clears) the app a role runs. A cleared
// role keeps its row with an empty package, so the one-time seed never fills it again.
// Clearing also drops the role's channels: with no app there is nothing for them to point at.
func (d *DB) SetRoleApp(ctx context.Context, role, basePackage, by string) error {
	role, basePackage = strings.TrimSpace(role), strings.TrimSpace(basePackage)
	defer d.roleApps.drop()
	if basePackage == "" {
		tx, err := d.pool.Begin(ctx)
		if err != nil {
			return err
		}
		defer tx.Rollback(ctx)
		if _, err := tx.Exec(ctx, `
			INSERT INTO role_apps (role, base_package, updated_by, updated_at) VALUES ($1, '', $2, now())
			ON CONFLICT (role) DO UPDATE SET base_package = '', updated_by = EXCLUDED.updated_by, updated_at = now()`, role, by); err != nil {
			return err
		}
		if _, err := tx.Exec(ctx, `DELETE FROM app_channels WHERE role = $1`, role); err != nil {
			return err
		}
		return tx.Commit(ctx)
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO role_apps (role, base_package, updated_by, updated_at) VALUES ($1, $2, $3, now())
		ON CONFLICT (role) DO UPDATE SET base_package = EXCLUDED.base_package, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		role, basePackage, by)
	if err != nil {
		return err
	}
	// A channel for a package outside the (new) family is meaningless now.
	_, err = d.pool.Exec(ctx, `DELETE FROM app_channels WHERE role = $1 AND package <> $2 AND package NOT LIKE $2 || '.%'`, role, basePackage)
	return err
}

// seedRoleApps gives each role its starting app (prod.DefaultRoleApps) the first time,
// and never again: a role an admin changed or cleared already has its row.
func seedRoleApps(ctx context.Context, tx pgx.Tx) error {
	for role, base := range prod.DefaultRoleApps() {
		if _, err := tx.Exec(ctx, `INSERT INTO role_apps (role, base_package, updated_by) VALUES ($1, $2, 'seed') ON CONFLICT (role) DO NOTHING`, role, base); err != nil {
			return err
		}
	}
	return nil
}

// roleAppCache holds role → base package for the check-in path, which asks on every
// check-in. SetRoleApp drops it; otherwise it is re-read after roleAppTTL.
type roleAppCache struct {
	mu sync.Mutex
	at time.Time
	m  map[string]string
}

const roleAppTTL = 30 * time.Second

func (c *roleAppCache) drop() {
	c.mu.Lock()
	c.at = time.Time{}
	c.mu.Unlock()
}

// RoleAppBase is the base package of the app a role runs, "" for none.
func (d *DB) RoleAppBase(ctx context.Context, role string) string {
	m := d.RoleAppBases(ctx)
	return m[role]
}

// RoleAppBases is every role's app, from the cache when it is fresh. On a read error it
// serves what it had.
func (d *DB) RoleAppBases(ctx context.Context) map[string]string {
	c := &d.roleApps
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.m != nil && time.Since(c.at) < roleAppTTL {
		return c.m
	}
	apps, err := d.ListRoleApps(ctx)
	if err != nil {
		return c.m
	}
	m := make(map[string]string, len(apps))
	for _, a := range apps {
		m[a.Role] = a.BasePackage
	}
	c.m, c.at = m, time.Now()
	return m
}

// ── app channels ─────────────────────────────────────────────────────────────

// Channel is the build one package of a role's app is on. The base package's channel
// is prod; the others are lab channels.
type Channel struct {
	Role    string
	Package string
	App     *App // nil when the pointer was cleared or its build deleted
	SetBy   string
	SetAt   time.Time
}

// ListChannels returns every channel with its build joined.
func (d *DB) ListChannels(ctx context.Context) ([]Channel, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT c.role, c.package, c.set_by, c.set_at,
		       a.id, COALESCE(a.name, ''), COALESCE(a.apk_url, ''), COALESCE(a.package_name, ''), COALESCE(a.version_name, ''), a.created_at
		FROM app_channels c
		LEFT JOIN apps a ON a.id = c.app_id
		ORDER BY c.role, c.package`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Channel
	for rows.Next() {
		var c Channel
		var id *uuid.UUID
		var name, url, pkg, ver string
		var created *time.Time
		if err := rows.Scan(&c.Role, &c.Package, &c.SetBy, &c.SetAt, &id, &name, &url, &pkg, &ver, &created); err != nil {
			return nil, err
		}
		if id != nil {
			a := App{ID: *id, Name: name, ApkURL: url, PackageName: pkg, VersionName: ver}
			if created != nil {
				a.CreatedAt = *created
			}
			c.App = &a
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// ErrWrongPackage is returned when a build is put on a channel of another package.
var ErrWrongPackage = errors.New("that build is a different package")

// SetChannel points one package's channel at a build of that same package and writes
// the history row in the same transaction. A build of another package can never be put
// on a channel: the column is the package.
func (d *DB) SetChannel(ctx context.Context, role, pkg string, appID uuid.UUID, by string) error {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	var appPkg, ver string
	if err := tx.QueryRow(ctx, `SELECT COALESCE(package_name, ''), COALESCE(version_name, '') FROM apps WHERE id = $1`, appID).Scan(&appPkg, &ver); err != nil {
		return err
	}
	if appPkg != pkg {
		return ErrWrongPackage
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO app_channels (role, package, app_id, set_by, set_at) VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (role, package) DO UPDATE SET app_id = EXCLUDED.app_id, set_by = EXCLUDED.set_by, set_at = now()`,
		role, pkg, appID, by); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `INSERT INTO app_channel_history (role, package, app_id, version, set_by) VALUES ($1, $2, $3, $4, $5)`,
		role, pkg, appID, ver, by); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

// ChannelEntry is one past pointer move.
type ChannelEntry struct {
	Role, Package, Version, SetBy string
	AppID                         *uuid.UUID
	SetAt                         time.Time
}

// ChannelHistory returns the most recent moves per (role, package), newest first,
// at most perChannel each.
func (d *DB) ChannelHistory(ctx context.Context, perChannel int) ([]ChannelEntry, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT role, package, version, set_by, app_id, set_at FROM (
			SELECT *, row_number() OVER (PARTITION BY role, package ORDER BY set_at DESC) AS n
			FROM app_channel_history
		) h WHERE n <= $1 ORDER BY role, package, set_at DESC`, perChannel)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ChannelEntry
	for rows.Next() {
		var e ChannelEntry
		if err := rows.Scan(&e.Role, &e.Package, &e.Version, &e.SetBy, &e.AppID, &e.SetAt); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// ── firmware channels ────────────────────────────────────────────────────────

// FirmwareChannel is one model's prod firmware: a release, and the standing deployment
// the minute job adds devices to.
type FirmwareChannel struct {
	Model        string
	ReleaseID    *int
	Version      string
	DeploymentID *int
	SetBy        string
	SetAt        time.Time
}

func (d *DB) ListFirmwareChannels(ctx context.Context) ([]FirmwareChannel, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT f.model, f.release_id, COALESCE(r.version, ''), f.deployment_id, f.set_by, f.set_at
		FROM firmware_channels f LEFT JOIN releases r ON r.id = f.release_id
		ORDER BY f.model`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FirmwareChannel
	for rows.Next() {
		var c FirmwareChannel
		if err := rows.Scan(&c.Model, &c.ReleaseID, &c.Version, &c.DeploymentID, &c.SetBy, &c.SetAt); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// SetFirmwareChannel points a model's prod at a release. The standing deployment is
// reset: the job opens a new one for the new release on its next tick.
func (d *DB) SetFirmwareChannel(ctx context.Context, model string, releaseID int, by string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO firmware_channels (model, release_id, deployment_id, set_by, set_at) VALUES ($1, $2, NULL, $3, now())
		ON CONFLICT (model) DO UPDATE SET release_id = EXCLUDED.release_id, deployment_id = NULL, set_by = EXCLUDED.set_by, set_at = now()`,
		prod.Normalize(model), releaseID, by)
	return err
}

// SetFirmwareDeployment remembers the standing deployment the job created for a model.
func (d *DB) SetFirmwareDeployment(ctx context.Context, model string, deploymentID int) error {
	_, err := d.pool.Exec(ctx, `UPDATE firmware_channels SET deployment_id = $2 WHERE model = $1`, prod.Normalize(model), deploymentID)
	return err
}

// ── lab sessions ─────────────────────────────────────────────────────────────

// LabSession is an open lab session on one device.
type LabSession struct {
	DeviceID uuid.UUID
	Until    time.Time
	Reason   string
	OpenedBy string
	OpenedAt time.Time
	// Package is the app package the device should run while in lab ("" = leave the
	// app alone, whatever it has).
	Package string
}

// Remaining is how long the session has left, never negative.
func (s LabSession) Remaining() time.Duration {
	if r := time.Until(s.Until); r > 0 {
		return r
	}
	return 0
}

// RemainingSec is Remaining in whole seconds, for the dashboard's dur formatter.
func (s LabSession) RemainingSec() int { return int(s.Remaining().Seconds()) }

// GetLabSession returns the device's open session, or nil when it is not in lab. An
// expired row counts as closed even before the minute job removes it.
func (d *DB) GetLabSession(ctx context.Context, deviceID uuid.UUID) (*LabSession, error) {
	var s LabSession
	err := d.pool.QueryRow(ctx, `SELECT device_id, until, reason, opened_by, opened_at, package FROM device_lab WHERE device_id = $1 AND until > now()`, deviceID).
		Scan(&s.DeviceID, &s.Until, &s.Reason, &s.OpenedBy, &s.OpenedAt, &s.Package)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// LabSessions returns every open session, keyed by device.
func (d *DB) LabSessions(ctx context.Context) (map[uuid.UUID]LabSession, error) {
	rows, err := d.pool.Query(ctx, `SELECT device_id, until, reason, opened_by, opened_at, package FROM device_lab WHERE until > now()`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uuid.UUID]LabSession{}
	for rows.Next() {
		var s LabSession
		if err := rows.Scan(&s.DeviceID, &s.Until, &s.Reason, &s.OpenedBy, &s.OpenedAt, &s.Package); err != nil {
			return nil, err
		}
		out[s.DeviceID] = s
	}
	return out, rows.Err()
}

// OpenLab starts (or, when one is already open, replaces the end and reason of) a lab
// session. The package choice survives a re-open.
func (d *DB) OpenLab(ctx context.Context, deviceID uuid.UUID, until time.Time, reason, by string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_lab (device_id, until, reason, opened_by, opened_at) VALUES ($1, $2, $3, $4, now())
		ON CONFLICT (device_id) DO UPDATE SET until = EXCLUDED.until, reason = EXCLUDED.reason,
			opened_by = CASE WHEN device_lab.until > now() THEN device_lab.opened_by ELSE EXCLUDED.opened_by END,
			opened_at = CASE WHEN device_lab.until > now() THEN device_lab.opened_at ELSE now() END,
			package   = CASE WHEN device_lab.until > now() THEN device_lab.package ELSE '' END`,
		deviceID, until, strings.TrimSpace(reason), by)
	return err
}

// SetLabPackage chooses the app package a device in lab should run.
func (d *DB) SetLabPackage(ctx context.Context, deviceID uuid.UUID, pkg string) error {
	tag, err := d.pool.Exec(ctx, `UPDATE device_lab SET package = $2 WHERE device_id = $1 AND until > now()`, deviceID, pkg)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return errors.New("device is not in lab")
	}
	return nil
}

// EndLab closes a session and returns the packages installed during it, which the
// caller removes. Safe to call on a device that is not in lab.
func (d *DB) EndLab(ctx context.Context, deviceID uuid.UUID) ([]string, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, `DELETE FROM device_lab WHERE device_id = $1`, deviceID); err != nil {
		return nil, err
	}
	rows, err := tx.Query(ctx, `DELETE FROM lab_installs WHERE device_id = $1 RETURNING package_name`, deviceID)
	if err != nil {
		return nil, err
	}
	var pkgs []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			rows.Close()
			return nil, err
		}
		pkgs = append(pkgs, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return pkgs, tx.Commit(ctx)
}

// ExpiredLabDevices lists devices whose session has run out but has not been closed.
func (d *DB) ExpiredLabDevices(ctx context.Context) ([]uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `SELECT device_id FROM device_lab WHERE until <= now()`)
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

// RecordLabInstall remembers a package installed during a session so it is removed
// when the session ends.
func (d *DB) RecordLabInstall(ctx context.Context, deviceID uuid.UUID, pkg string) error {
	_, err := d.pool.Exec(ctx, `INSERT INTO lab_installs (device_id, package_name) VALUES ($1, $2) ON CONFLICT DO NOTHING`, deviceID, pkg)
	return err
}

// LabInstalls lists the packages installed during the device's session.
func (d *DB) LabInstalls(ctx context.Context, deviceID uuid.UUID) ([]string, error) {
	rows, err := d.pool.Query(ctx, `SELECT package_name FROM lab_installs WHERE device_id = $1 ORDER BY installed_at`, deviceID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var p string
		if err := rows.Scan(&p); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ── device package inventory ─────────────────────────────────────────────────

// FamilyVersions returns, per device, the version of every package it reports under
// one of the given package prefixes (a family's base package covers its variants).
func (d *DB) FamilyVersions(ctx context.Context, prefixes []string) (map[uuid.UUID]map[string]string, error) {
	out := map[uuid.UUID]map[string]string{}
	if len(prefixes) == 0 {
		return out, nil
	}
	likes := make([]string, 0, len(prefixes))
	exact := make([]string, 0, len(prefixes))
	for _, p := range prefixes {
		exact = append(exact, p)
		likes = append(likes, p+".%")
	}
	rows, err := d.pool.Query(ctx, `
		SELECT device_id, package_name, version_name FROM device_packages
		WHERE package_name = ANY($1) OR package_name LIKE ANY($2)`, exact, likes)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var pkg, ver string
		if err := rows.Scan(&id, &pkg, &ver); err != nil {
			return nil, err
		}
		m := out[id]
		if m == nil {
			m = map[string]string{}
			out[id] = m
		}
		m[pkg] = ver
	}
	return out, rows.Err()
}

// InFlightInstalls returns the devices that already have an install or uninstall of
// one of the packages on its way, so a minute job never queues a second one.
func (d *DB) InFlightInstalls(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT ct.target_id
		FROM commands c
		JOIN command_targets ct ON ct.command_id = c.id
		LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = ct.target_id
		WHERE c.type IN ('install_apk', 'uninstall')
		  AND c.target_type = 'devices'
		  AND ct.target_id = ANY($1)
		  AND c.created_at > now() - interval '2 hours'
		  AND (cs.status IS NULL OR cs.status IN ('pending', 'delivered', 'downloading', 'installing'))`, ids)
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

// FailedInstalls counts, per device and APK, the installs that failed in the window.
func (d *DB) FailedInstalls(ctx context.Context, ids []uuid.UUID, since time.Duration) (map[uuid.UUID]map[string]int, error) {
	out := map[uuid.UUID]map[string]int{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT cs.device_id, c.apk_url, count(*)
		FROM commands c JOIN command_status cs ON cs.command_id = c.id
		WHERE c.type = 'install_apk' AND cs.status = 'failed'
		  AND c.created_at > now() - make_interval(secs => $2)
		  AND cs.device_id = ANY($1)
		GROUP BY 1, 2`, ids, since.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var url string
		var n int
		if err := rows.Scan(&id, &url, &n); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[string]int{}
		}
		out[id][url] = n
	}
	return out, rows.Err()
}

// CommandOutcome is how one device finished one command: its status and stored output.
// Status is "" while nothing terminal has been recorded.
func (d *DB) CommandOutcome(ctx context.Context, commandID, deviceID uuid.UUID) (status, output string, err error) {
	err = d.pool.QueryRow(ctx, `
		SELECT COALESCE((SELECT status FROM command_status WHERE command_id = $1 AND device_id = $2), ''),
		       COALESCE((SELECT output FROM command_results WHERE command_id = $1 AND device_id = $2), '')`,
		commandID, deviceID).Scan(&status, &output)
	return status, output, err
}

// FleetFamily is one AIO app family the fleet reports, with how many devices carry any
// package of it.
type FleetFamily struct {
	Base    string
	Devices int
}

// FleetAppFamilies lists the aio.app.<name> families on any device, most widespread first.
// A variant (aio.app.nugget.uatv2) counts toward its family.
func (d *DB) FleetAppFamilies(ctx context.Context) ([]FleetFamily, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT split_part(package_name, '.', 1) || '.' || split_part(package_name, '.', 2) || '.' || split_part(package_name, '.', 3) AS fam,
		       count(DISTINCT device_id)
		FROM device_packages WHERE package_name LIKE 'aio.app.%'
		GROUP BY 1 ORDER BY 2 DESC, 1`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FleetFamily
	for rows.Next() {
		var f FleetFamily
		if err := rows.Scan(&f.Base, &f.Devices); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// DevicesWithoutPackage returns, of the given devices, those that have reported their
// installed apps and do not have pkg. A device that never reported a list is left out:
// its missing package is unknown, not missing.
func (d *DB) DevicesWithoutPackage(ctx context.Context, ids []uuid.UUID, pkg string) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT device_id FROM device_packages
		WHERE device_id = ANY($1)
		GROUP BY device_id
		HAVING bool_and(package_name <> $2)`, ids, pkg)
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

// TransientInstallSQL matches a failed install's result that is worth trying again soon:
// the download was cut (server restarted, Wi-Fi dropped), the server answered 5xx, or the
// stall sweep failed it with no word from the device.
const TransientInstallSQL = `(COALESCE(cr.output, '') = '' OR cr.output ~* '(unexpected end of stream|ProtocolException|SocketException|SocketTimeout|timeout|connection reset|HTTP 5[0-9][0-9])')`

// RecentInstallsOf returns, per device, which of these APKs it was sent within the
// window, whatever became of it, except a transient failure (TransientInstallSQL) older
// than retrySoon: that one may be tried again.
func (d *DB) RecentInstallsOf(ctx context.Context, ids []uuid.UUID, urls []string, within, retrySoon time.Duration) (map[uuid.UUID]map[string]bool, error) {
	out := map[uuid.UUID]map[string]bool{}
	if len(ids) == 0 || len(urls) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT ct.target_id, c.apk_url
		FROM commands c
		JOIN command_targets ct ON ct.command_id = c.id
		LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = ct.target_id
		LEFT JOIN command_results cr ON cr.command_id = c.id AND cr.device_id = ct.target_id
		WHERE c.type = 'install_apk'
		  AND ct.target_id = ANY($1)
		  AND c.apk_url = ANY($2)
		  AND c.created_at > now() - make_interval(secs => $3)
		  AND NOT (cs.status = 'failed' AND `+TransientInstallSQL+`
		           AND cs.updated_at <= now() - make_interval(secs => $4))`, ids, urls, within.Seconds(), retrySoon.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var url string
		if err := rows.Scan(&id, &url); err != nil {
			return nil, err
		}
		if out[id] == nil {
			out[id] = map[string]bool{}
		}
		out[id][url] = true
	}
	return out, rows.Err()
}

// SetRoleAppAutoInstall switches the prod app auto-install on or off for a role.
func (d *DB) SetRoleAppAutoInstall(ctx context.Context, role string, on bool, by string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO role_apps (role, base_package, updated_by, updated_at, auto_install) VALUES ($1, '', $2, now(), $3)
		ON CONFLICT (role) DO UPDATE SET auto_install = EXCLUDED.auto_install, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		role, by, on)
	return err
}

// RefusedInstallsOf is the devices that refused one of these APKs in the last 30 days
// because it is signed with another key than the build they run (Android's
// INSTALL_FAILED_UPDATE_INCOMPATIBLE). Sending it again can never work, so the upgrade
// job leaves them alone until a different APK is the newest.
func (d *DB) RefusedInstallsOf(ctx context.Context, ids []uuid.UUID, urls []string) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(ids) == 0 || len(urls) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT ct.target_id
		FROM commands c
		JOIN command_targets ct ON ct.command_id = c.id
		JOIN command_results cr ON cr.command_id = c.id AND cr.device_id = ct.target_id
		WHERE c.type = 'install_apk'
		  AND ct.target_id = ANY($1)
		  AND c.apk_url = ANY($2)
		  AND c.created_at > now() - interval '30 days'
		  AND (cr.output ILIKE '%INSTALL_FAILED_UPDATE_INCOMPATIBLE%' OR cr.output ILIKE '%signed with a different key%'
		       OR cr.output ILIKE '%signatures do not match%')`, ids, urls)
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

// SetRoleAppAutoUpgrade switches upgrading to the library's newest on or off for a role.
func (d *DB) SetRoleAppAutoUpgrade(ctx context.Context, role string, on bool, by string) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO role_apps (role, base_package, updated_by, updated_at, auto_upgrade) VALUES ($1, '', $2, now(), $3)
		ON CONFLICT (role) DO UPDATE SET auto_upgrade = EXCLUDED.auto_upgrade, updated_by = EXCLUDED.updated_by, updated_at = now()`,
		role, by, on)
	return err
}

// InstallState is a device's latest install of an APK: where it got to and what the
// device said if it failed.
type InstallState struct {
	CommandID uuid.UUID
	Status    string // "" = not picked up yet, else the command_status status
	Progress  *int
	SentAt    time.Time
	UpdatedAt time.Time
	Output    string // the device's result text ("error: …" on a failure)
}

// LatestInstallsOf returns, per device, its latest install of one of these APKs sent
// within the window.
func (d *DB) LatestInstallsOf(ctx context.Context, ids []uuid.UUID, urls []string, within time.Duration) (map[uuid.UUID]InstallState, error) {
	out := map[uuid.UUID]InstallState{}
	if len(ids) == 0 || len(urls) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT DISTINCT ON (ct.target_id) ct.target_id, c.id, COALESCE(cs.status, ''), cs.progress,
		       c.created_at, COALESCE(cs.updated_at, c.created_at), COALESCE(cr.output, '')
		FROM commands c
		JOIN command_targets ct ON ct.command_id = c.id
		LEFT JOIN command_status cs ON cs.command_id = c.id AND cs.device_id = ct.target_id
		LEFT JOIN command_results cr ON cr.command_id = c.id AND cr.device_id = ct.target_id
		WHERE c.type = 'install_apk'
		  AND ct.target_id = ANY($1)
		  AND c.apk_url = ANY($2)
		  AND c.created_at > now() - make_interval(secs => $3)
		ORDER BY ct.target_id, c.created_at DESC`, ids, urls, within.Seconds())
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var st InstallState
		var pct *int16
		if err := rows.Scan(&id, &st.CommandID, &st.Status, &pct, &st.SentAt, &st.UpdatedAt, &st.Output); err != nil {
			return nil, err
		}
		if pct != nil {
			p := int(*pct)
			st.Progress = &p
		}
		out[id] = st
	}
	return out, rows.Err()
}
