package db

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	prod "mdm/internal/product"
)

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
		SELECT p.id, p.type, p.target_build_id, p.source_build_id, p.release_date, p.update_url, p.changelog, p.status, p.wipe, p.created_at,
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
		if err := rows.Scan(&p.ID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.Changelog, &p.Status, &p.Wipe, &p.CreatedAt, &p.DeploymentCount); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// GetOTAPayloadMeta returns the cached payload location for a package (what the
// legacy otautil client is told), ok=false when it was never computed.
func (d *DB) GetOTAPayloadMeta(ctx context.Context, id int) (offset, size int64, headers []string, ok bool, err error) {
	err = d.pool.QueryRow(ctx, `SELECT payload_offset, payload_size, payload_headers FROM ota_packages WHERE id = $1`, id).Scan(&offset, &size, &headers)
	if err != nil {
		return 0, 0, nil, false, err
	}
	return offset, size, headers, size > 0, nil
}

// SetOTAPayloadMeta caches the payload location computed from the package zip.
func (d *DB) SetOTAPayloadMeta(ctx context.Context, id int, offset, size int64, headers []string) error {
	if headers == nil {
		headers = []string{}
	}
	_, err := d.pool.Exec(ctx, `UPDATE ota_packages SET payload_offset = $2, payload_size = $3, payload_headers = $4 WHERE id = $1`, id, offset, size, headers)
	return err
}

func (d *DB) GetOTAPackage(ctx context.Context, id int) (*OTAPackage, error) {
	var p OTAPackage
	err := d.pool.QueryRow(ctx, `
		SELECT p.id, p.type, p.target_build_id, p.source_build_id, p.release_date, p.update_url, p.changelog, p.status, p.wipe, p.created_at,
		       COUNT(u.id) AS deployment_count
		FROM ota_packages p
		LEFT JOIN updates u ON u.ota_package_id = p.id
		WHERE p.id = $1
		GROUP BY p.id
	`, id).Scan(&p.ID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.Changelog, &p.Status, &p.Wipe, &p.CreatedAt, &p.DeploymentCount)
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
		SELECT id, version, product, name, changelog, status, is_dev, created_at, published_at
		FROM releases WHERE version = $1 AND product = $2
	`, version, p.Key).Scan(&r.ID, &r.Version, &r.Product, &r.Name, &r.Changelog, &r.Status, &r.IsDev, &r.CreatedAt, &r.PublishedAt)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (d *DB) GetRelease(ctx context.Context, id int) (*Release, error) {
	var r Release
	err := d.pool.QueryRow(ctx, `
		SELECT id, version, product, name, changelog, status, is_dev, created_at, published_at,
		       parent_release_id, is_branch,
		       merged_from_release_id, merged_into_release_id, merged_at, merged_by
		FROM releases WHERE id = $1
	`, id).Scan(&r.ID, &r.Version, &r.Product, &r.Name, &r.Changelog, &r.Status, &r.IsDev, &r.CreatedAt, &r.PublishedAt,
		&r.ParentReleaseID, &r.IsBranch,
		&r.MergedFromReleaseID, &r.MergedIntoReleaseID, &r.MergedAt, &r.MergedBy)
	if err != nil {
		return nil, err
	}
	return &r, nil
}

func (d *DB) ListReleases(ctx context.Context) ([]Release, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT r.id, r.version, r.product, r.name, r.changelog, r.status, r.hidden, r.is_dev, r.created_at, r.published_at,
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
		if err := rows.Scan(&r.ID, &r.Version, &r.Product, &r.Name, &r.Changelog, &r.Status, &r.Hidden, &r.IsDev, &r.CreatedAt, &r.PublishedAt, &r.ParentReleaseID, &r.IsBranch, &r.MergedFromReleaseID, &r.MergedIntoReleaseID, &r.MergedAt, &r.MergedBy, &r.PackageCount, &r.DeployCount); err != nil {
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
	IsDev       bool
}

// ListPublishedReleasesForRail returns published, non-hidden releases newest-first, each
// with the number of non-hidden devices currently on that version.
func (d *DB) ListPublishedReleasesForRail(ctx context.Context) ([]ReleaseRailItem, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT r.id, r.version, r.name, r.is_dev, COUNT(dev.build_id)::int AS device_count
		FROM releases r
		LEFT JOIN devices dev ON dev.build_id = r.version AND NOT dev.hidden
		    AND (CASE WHEN dev.product = '' THEN 't7' ELSE dev.product END) = r.product
		WHERE r.status = 'published' AND NOT r.hidden
		GROUP BY r.id, r.version, r.name, r.is_dev, r.published_at, r.created_at
		ORDER BY r.published_at DESC NULLS LAST, r.created_at DESC
	`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ReleaseRailItem
	for rows.Next() {
		var r ReleaseRailItem
		if err := rows.Scan(&r.ID, &r.Version, &r.Name, &r.IsDev, &r.DeviceCount); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// SetReleaseDev marks a release as dev-only (visible to devs and admins) or
// publishes it to every role.
func (d *DB) SetReleaseDev(ctx context.Context, id int, dev bool) error {
	_, err := d.pool.Exec(ctx, `UPDATE releases SET is_dev = $2 WHERE id = $1`, id, dev)
	return err
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

// GetFleetVersions returns every release version actually reported by non-hidden devices
// running OUR firmware, with the devices on each and a link to the managed release (if one
// exists). This is the "what's really running in the field" view for release tracking.
//
// DPC devices are excluded. They are stock Android managed by the agent, and their
// build_id is the vendor's ROM fingerprint — "AP3A.240905.015.A2.S146VLUDSLDZH3",
// "K20_V1.0.1_202303221019" — not a release of ours. Including them put those strings in
// the Releases page's untracked list, inviting someone to "track" a Samsung ROM as if it
// were something we ship and could push. Nothing here can build, sign or deploy one.
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
		  AND COALESCE(d.agent_kind, '') <> 'dpc'
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

// DevicesWithPackages maps each device to the given packages it has installed (from its
// reported app inventory). Only devices with at least one of them appear.
func (d *DB) DevicesWithPackages(ctx context.Context, packages []string) (map[uuid.UUID]map[string]bool, error) {
	out := map[uuid.UUID]map[string]bool{}
	rows, err := d.pool.Query(ctx, `
		SELECT device_id, package_name FROM device_packages WHERE package_name = ANY($1)`, packages)
	if err != nil {
		return out, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var p string
		if err := rows.Scan(&id, &p); err != nil {
			return out, err
		}
		if out[id] == nil {
			out[id] = map[string]bool{}
		}
		out[id][p] = true
	}
	return out, rows.Err()
}

// FilterFirmwareDeviceIDs keeps the ids of devices that are not DPC-managed. OTA is for
// our firmware devices only; a DPC agent on stock hardware has no OTA path, MDM or
// legacy, so it must never become an update target however it was selected.
func (d *DB) FilterFirmwareDeviceIDs(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT id FROM devices WHERE id = ANY($1) AND agent_kind <> $2`, ids, prod.KindDPC)
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

// RebootBlockedFor reports whether a device is mid-OTA, so an operator's reboot must be
// refused: rebooting during a download throws the bytes away, and rebooting mid-install
// can leave a half-written slot. Covers both fleets — agent OTA (update_devices) and
// legacy otautil (legacy_ota_deployment_devices).
//
// "awaiting_reboot" deliberately does NOT block: that state is waiting for exactly this
// reboot. Nor does this gate the MDM's own post-OTA reboot, which does not call it.
func (d *DB) RebootBlockedFor(ctx context.Context, deviceID uuid.UUID) (bool, string, error) {
	var phase string
	// Agent fleet: same predicate ActiveOTADevices/SerialsUpdating use for "in flight".
	err := d.pool.QueryRow(ctx, `
		SELECT ud.status
		FROM update_devices ud
		JOIN updates u ON u.id = ud.update_id AND u.status = 'active'
		WHERE ud.device_id = $1 AND ud.status IN ('pending', 'downloading')
		LIMIT 1`, deviceID).Scan(&phase)
	if err == nil {
		return true, "a system update is " + phase, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, "", err
	}
	// Legacy fleet: keyed by serial, and only for a device that also has a device row.
	err = d.pool.QueryRow(ctx, `
		SELECT t.status
		FROM legacy_ota_deployment_devices t
		JOIN legacy_ota_deployments p ON p.id = t.deployment_id AND p.status = 'active'
		JOIN devices dv ON dv.serial_number = t.serial
		WHERE dv.id = $1
		  AND t.status IN ('offered', 'downloading', 'installing', 'verifying', 'finalizing')
		LIMIT 1`, deviceID).Scan(&phase)
	if err == nil {
		return true, "a legacy OTA is " + phase, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return false, "", err
	}
	return false, "", nil
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
// device — unless that image wipes and allowWipe is off) OR the device's current build
// matches an active incremental's source_build_id.
// Without this, selecting a device on an unrelated build for an incremental-only release
// creates a pending row the resolver can never satisfy — it sits "pending" forever.
func (d *DB) RemoveInapplicableTargets(ctx context.Context, releaseID int, ids []uuid.UUID, allowWipe bool) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return ids, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT dv.id
		FROM devices dv
		WHERE dv.id = ANY($2)
		  AND (
		      EXISTS (SELECT 1 FROM ota_packages p
		              WHERE p.release_id = $1 AND p.status = 'active' AND p.type = 'full'
		                AND (NOT p.wipe OR $3))
		   OR EXISTS (SELECT 1 FROM ota_packages p
		              WHERE p.release_id = $1 AND p.status = 'active' AND p.type = 'incremental'
		                AND p.source_build_id = dv.build_id)
		  )`, releaseID, ids, allowWipe)
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

// CountDevicesByVersion returns how many non-hidden devices of a product (empty/legacy
// device product counts as t7) currently report a version. The same version string
// for another product is a different release and its devices don't count here.
func (d *DB) CountDevicesByVersion(ctx context.Context, version, product string) (int, error) {
	p, _ := prod.Resolve(product)
	var n int
	err := d.pool.QueryRow(ctx,
		`SELECT COUNT(*) FROM devices WHERE NOT hidden AND build_id = $1
		   AND (CASE WHEN product = '' THEN 't7' ELSE product END) = $2`, version, p.Key).Scan(&n)
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

// CreateBranchRelease forks an off-mainline branch build from parentID: a new release with
// is_branch + parent_release_id set. version must be unique. Returns the new release id.
func (d *DB) CreateBranchRelease(ctx context.Context, parentID int, version, name, changelog string) (int, error) {
	var id int
	err := d.pool.QueryRow(ctx, `
		INSERT INTO releases (version, name, changelog, is_branch, parent_release_id, product, is_dev)
		VALUES ($1, $2, $3, true, $4, (SELECT product FROM releases WHERE id = $4),
		        -- a fork of a dev release is dev too; forking a visible release keeps it visible
		        COALESCE((SELECT is_dev FROM releases WHERE id = $4), false)) RETURNING id`,
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
//   - Problems (always): its still-open problems carry over as open problems on main.
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
	// Only an unmerged branch can merge. No row → ErrNoRows → the merge aborts.
	if err := tx.QueryRow(ctx,
		`SELECT parent_release_id FROM releases WHERE id = $1 AND is_branch AND merged_into_release_id IS NULL`,
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

	// Carry the branch's still-open problems onto main as manual problems on the new
	// release. They land as 'open' regardless of their branch status: the branch's
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

// ListPackagesByRelease returns the packages (full + incrementals) of a release.
func (d *DB) ListPackagesByRelease(ctx context.Context, releaseID int) ([]OTAPackage, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, release_id, type, target_build_id, source_build_id, release_date, update_url, changelog, status, wipe, created_at,
		       COALESCE(payload_size, 0)
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
		if err := rows.Scan(&p.ID, &p.ReleaseID, &p.Type, &p.TargetBuildID, &p.SourceBuildID, &p.ReleaseDate, &p.UpdateURL, &p.Changelog, &p.Status, &p.Wipe, &p.CreatedAt, &p.PayloadSize); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// SetPackageSize records a package's size once something has measured it. Packages made
// before the size was recorded carry 0, and the host is the only place left to ask.
func (d *DB) SetPackageSize(ctx context.Context, id int, size int64) error {
	_, err := d.pool.Exec(ctx, `UPDATE ota_packages SET payload_size = $2 WHERE id = $1 AND COALESCE(payload_size,0) = 0`, id, size)
	return err
}


// ── MDM OTA support per build ────────────────────────────────────────────────
// See the ota_support table: which firmware builds can actually apply an MDM OTA.
// Builds that cannot are updated over the legacy otautil path instead.

// OTASupport is one explicit answer for a (build, product).
type OTASupport struct {
	BuildID   string
	Product   string
	Supported bool
	Source    string // manual | observed | reported
	Note      string
	UpdatedAt time.Time
}

// ListOTASupport returns every explicit build answer, newest change first.
func (d *DB) ListOTASupport(ctx context.Context) ([]OTASupport, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT build_id, product, supported, source, note, updated_at
		FROM ota_support ORDER BY updated_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OTASupport
	for rows.Next() {
		var o OTASupport
		if err := rows.Scan(&o.BuildID, &o.Product, &o.Supported, &o.Source, &o.Note, &o.UpdatedAt); err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

// SetOTASupport records an explicit answer for a build, replacing any existing one.
func (d *DB) SetOTASupport(ctx context.Context, buildID, product string, supported bool, source, note string) error {
	if strings.TrimSpace(buildID) == "" {
		return errors.New("build id is required")
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO ota_support (build_id, product, supported, source, note, updated_at)
		VALUES ($1, $2, $3, $4, $5, NOW())
		ON CONFLICT (build_id, product) DO UPDATE
		   SET supported = EXCLUDED.supported, source = EXCLUDED.source,
		       note = EXCLUDED.note, updated_at = NOW()`,
		strings.TrimSpace(buildID), product, supported, source, note)
	return err
}

// DeleteOTASupport drops an explicit answer, so the build falls back to the cutoff.
func (d *DB) DeleteOTASupport(ctx context.Context, buildID, product string) error {
	_, err := d.pool.Exec(ctx, `DELETE FROM ota_support WHERE build_id = $1 AND product = $2`, buildID, product)
	return err
}

// MarkOTAObserved records proof that a build can apply an MDM OTA: a device that was
// on it completed one. Never downgrades a manual answer — an admin's "no" stands.
func (d *DB) MarkOTAObserved(ctx context.Context, buildID, product string) error {
	if strings.TrimSpace(buildID) == "" {
		return nil
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO ota_support (build_id, product, supported, source, note, updated_at)
		VALUES ($1, $2, true, 'observed', 'a device on this build completed an MDM OTA', NOW())
		ON CONFLICT (build_id, product) DO UPDATE
		   SET supported = true, source = 'observed', note = EXCLUDED.note, updated_at = NOW()
		 WHERE ota_support.source <> 'manual'`, strings.TrimSpace(buildID), product)
	return err
}

// SourceBuildForTarget returns the build a device was on when a rollout targeted it
// ("" for rows created before the column existed).
func (d *DB) SourceBuildForTarget(ctx context.Context, updateID int, deviceID uuid.UUID) (string, error) {
	var b string
	err := d.pool.QueryRow(ctx, `SELECT source_build_id FROM update_devices WHERE update_id = $1 AND device_id = $2`, updateID, deviceID).Scan(&b)
	return b, err
}

// SerialsForDeviceIDs maps device ids to their serial numbers, in no particular
// order. Used where a feature is keyed by serial (the legacy OTA tables) but the
// caller works in ids.
func (d *DB) SerialsForDeviceIDs(ctx context.Context, ids []uuid.UUID) ([]string, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, `SELECT serial_number FROM devices WHERE id = ANY($1)`, ids)
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
