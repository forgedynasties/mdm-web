package db

import (
	"context"
	"fmt"
	"hash/fnv"
	"sort"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ── Device Packages ───────────────────────────────────────────────────────────

type DevicePackage struct {
	PackageName string `json:"package_name"`
	AppName     string `json:"app_name"`
	VersionName string `json:"version_name"`
	// IsSystem is the client-reported system flag: nil when the device's client is
	// too old to report it (then it counts as system). Used on the write path.
	IsSystem *bool `json:"is_system,omitempty"`
	// EffectiveSystem is the resolved classification (the client's report, or a vendor
	// package by name: vendorPackageSQL). Populated on the read path; gates uninstall.
	EffectiveSystem bool `json:"effective_system"`
	// Launchable is the client-reported "has a launcher (drawer) entry" flag: nil when
	// the client is too old to report it. The dashboard's app lists default to
	// launchable apps only (with the raw dump behind ?all=1).
	Launchable *bool     `json:"launchable,omitempty"`
	UpdatedAt  time.Time `json:"updated_at"`
	// Icon is a base64-encoded PNG launcher icon. On the write path it's the value the
	// device reported (may be empty); on the read path it's resolved from the shared
	// app_icons index keyed by package_name, so any device — even an old client that
	// never reports icons — shows an icon once ANY device has reported one for it.
	Icon string `json:"icon,omitempty"`
}

// AdminPackage is a fleet-wide package row (the lock allowlist picker reads it).
type AdminPackage struct {
	PackageName     string `json:"package_name"`
	AppName         string `json:"app_name"`
	DeviceCount     int    `json:"device_count"`
	ReportedSystem  int    `json:"reported_system"`  // devices whose client reported is_system=true
	ReportedUser    int    `json:"reported_user"`    // devices whose client reported is_system=false
	ReportedUnknown int    `json:"reported_unknown"` // devices whose client didn't report
	Vendor          bool   `json:"vendor"`           // firmware-bundled by name (vendorPackageSQL)
	EffectiveSystem bool   `json:"effective_system"`
	Icon            string `json:"icon"` // base64 PNG from app_icons, "" if none
	// App-drawer visibility as reported by clients that send the launchable flag
	// (the DPC agent does): rows with launchable=true vs rows that carry the flag at all.
	ReportedLaunchable  int `json:"reported_launchable"`
	ReportedLaunchKnown int `json:"reported_launch_known"`
}

// InAppDrawer reports whether the package shows in a launcher: launchable on at
// least one device, or, for clients that never report the flag, a user app.
func (p AdminPackage) InAppDrawer() bool {
	if p.ReportedLaunchKnown > 0 {
		return p.ReportedLaunchable > 0
	}
	return !p.EffectiveSystem
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
// NeedsAppInventory reports whether the stored app list for a device is unknown or
// stale, so the check-in response should ask the client for a full one. True when the
// inventory hash was cleared (a build change) or nothing was ever stored.
func (d *DB) NeedsAppInventory(ctx context.Context, deviceID uuid.UUID) bool {
	var stale bool
	if err := d.pool.QueryRow(ctx, `
		SELECT COALESCE(packages_hash, '') = ''
		       OR NOT EXISTS (SELECT 1 FROM device_packages dp WHERE dp.device_id = devices.id)
		FROM devices WHERE id = $1`, deviceID).Scan(&stale); err != nil {
		return false
	}
	return stale
}

func devicePackagesHash(packages []DevicePackage) string {
	sorted := make([]DevicePackage, len(packages))
	copy(sorted, packages)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].PackageName < sorted[j].PackageName })
	h := fnv.New64a()
	boolStr := func(b *bool) string {
		if b == nil {
			return "n"
		}
		if *b {
			return "1"
		}
		return "0"
	}
	for _, p := range sorted {
		// Launchable is part of the fingerprint so a client that STARTS reporting the
		// flag (agent upgrade / server upgrade adding the column) actually gets its
		// rows rewritten — otherwise the hash-gate below would skip the upsert and the
		// flag would stay NULL until the app set happened to change.
		fmt.Fprintf(h, "%s\x00%s\x00%s\x00%s\x00%s\n", p.PackageName, p.AppName, p.VersionName, boolStr(p.IsSystem), boolStr(p.Launchable))
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
			// Same apps and versions, but the device may be sending icons it didn't
			// before (the hash ignores icons): keep those, or they are lost for good.
			return d.upsertAppIcons(ctx, d.pool, packages)
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
		launchables := make([]*bool, len(packages))
		for i, p := range packages {
			names[i] = p.PackageName
			appNames[i] = p.AppName
			versions[i] = p.VersionName
			systems[i] = p.IsSystem       // *bool → NULL when the client didn't report
			launchables[i] = p.Launchable // *bool → NULL when the client didn't report
		}
		if _, err := tx.Exec(ctx, `
			INSERT INTO device_packages (device_id, package_name, app_name, version_name, is_system, launchable)
			SELECT $1, unnest($2::text[]), unnest($3::text[]), unnest($4::text[]), unnest($5::bool[]), unnest($6::bool[])
			ON CONFLICT (device_id, package_name) DO UPDATE SET app_name = EXCLUDED.app_name, version_name = EXCLUDED.version_name, is_system = EXCLUDED.is_system, launchable = EXCLUDED.launchable, updated_at = NOW()
		`, deviceID, names, appNames, versions, systems, launchables); err != nil {
			return err
		}

		if err := d.upsertAppIcons(ctx, tx, packages); err != nil {
			return err
		}
	}

	if _, err := tx.Exec(ctx, `UPDATE devices SET packages_hash = $2 WHERE id = $1`, deviceID, newHash); err != nil {
		return err
	}

	return tx.Commit(ctx)
}

// upsertAppIcons populates the shared package→icon index from whatever icons a device
// sent. Non-empty only; keyed by package_name so all devices benefit. Last write wins.
func (d *DB) upsertAppIcons(ctx context.Context, q interface {
	Exec(context.Context, string, ...any) (pgconn.CommandTag, error)
}, packages []DevicePackage) error {
	var iconPkgs, iconVals []string
	for _, p := range packages {
		if p.Icon != "" {
			iconPkgs = append(iconPkgs, p.PackageName)
			iconVals = append(iconVals, p.Icon)
		}
	}
	if len(iconPkgs) == 0 {
		return nil
	}
	_, err := q.Exec(ctx, `
		INSERT INTO app_icons (package_name, icon)
		SELECT unnest($1::text[]), unnest($2::text[])
		ON CONFLICT (package_name) DO UPDATE SET icon = EXCLUDED.icon, updated_at = NOW()
	`, iconPkgs, iconVals)
	return err
}

func (d *DB) GetDevicePackages(ctx context.Context, deviceID uuid.UUID) ([]DevicePackage, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT dp.package_name, dp.app_name, dp.version_name,
		       `+effectiveSystemSQL+` AS effective_system,
		       dp.launchable, dp.updated_at, COALESCE(ai.icon, '')
		FROM device_packages dp
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
		if err := rows.Scan(&p.PackageName, &p.AppName, &p.VersionName, &p.EffectiveSystem, &p.Launchable, &p.UpdatedAt, &p.Icon); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// vendorPackageSQL is the automatic system-app rule by name (26 Oct 2026; the user:
// "no need for manual classification", which retired the admin flag list): packages that
// come with the firmware (AOSP, Google, Qualcomm / CodeAurora, MediaTek, Chromium, NXP,
// test apps) count as system even where a client reports them as user apps, which
// firmware-bundled apps often are. A device's own is_system report still applies on top.
func vendorPackageSQL(col string) string {
	return "(" + col + " = 'android' OR " + col + " = 'io.github.vvb2060.keyattestation' OR " + col +
		" LIKE ANY (ARRAY['android.%', 'com.android.%', 'com.google.android.%', 'com.qualcomm.%'," +
		" 'com.qti.%', 'com.quicinc.%', 'org.codeaurora.%', 'com.caf.%', 'com.mediatek.%'," +
		" 'org.chromium.%', 'com.nxp.%', 'com.example.%', 'my.tests.%']))"
}

// effectiveSystemSQL: the package row counts as system on that device.
var effectiveSystemSQL = "(COALESCE(dp.is_system, true) OR " + vendorPackageSQL("dp.package_name") + ")"

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
			LEFT JOIN app_icons ai ON ai.package_name = dp.package_name
			WHERE (dp.package_name ILIKE $1 OR dp.app_name ILIKE $1)
			  AND NOT `+effectiveSystemSQL+`
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
			LEFT JOIN app_icons ai ON ai.package_name = dp.package_name
			WHERE NOT `+effectiveSystemSQL+`
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
		LEFT JOIN app_icons ai ON ai.package_name = dp.package_name
		WHERE dp.device_id = ANY($1)
		  AND NOT `+effectiveSystemSQL+`
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
		WHERE dp.device_id = ANY($1)
		  AND NOT `+effectiveSystemSQL+`
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
// device's client classified it (system / user / not-reported) and whether it is a
// vendor package by name. Optional name filter.
func (d *DB) ListPackagesAdmin(ctx context.Context, query string) ([]AdminPackage, error) {
	base := `
		SELECT
			dp.package_name,
			COALESCE(MAX(dp.app_name), '') AS app_name,
			COUNT(DISTINCT dp.device_id) AS device_count,
			COUNT(*) FILTER (WHERE dp.is_system IS TRUE)  AS rep_system,
			COUNT(*) FILTER (WHERE dp.is_system IS FALSE) AS rep_user,
			COUNT(*) FILTER (WHERE dp.is_system IS NULL)  AS rep_unknown,
			bool_or(` + vendorPackageSQL("dp.package_name") + `) AS vendor,
			COALESCE(MAX(ai.icon), '') AS icon,
			COUNT(*) FILTER (WHERE dp.launchable IS TRUE)     AS rep_launchable,
			COUNT(*) FILTER (WHERE dp.launchable IS NOT NULL) AS rep_launch_known
		FROM device_packages dp
		LEFT JOIN app_icons ai ON ai.package_name = dp.package_name`
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
			&p.ReportedSystem, &p.ReportedUser, &p.ReportedUnknown, &p.Vendor, &p.Icon,
			&p.ReportedLaunchable, &p.ReportedLaunchKnown); err != nil {
			return nil, err
		}
		// Effective classification, matching effectiveSystemSQL: a vendor package or a
		// client system report wins, then a client user report, then unknown defaults
		// to system.
		switch {
		case p.Vendor || p.ReportedSystem > 0:
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
