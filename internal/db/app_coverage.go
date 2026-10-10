package db

import (
	"context"

	"github.com/google/uuid"
)

// Fleet app coverage: which devices have which app, at which version.
//
// device_packages has held this since app inventory was added, and it is read today only
// to gate kiosk rules and to skip devices that already have an APK. It has never been
// shown fleet-wide, so nobody could answer the question an operator actually has —
// "where is this app, and where is it wrong". One query over an existing table answers it.

// PackageInstall is one (package, version) pair and how many live devices report it.
type PackageInstall struct {
	PackageName string
	AppName     string
	Version     string // "" means the device reported the package with no version
	Devices     int
}

// AppInstallCounts returns install counts per package and version, for the apps that are
// ours or in the library — the fleet also reports a few hundred Google and vendor system
// packages, which are inventory, not something anyone manages here.
//
// Joined to the live device set on purpose: device_packages keeps rows for devices that
// were hidden or retired (206 device ids against 187 live devices on 9 Oct 2026), so an
// unjoined count would quietly inflate every denominator on the page.
func (d *DB) AppInstallCounts(ctx context.Context) ([]PackageInstall, error) {
	rows, err := d.pool.Query(ctx, `
		WITH live AS (
			SELECT id FROM devices
			WHERE NOT hidden AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')
		)
		SELECT dp.package_name,
		       COALESCE(MAX(NULLIF(dp.app_name, '')), '') AS app_name,
		       COALESCE(dp.version_name, '')              AS version,
		       COUNT(*)                                   AS devices
		FROM device_packages dp
		JOIN live l ON l.id = dp.device_id
		WHERE dp.package_name LIKE 'aio.app%'
		   OR dp.package_name LIKE 'com.aioapp%'
		   OR EXISTS (SELECT 1 FROM apps a WHERE a.package_name = dp.package_name)
		GROUP BY dp.package_name, COALESCE(dp.version_name, '')
		ORDER BY dp.package_name, devices DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PackageInstall
	for rows.Next() {
		var p PackageInstall
		if err := rows.Scan(&p.PackageName, &p.AppName, &p.Version, &p.Devices); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// FleetAppTotals is the denominator for everything on the coverage view, and the honesty
// check on it: a device that has never reported its apps is absent from the numerator for
// every app, so it has to be visible rather than silently lowering coverage.
type FleetAppTotals struct {
	Devices       int // live devices
	ReportingApps int // live devices that have reported at least one package
}

func (d *DB) FleetAppTotals(ctx context.Context) (FleetAppTotals, error) {
	var t FleetAppTotals
	err := d.pool.QueryRow(ctx, `
		WITH live AS (
			SELECT id FROM devices
			WHERE NOT hidden AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')
		)
		SELECT (SELECT COUNT(*) FROM live),
		       (SELECT COUNT(DISTINCT dp.device_id) FROM device_packages dp JOIN live l ON l.id = dp.device_id)`).
		Scan(&t.Devices, &t.ReportingApps)
	return t, err
}

// DevicesWithPackageVersion lists the devices reporting one package, with the version each
// one has — the drill-down behind a coverage row. "" version means the device reported the
// package and no version, which is a different thing from being up to date.
type DeviceAppRow struct {
	DeviceID uuid.UUID
	Serial   string
	Version  string
	Venue    string
}

func (d *DB) DevicesWithPackage(ctx context.Context, pkg string) ([]DeviceAppRow, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT d.id, d.serial_number, COALESCE(dp.version_name, ''), COALESCE(r.name, '')
		FROM device_packages dp
		JOIN devices d ON d.id = dp.device_id
		LEFT JOIN restaurants r ON r.id = d.restaurant_id
		WHERE dp.package_name = $1
		  AND NOT d.hidden AND d.enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')
		ORDER BY COALESCE(dp.version_name, '') DESC, d.serial_number`, pkg)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceAppRow
	for rows.Next() {
		var x DeviceAppRow
		if err := rows.Scan(&x.DeviceID, &x.Serial, &x.Version, &x.Venue); err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}
