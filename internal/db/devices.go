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

	prod "mdm/internal/product"
)

// ── Device nicknames ──────────────────────────────────────────────────────────

func (d *DB) GetNicknames(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]string, error) {
	out := map[uuid.UUID]string{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `SELECT device_id, name FROM device_nicknames WHERE device_id = ANY($1)`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id uuid.UUID
		var n string
		if err := rows.Scan(&id, &n); err != nil {
			return nil, err
		}
		out[id] = n
	}
	return out, rows.Err()
}

func (d *DB) SetNickname(ctx context.Context, id uuid.UUID, name string) error {
	name = strings.TrimSpace(name)
	if name == "" {
		_, err := d.pool.Exec(ctx, `DELETE FROM device_nicknames WHERE device_id = $1`, id)
		return err
	}
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_nicknames (device_id, name) VALUES ($1, $2)
		ON CONFLICT (device_id) DO UPDATE SET name = EXCLUDED.name, updated_at = NOW()`, id, name)
	return err
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
		w, a := deviceSearchWhere(f.Search, &argN)
		wheres = append(wheres, w)
		args = append(args, a...)
	}
	if f.AgentKind != "" {
		w, a := agentKindWhere(f.AgentKind, &argN)
		wheres = append(wheres, w)
		args = append(args, a...)
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
	if w := hygieneWhere(f.Hygiene); w != "" {
		wheres = append(wheres, w)
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
	// The product tabs filter by class, so the quick-view counts under them have to
	// as well — otherwise picking "Tableside AI" leaves the pills counting the whole
	// fleet. Same predicate ListDevices uses, including the product-default fallback
	// for firmware devices that store no class.
	if f.Class != "" {
		wheres = append(wheres, fmt.Sprintf("(d.device_class = $%d OR (d.device_class = '' AND d.product = ANY($%d)))", argN, argN+1))
		args = append(args, f.Class, productKeysForClass(f.Class))
		argN += 2
	}
	_ = activeSecs // retained for signature compatibility; online is now WS-based
	args = append(args, f.Connected)
	connArg := argN
	q := fmt.Sprintf(`SELECT
			COUNT(d.id),
			COUNT(*) FILTER (WHERE d.id = ANY($%d::uuid[])),
			COUNT(*) FILTER (WHERE d.latest_battery_pct < 20 AND `+hasBatteryD+`),
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
			COUNT(*) FILTER (WHERE d.latest_battery_pct < 20 AND `+hasBatteryD+`),
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
		if err := rows.Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct, &dev.PollIntervalMs, &dev.KioskEnabled, &dev.KioskPackage, &dev.LatestExtra, &dev.Hidden, &dev.RestaurantName, &dev.DischargeTotalPct, &dev.DischargeBackfilled, &dev.Product, &dev.AgentKind, &dev.DeviceClass, &dev.Capabilities, &dev.CapabilitiesDegraded, &dev.EnrollmentStatus, &dev.EnrolledAt, &dev.KeyRotatedAt, &dev.OnboardedAt, &dev.EnrolledBy, &dev.RestaurantID, &dev.DeployedEffective); err != nil {
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

// agentKindWhere matches the fleet's Agent filter. MDM-lite devices are stored as
// dpc with latest_extra.agent_type "mdm-lite", so "dpc" means the DPC agent proper.
// deviceSearchWhere is the fleet search predicate. Operators know a device by the
// name on the ticket ("front counter kiosk", "Downtown"), not by its serial, so the
// one search box matches the serial, the device's nickname, the venue it is placed
// in and any group it belongs to. The name matches are EXISTS subqueries rather than
// joins so a device in three matching groups still returns one row, and so the
// caller's join list (and its argument numbering) is untouched.
func deviceSearchWhere(search string, argN *int) (string, []interface{}) {
	n := *argN
	*argN++
	return fmt.Sprintf(`(d.serial_number ILIKE $%[1]d
		   OR EXISTS (SELECT 1 FROM device_nicknames dns WHERE dns.device_id = d.id AND dns.name ILIKE $%[1]d)
		   OR EXISTS (SELECT 1 FROM restaurants rs WHERE rs.id = d.restaurant_id AND rs.name ILIKE $%[1]d)
		   OR EXISTS (SELECT 1 FROM device_groups dgs JOIN groups gs ON gs.id = dgs.group_id
		              WHERE dgs.device_id = d.id AND gs.name ILIKE $%[1]d))`, n),
		[]interface{}{"%" + search + "%"}
}

func agentKindWhere(kind string, argN *int) (string, []interface{}) {
	isLite := "COALESCE(d.latest_extra->>'agent_type', '') = '" + prod.AgentTypeMDMLite + "'"
	switch kind {
	case prod.AgentTypeMDMLite:
		return "d.agent_kind = '" + prod.KindDPC + "' AND " + isLite, nil
	case prod.KindDPC:
		return "d.agent_kind = '" + prod.KindDPC + "' AND NOT " + isLite, nil
	}
	w := fmt.Sprintf("d.agent_kind = $%d", *argN)
	*argN++
	return w, []interface{}{kind}
}

// SQL forms of the card vitals. Numbers only: a malformed value reads as no value.
const (
	ramPctSQL = `(CASE WHEN jsonb_typeof(d.latest_extra->'ram_usage_mb'->'used') = 'number'
		AND jsonb_typeof(d.latest_extra->'ram_usage_mb'->'total') = 'number'
		THEN (d.latest_extra->'ram_usage_mb'->>'used')::numeric * 100 / NULLIF((d.latest_extra->'ram_usage_mb'->>'total')::numeric, 0) END)`
	batteryTempSQL = `(CASE WHEN jsonb_typeof(d.latest_extra->'battery_temp_c') = 'number' THEN (d.latest_extra->>'battery_temp_c')::numeric END)`
	cpuTempSQL     = `(CASE WHEN jsonb_typeof(d.latest_extra->'cpu_temp_c') = 'number' THEN (d.latest_extra->>'cpu_temp_c')::numeric END)`
	// tempSQL is the reading the card shows: the battery sensor, else the SoC.
	tempSQL = "COALESCE(" + batteryTempSQL + ", " + cpuTempSQL + ")"
)

// Temperature bands, shared with the dashboard's colouring (dashboard.tempLevel).
const (
	BatteryTempWarn, BatteryTempDanger = 40, 60
	CPUTempWarn, CPUTempDanger         = 70, 85
	RAMWarnPct, RAMDangerPct           = 60, 80
)

// vitalsWhere is the RAM and temperature filters as an AND-prefixed clause.
func vitalsWhere(f DeviceFilter) string {
	var out string
	switch f.RAM {
	case "high":
		out += fmt.Sprintf(" AND %s >= %d", ramPctSQL, RAMDangerPct)
	case "warn":
		out += fmt.Sprintf(" AND %s >= %d", ramPctSQL, RAMWarnPct)
	}
	bw, bd, cw, cd := BatteryTempWarn, BatteryTempDanger, CPUTempWarn, CPUTempDanger
	switch f.Temp {
	case "hot":
		out += fmt.Sprintf(" AND (%s >= %d OR (%s IS NULL AND %s >= %d))", batteryTempSQL, bd, batteryTempSQL, cpuTempSQL, cd)
	case "warm":
		out += fmt.Sprintf(" AND (%s >= %d OR (%s IS NULL AND %s >= %d))", batteryTempSQL, bw, batteryTempSQL, cpuTempSQL, cw)
	}
	return out
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

	// Lifecycle: retired/wiped devices stay out of every list unless asked for.
	switch f.Lifecycle {
	case "retired":
		wheres = append(wheres, "d.enrollment_status IN ('retired', 'wiped', 'unenrolled')")
	case "all":
	default:
		wheres = append(wheres, "d.enrollment_status NOT IN ('retired', 'wiped', 'unenrolled')")
	}
	switch f.Onboarding {
	case "pending":
		wheres = append(wheres, "d.onboarded_at IS NULL")
	case "done":
		wheres = append(wheres, "d.onboarded_at IS NOT NULL")
	}
	if f.EnrolledBy != "" {
		wheres = append(wheres, fmt.Sprintf("d.enrolled_by = $%d", argN))
		args = append(args, f.EnrolledBy)
		argN++
	}
	if f.AgentKind != "" {
		w, a := agentKindWhere(f.AgentKind, &argN)
		wheres = append(wheres, w)
		args = append(args, a...)
	}
	if f.Class != "" {
		// Firmware devices rarely store a class; match the product default too.
		wheres = append(wheres, fmt.Sprintf("(d.device_class = $%d OR (d.device_class = '' AND d.product = ANY($%d)))", argN, argN+1))
		args = append(args, f.Class, productKeysForClass(f.Class))
		argN += 2
	}

	if f.OnlyIDs != nil {
		wheres = append(wheres, fmt.Sprintf("d.id = ANY($%d)", argN))
		args = append(args, f.OnlyIDs)
		argN++
	}
	if f.Search != "" {
		w, a := deviceSearchWhere(f.Search, &argN)
		wheres = append(wheres, w)
		args = append(args, a...)
	}

	if f.GroupID != uuid.Nil {
		joins = append(joins, fmt.Sprintf("JOIN device_groups dg ON dg.device_id = d.id AND dg.group_id = $%d", argN))
		args = append(args, f.GroupID)
		argN++
	}

	if f.ExcludeRestaurantID != uuid.Nil {
		// A picker that assigns devices TO a venue should not offer the ones already
		// there — they would be a no-op move, and they crowd out the devices you came
		// to find.
		wheres = append(wheres, fmt.Sprintf("d.restaurant_id IS DISTINCT FROM $%d", argN))
		args = append(args, f.ExcludeRestaurantID)
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
	if w := hygieneWhere(f.Hygiene); w != "" {
		wheres = append(wheres, w)
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
			d.product,
			d.agent_kind, d.device_class, d.capabilities, d.capabilities_degraded,
			d.enrollment_status, d.enrolled_at, d.key_rotated_at, d.onboarded_at, d.enrolled_by,
			d.restaurant_id, (d.restaurant_id IS NOT NULL)
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
			base += " AND "+hasBatteryD+" AND d.latest_battery_pct < 20"
		case "mid":
			base += " AND "+hasBatteryD+" AND d.latest_battery_pct BETWEEN 20 AND 49"
		case "ok":
			base += " AND "+hasBatteryD+" AND d.latest_battery_pct >= 50"
		}
		base += vitalsWhere(f)

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
			orderClause = "COALESCE(" + ramPctSQL + ", 0) "
			if dir == "desc" {
				orderClause += "DESC"
			} else {
				orderClause += "ASC"
			}
		case "temp":
			if dir == "desc" {
				orderClause = "COALESCE(" + tempSQL + ", 0) DESC"
			} else {
				orderClause = "COALESCE(" + tempSQL + ", 0) ASC"
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
		case "enrolled_at":
			if dir == "asc" {
				orderClause = "d.enrolled_at ASC"
			} else {
				orderClause = "d.enrolled_at DESC"
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
		base += " AND "+hasBatteryD+" AND d.latest_battery_pct < 20"
	case "mid":
		base += " AND "+hasBatteryD+" AND d.latest_battery_pct BETWEEN 20 AND 49"
	case "ok":
		base += " AND "+hasBatteryD+" AND d.latest_battery_pct >= 50"
	}
	base += vitalsWhere(f)

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



// ListAllSerials returns every device serial (visible and hidden), so callers can
// detect and linkify serials named in free text (e.g. the AI report prose).
// FamilySample is what the enroll app learns hardware families from: one live device's serial, the
// class it was given, and the manufacturer/model it reports.
type FamilySample struct {
	Serial       string
	DeviceClass  string
	Manufacturer string
	Model        string
}

// FamilySamples returns every device that has not been retired, wiped or hidden. Fabricated
// serials (the DPC agent's android-<id> fallback, corrupt msm- reads) are left for the caller to drop.
func (d *DB) FamilySamples(ctx context.Context) ([]FamilySample, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT serial_number, COALESCE(device_class, ''), COALESCE(product, ''),
		       COALESCE(latest_extra->>'manufacturer', ''), COALESCE(latest_extra->>'model', '')
		FROM devices
		WHERE NOT hidden AND enrollment_status NOT IN ('retired', 'wiped', 'unenrolled') AND serial_number <> ''`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []FamilySample
	for rows.Next() {
		var f FamilySample
		var product string
		if err := rows.Scan(&f.Serial, &f.DeviceClass, &product, &f.Manufacturer, &f.Model); err != nil {
			return nil, err
		}
		// Firmware devices often leave device_class empty and carry their class in the
		// product they report: resolve it the way Device.Class does.
		if f.DeviceClass == "" && product != "" {
			p, _ := prod.Resolve(product)
			f.DeviceClass = p.Class
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// DeviceEnrolledBy is the username that enrolled the device through the enroll app ('' if unknown).
func (d *DB) DeviceEnrolledBy(ctx context.Context, serial string) (string, error) {
	var by string
	err := d.pool.QueryRow(ctx, `SELECT enrolled_by FROM devices WHERE serial_number = $1`, serial).Scan(&by)
	return by, err
}

// SetEnrolledBy records the username that enrolled the given devices (admin backfill for devices
// enrolled before this was tracked). Returns how many devices changed.
func (d *DB) SetEnrolledBy(ctx context.Context, serials []string, username string) (int64, error) {
	tag, err := d.pool.Exec(ctx, `UPDATE devices SET enrolled_by = $2 WHERE serial_number = ANY($1::text[])`, serials, username)
	if err != nil {
		return 0, err
	}
	return tag.RowsAffected(), nil
}

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
			d.product,
			d.agent_kind, d.device_class, d.capabilities, d.capabilities_degraded,
			d.enrollment_status, d.enrolled_at, d.key_rotated_at, d.onboarded_at, d.enrolled_by
		FROM devices d
		LEFT JOIN device_config dc ON dc.device_id = d.id
		LEFT JOIN restaurants r ON r.id = d.restaurant_id
		WHERE d.serial_number = $1
	`, serial).Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct, &dev.PollIntervalMs, &dev.KioskEnabled, &dev.KioskPackage, &dev.LatestExtra, &dev.DischargeTotalPct, &dev.DischargeLegacyPct, &dev.DischargeBackfilled, &dev.RestaurantID, &dev.RestaurantName, &dev.DeployedEffective, &dev.Product, &dev.AgentKind, &dev.DeviceClass, &dev.Capabilities, &dev.CapabilitiesDegraded, &dev.EnrollmentStatus, &dev.EnrolledAt, &dev.KeyRotatedAt, &dev.OnboardedAt, &dev.EnrolledBy)
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
			d.product,
			d.agent_kind, d.device_class, d.capabilities, d.capabilities_degraded,
			d.enrollment_status, d.enrolled_at, d.key_rotated_at, d.onboarded_at, d.enrolled_by
		FROM devices d
		LEFT JOIN device_config dc ON dc.device_id = d.id
		WHERE d.id = $1
	`, id).Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct, &dev.PollIntervalMs, &dev.KioskEnabled, &dev.KioskPackage, &dev.LatestExtra, &dev.Hidden, &dev.Product, &dev.AgentKind, &dev.DeviceClass, &dev.Capabilities, &dev.CapabilitiesDegraded, &dev.EnrollmentStatus, &dev.EnrolledAt, &dev.KeyRotatedAt, &dev.OnboardedAt, &dev.EnrolledBy)
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

// GetDeviceLive returns the device's current state for the live page stream: the
// snapshot on the devices row, not the newest history row.
//
// It is both fresher and cheaper. Fresher because devices.latest_extra is written on
// every report, while a check-in row is only stored when something changed or the
// sampling window elapsed — so the history-row version of "live" could sit a minute
// behind what the device just said. Cheaper because it is one row on a 193-row table by
// primary key, instead of an ordered read of the newest row from a table of millions
// with a jsonb column to detoast.
//
// Shaped as a Checkin so the payload builder is unchanged: CreatedAt carries
// last_seen_at, the instant the device last reported.
func (d *DB) GetDeviceLive(ctx context.Context, deviceID uuid.UUID) (*Checkin, error) {
	var c Checkin
	err := d.pool.QueryRow(ctx, `
		SELECT id, COALESCE(latest_battery_pct, 0), build_id, COALESCE(latest_extra, '{}'::jsonb), last_seen_at
		FROM devices WHERE id = $1
	`, deviceID).Scan(&c.DeviceID, &c.BatteryPct, &c.BuildID, &c.Extra, &c.CreatedAt)
	if err != nil {
		return nil, err
	}
	return &c, nil
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





// chartExtraKeys are the only extra keys the device graph reads (temperature, RAM,
// wireless charging, charging state, and a flapping charger on the charge strip).
var chartExtraKeys = []string{"battery_temp_c", "cpu_temp_c", "ram_usage_mb", "wlc_status", "charging", "charger_flapping"}


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

func (d *DB) SearchDevicesBySerial(ctx context.Context, query string, limit int) ([]Device, error) {
	hits, err := d.SearchDevices(ctx, query, limit, nil)
	if err != nil {
		return nil, err
	}
	devs := make([]Device, len(hits))
	for i, h := range hits {
		devs[i] = h.Device
	}
	return devs, nil
}

// DeviceHit is one search result: the device, how it matched (lower is better —
// see SearchDevices) and its last reported IP address.
type DeviceHit struct {
	Device
	Rank int
	IP   string
}

// SearchDevices is the fleet search behind ⌘K and the type-ahead pickers.
//
// Fuzzy serial match: the typed characters must appear in order anywhere in the
// serial (subsequence), so "at866" or "a070b86" both find "AT070AABU00866".
// The nickname, the last IP and the venue name are matched as plain contiguous
// substrings — a name someone typed ("front counter") is not a code to be fuzzed,
// and subsequence matching over free text matches almost everything.
//
// Rank: 0 exact serial, 1 exact nickname or IP, 2 serial substring, 3 nickname
// substring, 4 IP substring, 5 serial subsequence, 6 venue name. Shorter serials
// break ties. %/_/\ in the query are escaped so they're matched literally rather
// than acting as ILIKE wildcards.
//
// onlyIDs, when non-nil, limits the search to those devices. It is applied before
// LIMIT: filtering afterwards let eight hidden devices fill the page and a
// restricted user's own device never come back.
func (d *DB) SearchDevices(ctx context.Context, query string, limit int, onlyIDs []uuid.UUID) ([]DeviceHit, error) {
	return d.searchDevices(ctx, query, limit, onlyIDs, nil)
}

// DevicesBySerials loads the named devices in the same shape as a search hit (the
// palette's recent devices), limited to onlyIDs like SearchDevices.
func (d *DB) DevicesBySerials(ctx context.Context, serials []string, onlyIDs []uuid.UUID) ([]DeviceHit, error) {
	if len(serials) == 0 {
		return nil, nil
	}
	return d.searchDevices(ctx, "", len(serials), onlyIDs, serials)
}

func (d *DB) searchDevices(ctx context.Context, query string, limit int, onlyIDs []uuid.UUID, serials []string) ([]DeviceHit, error) {
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
		SELECT * FROM (
			SELECT
				d.id, d.serial_number, d.build_id, d.last_seen_at, d.created_at,
				d.latest_battery_pct AS battery_pct,
				d.poll_interval_ms,
				COALESCE(dc.kiosk_enabled, false),
				COALESCE(dc.kiosk_package, ''),
				d.latest_extra AS latest_extra,
				COALESCE(dn.name, ''),
				COALESCE(r.name, ''), d.device_class, d.product, d.agent_kind,
				COALESCE(d.latest_extra->>'ip_address', '') AS ip,
				CASE WHEN lower(d.serial_number) = lower($2) THEN 0
				     WHEN lower(COALESCE(dn.name, '')) = lower($2)
				       OR COALESCE(d.latest_extra->>'ip_address', '') = $2 THEN 1
				     WHEN d.serial_number ILIKE $3 THEN 2
				     WHEN dn.name ILIKE $3 THEN 3
				     WHEN d.latest_extra->>'ip_address' ILIKE $3 THEN 4
				     WHEN d.serial_number ILIKE $1 THEN 5
				     ELSE 6 END AS rank
			FROM devices d
			LEFT JOIN device_config dc ON dc.device_id = d.id
			LEFT JOIN device_nicknames dn ON dn.device_id = d.id
			LEFT JOIN restaurants r ON r.id = d.restaurant_id
			WHERE CASE WHEN $6::text[] IS NULL
			           THEN d.serial_number ILIKE $1 OR dn.name ILIKE $3
			                OR d.latest_extra->>'ip_address' ILIKE $3 OR r.name ILIKE $3
			           ELSE d.serial_number = ANY($6) END
			  AND NOT d.hidden
			  AND ($5::uuid[] IS NULL OR d.id = ANY($5))
		) m
		ORDER BY rank, length(serial_number), serial_number
		LIMIT $4
	`, subseq, query, contig, limit, onlyIDs, serials)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var hits []DeviceHit
	for rows.Next() {
		var h DeviceHit
		dev := &h.Device
		if err := rows.Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.CreatedAt, &dev.BatteryPct, &dev.PollIntervalMs, &dev.KioskEnabled, &dev.KioskPackage, &dev.LatestExtra, &dev.Nickname,
			&dev.RestaurantName, &dev.DeviceClass, &dev.Product, &dev.AgentKind, &h.IP, &h.Rank); err != nil {
			return nil, err
		}
		hits = append(hits, h)
	}
	return hits, rows.Err()
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

// WlcEnabledBySerial maps each device's serial to whether its charging pad is
// enabled. The CSV export needs it per row, and the setting lives in config, not in
// telemetry, so it is read once up front (FW-2026-000052).
func (d *DB) WlcEnabledBySerial(ctx context.Context, ids []uuid.UUID) (map[string]bool, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT dv.serial_number, COALESCE(dc.wlc_charging_enabled, TRUE)
		FROM devices dv
		LEFT JOIN device_config dc ON dc.device_id = dv.id
		WHERE dv.id = ANY($1)
	`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[string]bool{}
	for rows.Next() {
		var serial string
		var on bool
		if err := rows.Scan(&serial, &on); err != nil {
			return nil, err
		}
		out[serial] = on
	}
	return out, rows.Err()
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
		SELECT id, serial_number, COALESCE(build_id,''), last_seen_at, COALESCE(latest_battery_pct,0),
		       product, agent_kind, device_class, capabilities, capabilities_degraded, enrollment_status
		FROM devices WHERE id = ANY($1) AND NOT hidden`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Device
	for rows.Next() {
		var dev Device
		if err := rows.Scan(&dev.ID, &dev.SerialNumber, &dev.BuildID, &dev.LastSeenAt, &dev.BatteryPct,
			&dev.Product, &dev.AgentKind, &dev.DeviceClass, &dev.Capabilities, &dev.CapabilitiesDegraded, &dev.EnrollmentStatus); err != nil {
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

// CountDPCDevices reports how many of the given devices run the standalone
// Device-Owner ("dpc") agent (devices.agent_kind, learned from agent_type on
// checkin. Powers the Actions console's capability-aware grid: system-app-only
// actions dim on DPC devices, and wipe is DPC-only.
func (d *DB) CountDPCDevices(ctx context.Context, ids []uuid.UUID) (int, error) {
	if len(ids) == 0 {
		return 0, nil
	}
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM devices
		WHERE id = ANY($1) AND NOT hidden AND agent_kind = 'dpc'`, ids).Scan(&n)
	return n, err
}

// MDMLiteIDs picks out the targets running MDM Lite. Lite is stored as agent_kind
// 'dpc' — every non-firmware rule applies to it — and is told apart only by
// latest_extra.agent_type, which GetDevicesByIDs deliberately does not load: that
// column is the whole check-in blob, and a fleet-wide action resolves thousands of
// rows. Asking for the one field keeps Device.IsMDMLite's answer available without
// dragging the blob along.
func (d *DB) MDMLiteIDs(ctx context.Context, ids []uuid.UUID) (map[uuid.UUID]bool, error) {
	out := map[uuid.UUID]bool{}
	if len(ids) == 0 {
		return out, nil
	}
	rows, err := d.pool.Query(ctx, `
		SELECT id FROM devices
		WHERE id = ANY($1) AND NOT hidden AND latest_extra->>'agent_type' = 'mdm-lite'`, ids)
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

// GetDeviceIDsInGroups expands a set of group IDs into the distinct device IDs that
// belong to any of them. Used to target ad-hoc actions (e.g. a fleet log capture)
// at whole groups while operating on concrete devices.
// GetDeviceIDsInRestaurants lists the active devices placed at any of the sites.
func (d *DB) GetDeviceIDsInRestaurants(ctx context.Context, ids []uuid.UUID) ([]uuid.UUID, error) {
	if len(ids) == 0 {
		return nil, nil
	}
	rows, err := d.pool.Query(ctx, `SELECT id FROM devices WHERE restaurant_id = ANY($1) AND NOT hidden`, ids)
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
