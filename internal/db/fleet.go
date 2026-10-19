package db

import (
	"context"
	"time"

	"github.com/google/uuid"

	prod "mdm/internal/product"
)

// FleetCounts holds the headline totals shown in the unified Fleet tab strip
// (Devices / Restaurants / Groups), fetched in one round-trip.
type FleetCounts struct {
	Devices     int `json:"devices"`
	Restaurants int `json:"restaurants"`
	Groups      int `json:"groups"`
	Inbox       int `json:"inbox"` // devices waiting in the onboarding inbox
}

// FleetCounts returns visible-device, restaurant, and group totals in one query.
func (d *DB) FleetCounts(ctx context.Context, excludeDPC bool) (FleetCounts, error) {
	var c FleetCounts
	kindWhere := ""
	if excludeDPC {
		kindWhere = " AND agent_kind <> 'dpc'"
	}
	err := d.pool.QueryRow(ctx, `
		SELECT (SELECT COUNT(*) FROM devices WHERE NOT hidden AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')`+kindWhere+`),
		       (SELECT COUNT(*) FROM restaurants),
		       (SELECT COUNT(*) FROM groups),
		       (SELECT COUNT(*) FROM devices WHERE NOT hidden AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled') AND onboarded_at IS NULL)
	`).Scan(&c.Devices, &c.Restaurants, &c.Groups, &c.Inbox)
	return c, err
}

// ClassCount is one slice of the fleet composition strip.
type ClassCount struct {
	Class string
	N     int
}

// FleetComposition counts active devices per class (stored class, else the
// product default) and per agent kind, in one query. Feeds the Fleet page's
// composition strip and its filter counts.
func (d *DB) FleetComposition(ctx context.Context, excludeDPC bool) (classes []ClassCount, firmware, dpc int, err error) {
	kindWhere := ""
	if excludeDPC {
		kindWhere = " AND agent_kind <> 'dpc'"
	}
	rows, err := d.pool.Query(ctx, `
		SELECT `+derivedClassSQL()+` AS cls,
		       agent_kind, COUNT(*)
		FROM devices
		WHERE NOT hidden AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')`+kindWhere+`
		GROUP BY 1, 2`)
	if err != nil {
		return nil, 0, 0, err
	}
	defer rows.Close()
	byClass := map[string]int{}
	for rows.Next() {
		var cls, kind string
		var n int
		if err := rows.Scan(&cls, &kind, &n); err != nil {
			return nil, 0, 0, err
		}
		byClass[cls] += n
		if kind == prod.KindDPC {
			dpc += n
		} else {
			firmware += n
		}
	}
	if err := rows.Err(); err != nil {
		return nil, 0, 0, err
	}
	for _, c := range prod.Classes() {
		if byClass[c] > 0 {
			classes = append(classes, ClassCount{c, byClass[c]})
		}
	}
	if byClass[""] > 0 {
		classes = append(classes, ClassCount{"", byClass[""]})
	}
	return classes, firmware, dpc, nil
}

// ClassOnline is one device class with its online share, for the Overview's
// devices-by-type table.
type ClassOnline struct {
	Class  string
	Total  int
	Online int
}

// FleetClassOnline counts active devices per class and how many of them are in
// connected (the hub's live set), in the class order of prod.Classes(), with
// unclassified devices last under Class "".
func (d *DB) FleetClassOnline(ctx context.Context, connected []uuid.UUID, excludeDPC bool) ([]ClassOnline, error) {
	kindWhere := ""
	if excludeDPC {
		kindWhere = " AND agent_kind <> 'dpc'"
	}
	rows, err := d.pool.Query(ctx, `
		SELECT `+derivedClassSQL()+` AS cls,
		       COUNT(*), COUNT(*) FILTER (WHERE id = ANY($1::uuid[]))
		FROM devices
		WHERE NOT hidden AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')`+kindWhere+`
		GROUP BY 1`, connected)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	byClass := map[string]ClassOnline{}
	for rows.Next() {
		var c ClassOnline
		if err := rows.Scan(&c.Class, &c.Total, &c.Online); err != nil {
			return nil, err
		}
		byClass[c.Class] = c
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	var out []ClassOnline
	for _, c := range prod.Classes() {
		if v, ok := byClass[c]; ok && v.Total > 0 {
			out = append(out, v)
		}
	}
	if v, ok := byClass[""]; ok && v.Total > 0 {
		out = append(out, v)
	}
	return out, nil
}

// WallDevice is one square of the Overview's device wall.
type WallDevice struct {
	ID           uuid.UUID
	Serial       string
	Class        string
	RestaurantID *uuid.UUID
	LastSeenAt   time.Time
}

// FleetWall lists every active device with its class and restaurant, for the
// Overview's device wall. Same population as FleetClassOnline.
func (d *DB) FleetWall(ctx context.Context, excludeDPC bool) ([]WallDevice, error) {
	kindWhere := ""
	if excludeDPC {
		kindWhere = " AND agent_kind <> 'dpc'"
	}
	rows, err := d.pool.Query(ctx, `
		SELECT id, serial_number, `+derivedClassSQL()+`, restaurant_id, last_seen_at
		FROM devices
		WHERE NOT hidden AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')`+kindWhere+`
		ORDER BY serial_number`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WallDevice
	for rows.Next() {
		var w WallDevice
		if err := rows.Scan(&w.ID, &w.Serial, &w.Class, &w.RestaurantID, &w.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, w)
	}
	return out, rows.Err()
}

// EnrollmentStats feeds the Enroll page's number strip.
type EnrollmentStats struct {
	Inbox       int // waiting for assignment
	Enrolled7d  int // devices that enrolled / first checked in during the last 7 days
	Firmware    int // active firmware devices
	DPC         int // active DPC devices
	Retired     int
	ActiveProfs int // profiles that can still enroll
}

func (d *DB) EnrollmentStats(ctx context.Context) (EnrollmentStats, error) {
	var s EnrollmentStats
	err := d.pool.QueryRow(ctx, `
		WITH live AS (SELECT * FROM devices WHERE NOT hidden)
		SELECT (SELECT COUNT(*) FROM live WHERE onboarded_at IS NULL AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')),
		       (SELECT COUNT(*) FROM live WHERE enrolled_at > NOW() - INTERVAL '7 days'),
		       (SELECT COUNT(*) FROM live WHERE agent_kind = 'firmware' AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')),
		       (SELECT COUNT(*) FROM live WHERE agent_kind = 'dpc' AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')),
		       (SELECT COUNT(*) FROM live WHERE enrollment_status IN ('retired', 'wiped', 'unenrolled')),
		       (SELECT COUNT(*) FROM enrollment_profiles WHERE revoked_at IS NULL
		          AND (expires_at IS NULL OR expires_at > NOW())
		          AND (max_enrolls IS NULL OR enroll_count < max_enrolls))
	`).Scan(&s.Inbox, &s.Enrolled7d, &s.Firmware, &s.DPC, &s.Retired, &s.ActiveProfs)
	return s, err
}

// ProductCount is one hardware product actually present in the fleet, with the
// manufacturer and model its devices report — the name an admin recognises, rather
// than the key the client happens to send ("Sunmi D3 PRO", not "d3_pro").
type ProductCount struct {
	Product      string
	Devices      int
	Manufacturer string
	Model        string
}

// FleetProducts lists the distinct products devices report, with how many run each.
// The catalog covers our own hardware; this covers everything else that turned up —
// a Sunmi D3 PRO, a stock Pixel — so the dashboard can offer them as filters instead
// of pretending the fleet is only what the catalog knows about.
func (d *DB) FleetProducts(ctx context.Context) ([]ProductCount, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT COALESCE(NULLIF(product, ''), 't7') AS p, COUNT(*)::int,
		       COALESCE((array_agg(latest_extra->>'manufacturer') FILTER (WHERE latest_extra->>'manufacturer' <> ''))[1], ''),
		       COALESCE((array_agg(latest_extra->>'model') FILTER (WHERE latest_extra->>'model' <> ''))[1], '')
		FROM devices WHERE NOT hidden
		GROUP BY p ORDER BY COUNT(*) DESC, p`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []ProductCount
	for rows.Next() {
		var pc ProductCount
		if err := rows.Scan(&pc.Product, &pc.Devices, &pc.Manufacturer, &pc.Model); err != nil {
			return nil, err
		}
		out = append(out, pc)
	}
	return out, rows.Err()
}
