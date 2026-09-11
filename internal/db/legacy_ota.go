package db

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/jackc/pgx/v5"
)

// Legacy OTA: devices on pre-agent builds that speak the otautil protocol. They
// are kept apart from the fleet (own tables, own page under Updates) so the
// Devices list only shows managed devices. The model mirrors the old ota-server:
// groups with an allowlist of serials and one target release each.

const legacyOTASchema = `
CREATE TABLE IF NOT EXISTS legacy_ota_devices (
	serial        TEXT        PRIMARY KEY,
	build_id      TEXT        NOT NULL DEFAULT '',
	first_seen    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	last_seen     TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	last_check    TIMESTAMPTZ,
	last_ip       TEXT        NOT NULL DEFAULT '',
	polls         INTEGER     NOT NULL DEFAULT 0,
	status        TEXT        NOT NULL DEFAULT 'idle',
	status_at     TIMESTAMPTZ,
	offered_build TEXT        NOT NULL DEFAULT '',
	offered_at    TIMESTAMPTZ,
	error         TEXT        NOT NULL DEFAULT ''
);
CREATE TABLE IF NOT EXISTS legacy_ota_groups (
	id         SERIAL      PRIMARY KEY,
	name       TEXT        NOT NULL UNIQUE,
	release_id INTEGER     REFERENCES releases(id) ON DELETE SET NULL,
	enabled    BOOLEAN     NOT NULL DEFAULT false,
	priority   INTEGER     NOT NULL DEFAULT 0,
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS legacy_ota_group_devices (
	group_id INTEGER NOT NULL REFERENCES legacy_ota_groups(id) ON DELETE CASCADE,
	serial   TEXT    NOT NULL,
	PRIMARY KEY (group_id, serial)
);
-- Legacy deployments: the same shape as a fleet rollout (a release pushed to a
-- set of devices, tracked per device) but keyed by serial, because otautil
-- devices deliberately never enter the devices table. Replaces the older
-- group + allowlist model, whose tables are left in place but unused.
CREATE TABLE IF NOT EXISTS legacy_ota_deployments (
	id         SERIAL      PRIMARY KEY,
	release_id INTEGER     NOT NULL REFERENCES releases(id) ON DELETE CASCADE,
	status     TEXT        NOT NULL DEFAULT 'active',
	created_by TEXT        NOT NULL DEFAULT '',
	created_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
CREATE TABLE IF NOT EXISTS legacy_ota_deployment_devices (
	deployment_id INTEGER     NOT NULL REFERENCES legacy_ota_deployments(id) ON DELETE CASCADE,
	serial        TEXT        NOT NULL,
	status        TEXT        NOT NULL DEFAULT 'pending',
	percent       INTEGER     NOT NULL DEFAULT 0,
	error         TEXT        NOT NULL DEFAULT '',
	offered_build TEXT        NOT NULL DEFAULT '',
	updated_at    TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY (deployment_id, serial)
);
CREATE INDEX IF NOT EXISTS idx_legacy_dep_devices_serial ON legacy_ota_deployment_devices(serial);
`

// LegacyOTADevice is one otautil client as last seen.
type LegacyOTADevice struct {
	Serial       string
	BuildID      string
	FirstSeen    time.Time
	LastSeen     time.Time
	LastCheck    *time.Time
	LastIP       string
	Polls        int
	Status       string // idle | offered | downloading | installing | updated | failed
	StatusAt     *time.Time
	OfferedBuild string
	OfferedAt    *time.Time
	Error        string
	// joined
	GroupID   int
	GroupName string
	// live, filled by the page from the in-memory progress store
	Percent int
}

// LegacyOTAGroup is an allowlist of serials with one target release.
type LegacyOTAGroup struct {
	ID             int
	Name           string
	ReleaseID      *int
	ReleaseVersion string
	ReleaseProduct string
	Enabled        bool
	Priority       int
	CreatedAt      time.Time
	Serials        []string
	SeenCount      int // serials that have polled at least once
	OnTargetCount  int // serials already on the release's build
}

// TouchLegacyOTADevice records a poll or check-update. A device whose build now
// equals the build it was offered is marked updated; a device that changed build
// otherwise goes back to idle.
func (d *DB) TouchLegacyOTADevice(ctx context.Context, serial, buildID, ip string, isCheck bool) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO legacy_ota_devices (serial, build_id, last_ip, polls, last_check)
		VALUES ($1, $2, $3, 1, CASE WHEN $4 THEN NOW() ELSE NULL END)
		ON CONFLICT (serial) DO UPDATE SET
			last_seen  = NOW(),
			last_ip    = EXCLUDED.last_ip,
			polls      = legacy_ota_devices.polls + 1,
			last_check = CASE WHEN $4 THEN NOW() ELSE legacy_ota_devices.last_check END,
			status = CASE
				WHEN legacy_ota_devices.offered_build <> '' AND $2 = legacy_ota_devices.offered_build THEN 'updated'
				WHEN $2 <> legacy_ota_devices.build_id AND legacy_ota_devices.status IN ('offered','downloading','installing') THEN 'idle'
				ELSE legacy_ota_devices.status END,
			status_at = CASE
				WHEN legacy_ota_devices.offered_build <> '' AND $2 = legacy_ota_devices.offered_build AND legacy_ota_devices.status <> 'updated' THEN NOW()
				ELSE legacy_ota_devices.status_at END,
			build_id   = $2
	`, serial, buildID, ip, isCheck)
	return err
}

// SetLegacyOTADeviceStatus moves a device through offered → downloading →
// installing → updated | failed. offeredBuild is set only when non-empty.
func (d *DB) SetLegacyOTADeviceStatus(ctx context.Context, serial, status, offeredBuild, errText string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE legacy_ota_devices SET status = $2, status_at = NOW(), error = $4,
			offered_build = CASE WHEN $3 <> '' THEN $3 ELSE offered_build END,
			offered_at    = CASE WHEN $3 <> '' THEN NOW() ELSE offered_at END
		WHERE serial = $1`, serial, status, offeredBuild, errText)
	return err
}

func (d *DB) DeleteLegacyOTADevice(ctx context.Context, serial string) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM legacy_ota_devices WHERE serial = $1`, serial)
	return err
}

// ListLegacyOTADevices returns every otautil client seen, newest activity first,
// with the group it belongs to (first match by priority, then name).
func (d *DB) ListLegacyOTADevices(ctx context.Context) ([]LegacyOTADevice, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT v.serial, v.build_id, v.first_seen, v.last_seen, v.last_check, v.last_ip, v.polls, v.status, v.status_at,
		       v.offered_build, v.offered_at, v.error, COALESCE(g.id, 0), COALESCE(g.name, '')
		FROM legacy_ota_devices v
		LEFT JOIN LATERAL (
			SELECT g.id, g.name FROM legacy_ota_group_devices gd JOIN legacy_ota_groups g ON g.id = gd.group_id
			WHERE gd.serial = v.serial ORDER BY g.priority DESC, g.name LIMIT 1
		) g ON true
		ORDER BY v.last_seen DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LegacyOTADevice
	for rows.Next() {
		var v LegacyOTADevice
		if err := rows.Scan(&v.Serial, &v.BuildID, &v.FirstSeen, &v.LastSeen, &v.LastCheck, &v.LastIP, &v.Polls, &v.Status, &v.StatusAt,
			&v.OfferedBuild, &v.OfferedAt, &v.Error, &v.GroupID, &v.GroupName); err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}

// LegacyOTAGroupForSerial returns the enabled-or-not group whose allowlist holds
// the serial (highest priority wins), or nil.
func (d *DB) LegacyOTAGroupForSerial(ctx context.Context, serial string) (*LegacyOTAGroup, error) {
	var g LegacyOTAGroup
	err := d.pool.QueryRow(ctx, `
		SELECT g.id, g.name, g.release_id, COALESCE(r.version, ''), COALESCE(r.product, ''), g.enabled, g.priority, g.created_at
		FROM legacy_ota_group_devices gd
		JOIN legacy_ota_groups g ON g.id = gd.group_id
		LEFT JOIN releases r ON r.id = g.release_id
		WHERE gd.serial = $1
		ORDER BY g.priority DESC, g.name LIMIT 1`, serial).
		Scan(&g.ID, &g.Name, &g.ReleaseID, &g.ReleaseVersion, &g.ReleaseProduct, &g.Enabled, &g.Priority, &g.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &g, nil
}

// ListLegacyOTAGroups returns groups with their serials and counts.
func (d *DB) ListLegacyOTAGroups(ctx context.Context) ([]LegacyOTAGroup, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT g.id, g.name, g.release_id, COALESCE(r.version, ''), COALESCE(r.product, ''), g.enabled, g.priority, g.created_at,
		       COALESCE((SELECT array_agg(gd.serial ORDER BY gd.serial) FROM legacy_ota_group_devices gd WHERE gd.group_id = g.id), '{}'),
		       (SELECT COUNT(*) FROM legacy_ota_group_devices gd JOIN legacy_ota_devices v ON v.serial = gd.serial WHERE gd.group_id = g.id),
		       (SELECT COUNT(*) FROM legacy_ota_group_devices gd JOIN legacy_ota_devices v ON v.serial = gd.serial WHERE gd.group_id = g.id AND r.version IS NOT NULL AND v.build_id = r.version)
		FROM legacy_ota_groups g
		LEFT JOIN releases r ON r.id = g.release_id
		ORDER BY g.priority DESC, g.name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LegacyOTAGroup
	for rows.Next() {
		var g LegacyOTAGroup
		if err := rows.Scan(&g.ID, &g.Name, &g.ReleaseID, &g.ReleaseVersion, &g.ReleaseProduct, &g.Enabled, &g.Priority, &g.CreatedAt, &g.Serials, &g.SeenCount, &g.OnTargetCount); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

func (d *DB) CreateLegacyOTAGroup(ctx context.Context, name string, releaseID *int, enabled bool) (int, error) {
	var id int
	err := d.pool.QueryRow(ctx, `INSERT INTO legacy_ota_groups (name, release_id, enabled) VALUES ($1, $2, $3) RETURNING id`, name, releaseID, enabled).Scan(&id)
	return id, err
}

func (d *DB) UpdateLegacyOTAGroup(ctx context.Context, id int, name string, releaseID *int, enabled bool, priority int) error {
	_, err := d.pool.Exec(ctx, `UPDATE legacy_ota_groups SET name = $2, release_id = $3, enabled = $4, priority = $5 WHERE id = $1`, id, name, releaseID, enabled, priority)
	return err
}

func (d *DB) DeleteLegacyOTAGroup(ctx context.Context, id int) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM legacy_ota_groups WHERE id = $1`, id)
	return err
}

// AddLegacyOTAGroupSerials adds serials to a group's allowlist (duplicates ignored).
func (d *DB) AddLegacyOTAGroupSerials(ctx context.Context, id int, serials []string) (int, error) {
	n := 0
	for _, s := range serials {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		tag, err := d.pool.Exec(ctx, `INSERT INTO legacy_ota_group_devices (group_id, serial) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, s)
		if err != nil {
			return n, err
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}

func (d *DB) RemoveLegacyOTAGroupSerial(ctx context.Context, id int, serial string) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM legacy_ota_group_devices WHERE group_id = $1 AND serial = $2`, id, serial)
	return err
}

// PickLegacyOTAPackage chooses the package of a release for a device on buildID:
// the active incremental whose source is that build, else the active full image.
func (d *DB) PickLegacyOTAPackage(ctx context.Context, releaseID int, buildID string) (*OTAPackage, error) {
	var p OTAPackage
	err := d.pool.QueryRow(ctx, `
		SELECT id, release_id, type, target_build_id, source_build_id, release_date, update_url, changelog, status, created_at
		FROM ota_packages
		WHERE release_id = $1 AND status = 'active'
		  AND (type = 'full' OR (type = 'incremental' AND source_build_id = $2))
		ORDER BY (type = 'incremental') DESC, created_at DESC
		LIMIT 1`, releaseID, buildID).
		Scan(&p.ID, &p.ReleaseID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.Changelog, &p.Status, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &p, nil
}

// ListReleasesWithPackages lists published releases that carry an active FULL
// package, for the legacy group's target picker. A legacy device can be on any
// build, so only a full image is a safe target; incrementals still get picked
// per device at check-update time when their source build matches.
func (d *DB) ListReleasesWithPackages(ctx context.Context) ([]Release, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT r.id, r.version, r.product, r.is_dev, r.created_at
		FROM releases r
		WHERE r.status = 'published' AND EXISTS (SELECT 1 FROM ota_packages p WHERE p.release_id = r.id AND p.status = 'active' AND p.type = 'full')
		ORDER BY r.product, r.created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Release
	for rows.Next() {
		var r Release
		if err := rows.Scan(&r.ID, &r.Version, &r.Product, &r.IsDev, &r.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── Legacy deployments ────────────────────────────────────────────────────────

// LegacyDeployment is one release pushed to a set of otautil devices.
type LegacyDeployment struct {
	ID             int
	ReleaseID      int
	ReleaseVersion string
	ReleaseProduct string
	Status         string
	CreatedBy      string
	CreatedAt      time.Time
	Devices        []LegacyDeploymentDevice
	Total          int
	Installed      int
	Failed         int
}

// LegacyDeploymentDevice is one device's progress inside a legacy deployment.
type LegacyDeploymentDevice struct {
	Serial       string
	Status       string
	Percent      int
	Error        string
	OfferedBuild string
	UpdatedAt    time.Time
	BuildID      string // the device's current build, joined from legacy_ota_devices
	LastSeen     *time.Time
	InFleet      bool // the same serial also runs the MDM client
}

// CreateLegacyDeployment records a push and its target serials.
func (d *DB) CreateLegacyDeployment(ctx context.Context, releaseID int, serials []string, createdBy string) (int, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)
	var id int
	if err := tx.QueryRow(ctx, `INSERT INTO legacy_ota_deployments (release_id, created_by) VALUES ($1, $2) RETURNING id`, releaseID, createdBy).Scan(&id); err != nil {
		return 0, err
	}
	for _, s := range serials {
		s = strings.TrimSpace(s)
		if s == "" {
			continue
		}
		if _, err := tx.Exec(ctx, `INSERT INTO legacy_ota_deployment_devices (deployment_id, serial) VALUES ($1, $2) ON CONFLICT DO NOTHING`, id, s); err != nil {
			return 0, err
		}
	}
	return id, tx.Commit(ctx)
}

// ResolveLegacyDeployment returns the newest active deployment row still owed to
// this serial, or nil. Terminal rows (installed, failed, canceled) are skipped,
// so a failure waits for an operator's retry exactly like a fleet rollout.
func (d *DB) ResolveLegacyDeployment(ctx context.Context, serial string) (*LegacyDeployment, *LegacyDeploymentDevice, error) {
	var dep LegacyDeployment
	var dev LegacyDeploymentDevice
	err := d.pool.QueryRow(ctx, `
		SELECT p.id, p.release_id, COALESCE(r.version, ''), COALESCE(r.product, ''), p.status, p.created_by, p.created_at,
		       t.serial, t.status, t.percent, t.error, t.offered_build, t.updated_at
		FROM legacy_ota_deployment_devices t
		JOIN legacy_ota_deployments p ON p.id = t.deployment_id
		JOIN releases r ON r.id = p.release_id
		WHERE t.serial = $1 AND p.status = 'active' AND t.status NOT IN ('installed', 'failed', 'canceled')
		ORDER BY p.created_at DESC
		LIMIT 1`, serial).
		Scan(&dep.ID, &dep.ReleaseID, &dep.ReleaseVersion, &dep.ReleaseProduct, &dep.Status, &dep.CreatedBy, &dep.CreatedAt,
			&dev.Serial, &dev.Status, &dev.Percent, &dev.Error, &dev.OfferedBuild, &dev.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, nil
	}
	if err != nil {
		return nil, nil, err
	}
	return &dep, &dev, nil
}

// SetLegacyDeploymentDevice moves one target row along. Empty strings leave the
// matching field untouched; percent < 0 leaves the percent alone.
func (d *DB) SetLegacyDeploymentDevice(ctx context.Context, depID int, serial, status string, percent int, errText, offeredBuild string) error {
	_, err := d.pool.Exec(ctx, `
		UPDATE legacy_ota_deployment_devices
		SET status = COALESCE(NULLIF($3, ''), status),
		    percent = CASE WHEN $4 >= 0 THEN $4 ELSE percent END,
		    error = CASE WHEN $3 = 'failed' THEN $5 ELSE '' END,
		    offered_build = COALESCE(NULLIF($6, ''), offered_build),
		    updated_at = NOW()
		WHERE deployment_id = $1 AND serial = $2`, depID, serial, status, percent, errText, offeredBuild)
	return err
}

// CompleteLegacyDeploymentsAtBuild marks every open row for this serial installed
// once it reports the build that row was offered, and closes deployments whose
// devices have all settled.
func (d *DB) CompleteLegacyDeploymentsAtBuild(ctx context.Context, serial, buildID string) error {
	if buildID == "" {
		return nil
	}
	if _, err := d.pool.Exec(ctx, `
		UPDATE legacy_ota_deployment_devices t
		SET status = 'installed', percent = 100, error = '', updated_at = NOW()
		FROM legacy_ota_deployments p, releases r
		WHERE p.id = t.deployment_id AND r.id = p.release_id
		  AND t.serial = $1 AND r.version = $2
		  AND t.status NOT IN ('installed', 'canceled')`, serial, buildID); err != nil {
		return err
	}
	_, err := d.pool.Exec(ctx, `
		UPDATE legacy_ota_deployments p SET status = 'complete'
		WHERE p.status = 'active'
		  AND NOT EXISTS (
		      SELECT 1 FROM legacy_ota_deployment_devices t
		      WHERE t.deployment_id = p.id AND t.status NOT IN ('installed', 'failed', 'canceled')
		  )`)
	return err
}

// ListLegacyDeployments returns every legacy rollout, newest first, with its rows.
// GetLegacyDeployment returns one rollout with its device rows.
func (d *DB) GetLegacyDeployment(ctx context.Context, id int) (*LegacyDeployment, error) {
	var p LegacyDeployment
	err := d.pool.QueryRow(ctx, `
		SELECT p.id, p.release_id, COALESCE(r.version, ''), COALESCE(r.product, ''), p.status, p.created_by, p.created_at
		FROM legacy_ota_deployments p
		LEFT JOIN releases r ON r.id = p.release_id
		WHERE p.id = $1`, id).
		Scan(&p.ID, &p.ReleaseID, &p.ReleaseVersion, &p.ReleaseProduct, &p.Status, &p.CreatedBy, &p.CreatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	rows, err := d.pool.Query(ctx, `
		SELECT t.serial, t.status, t.percent, t.error, t.offered_build, t.updated_at,
		       COALESCE(v.build_id, ''), v.last_seen, EXISTS (SELECT 1 FROM devices dv WHERE dv.serial_number = t.serial)
		FROM legacy_ota_deployment_devices t
		LEFT JOIN legacy_ota_devices v ON v.serial = t.serial
		WHERE t.deployment_id = $1
		ORDER BY t.serial`, id)
	if err != nil {
		return &p, err
	}
	defer rows.Close()
	for rows.Next() {
		var dv LegacyDeploymentDevice
		if err := rows.Scan(&dv.Serial, &dv.Status, &dv.Percent, &dv.Error, &dv.OfferedBuild, &dv.UpdatedAt, &dv.BuildID, &dv.LastSeen, &dv.InFleet); err != nil {
			return &p, err
		}
		p.Devices = append(p.Devices, dv)
		p.Total++
		switch dv.Status {
		case "installed":
			p.Installed++
		case "failed":
			p.Failed++
		}
	}
	return &p, rows.Err()
}

func (d *DB) ListLegacyDeployments(ctx context.Context) ([]LegacyDeployment, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT p.id, p.release_id, COALESCE(r.version, ''), COALESCE(r.product, ''), p.status, p.created_by, p.created_at
		FROM legacy_ota_deployments p
		LEFT JOIN releases r ON r.id = p.release_id
		ORDER BY p.created_at DESC
		LIMIT 50`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LegacyDeployment
	byID := map[int]int{}
	for rows.Next() {
		var p LegacyDeployment
		if err := rows.Scan(&p.ID, &p.ReleaseID, &p.ReleaseVersion, &p.ReleaseProduct, &p.Status, &p.CreatedBy, &p.CreatedAt); err != nil {
			return nil, err
		}
		byID[p.ID] = len(out)
		out = append(out, p)
	}
	if err := rows.Err(); err != nil || len(out) == 0 {
		return out, err
	}
	drows, err := d.pool.Query(ctx, `
		SELECT t.deployment_id, t.serial, t.status, t.percent, t.error, t.offered_build, t.updated_at,
		       COALESCE(v.build_id, ''), v.last_seen, EXISTS (SELECT 1 FROM devices dv WHERE dv.serial_number = t.serial)
		FROM legacy_ota_deployment_devices t
		LEFT JOIN legacy_ota_devices v ON v.serial = t.serial
		ORDER BY t.serial`)
	if err != nil {
		return out, err
	}
	defer drows.Close()
	for drows.Next() {
		var depID int
		var dv LegacyDeploymentDevice
		if err := drows.Scan(&depID, &dv.Serial, &dv.Status, &dv.Percent, &dv.Error, &dv.OfferedBuild, &dv.UpdatedAt, &dv.BuildID, &dv.LastSeen, &dv.InFleet); err != nil {
			return out, err
		}
		i, ok := byID[depID]
		if !ok {
			continue
		}
		out[i].Devices = append(out[i].Devices, dv)
		out[i].Total++
		switch dv.Status {
		case "installed":
			out[i].Installed++
		case "failed":
			out[i].Failed++
		}
	}
	return out, drows.Err()
}

// CancelLegacyDeployment stops a rollout; devices mid-flight keep whatever they
// have already downloaded but are offered nothing further.
func (d *DB) CancelLegacyDeployment(ctx context.Context, id int) error {
	if _, err := d.pool.Exec(ctx, `UPDATE legacy_ota_deployments SET status = 'canceled' WHERE id = $1`, id); err != nil {
		return err
	}
	_, err := d.pool.Exec(ctx, `UPDATE legacy_ota_deployment_devices SET status = 'canceled', updated_at = NOW() WHERE deployment_id = $1 AND status NOT IN ('installed', 'failed')`, id)
	return err
}

// RetryLegacyDeploymentDevice re-arms one failed row.
func (d *DB) RetryLegacyDeploymentDevice(ctx context.Context, id int, serial string) error {
	if _, err := d.pool.Exec(ctx, `UPDATE legacy_ota_deployments SET status = 'active' WHERE id = $1 AND status <> 'active'`, id); err != nil {
		return err
	}
	_, err := d.pool.Exec(ctx, `UPDATE legacy_ota_deployment_devices SET status = 'pending', percent = 0, error = '', updated_at = NOW() WHERE deployment_id = $1 AND serial = $2`, id, serial)
	return err
}

// CountLegacyOTADevices is the number of otautil devices that have ever polled —
// the Updates page shows it so the legacy fleet is visible without a detour.
func (d *DB) CountLegacyOTADevices(ctx context.Context) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `SELECT count(*) FROM legacy_ota_devices`).Scan(&n)
	return n, err
}
