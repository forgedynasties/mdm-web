package db

import (
	"context"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

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
		WHERE device_id = $1 AND kind NOT IN ('reboot', 'kiosk_exit_offline', 'offline_dropped')
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
		WHERE e.build_id = $1 AND e.kind NOT IN ('reboot', 'kiosk_exit_offline', 'offline_dropped')
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
	// Set only by ListRecentCrashGroupsPage, where a row is a signature rather than a
	// single event: how many devices hit it and how many times in total. Zero means the
	// row is one raw event (Serial/OccurredAt are that event's).
	DeviceCount int
	EventCount  int
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
		WHERE e.kind NOT IN ('reboot', 'kiosk_exit_offline', 'offline_dropped') AND NOT dv.hidden
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

// CountDeviceCrashes is the badge count for the device page's Alerts tab — same
// filter as ListDeviceCrashes, without pulling the (large) trace payloads.
func (d *DB) CountDeviceCrashes(ctx context.Context, deviceID uuid.UUID) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM device_events e
		JOIN devices dv ON dv.id = e.device_id
		WHERE e.device_id = $1 AND e.kind NOT IN ('reboot', 'kiosk_exit_offline', 'offline_dropped')
		  AND e.build_id = dv.build_id`, deviceID).Scan(&n)
	return n, err
}

// ListDeviceCrashes returns crash/ANR/tombstone events (not reboots) for a single
// device, newest first — the crash feed for that device's Alerts tab. limit <= 0
// means 50.
//
// Only crashes on the build the device runs now: a crash from before an update is
// history, most likely fixed by it, and listing it on the device reads as a current
// fault. Each event carries the build it happened on (see the insert in the check-in
// path). The Alerts page's crash view still has every build.

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
		WHERE e.device_id = $1 AND e.kind NOT IN ('reboot', 'kiosk_exit_offline', 'offline_dropped')
		  AND e.build_id = dv.build_id
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
// CountRecentCrashEvents totals raw crash events (not signatures) so the KPI keeps
// reporting how much crashing is going on, even though the list below it merges
// identical crashes into one row.
func (d *DB) CountRecentCrashEvents(ctx context.Context, deviceID *uuid.UUID, sinceDays int) (int, error) {
	if sinceDays <= 0 {
		sinceDays = 7
	}
	scope := ""
	args := []any{sinceDays}
	if deviceID != nil {
		scope = " AND e.device_id = $2"
		args = append(args, *deviceID)
	}
	var n int
	err := d.pool.QueryRow(ctx, `
		SELECT COUNT(*)
		FROM device_events e
		JOIN devices dv ON dv.id = e.device_id
		WHERE e.kind NOT IN ('reboot', 'kiosk_exit_offline', 'offline_dropped') AND NOT dv.hidden
		  AND e.occurred_at > now() - make_interval(days => $1)`+scope, args...).Scan(&n)
	return n, err
}

// ListRecentCrashGroupsPage returns crashes merged by signature (kind + summary)
// instead of one row per event. One app crashing on forty devices is one problem, and
// listing it forty times buried everything else; the group carries how many devices hit
// it and how many times in total. deviceID scopes it to a single device (the group then
// answers "how often", not "how many devices").
//
// The sample columns come from the most recent event in the group, so the card shows a
// current serial, build and trace to open.
func (d *DB) ListRecentCrashGroupsPage(ctx context.Context, deviceID *uuid.UUID, sinceDays, limit, offset int) ([]CrashEvent, int, error) {
	if limit <= 0 {
		limit = 25
	}
	if offset < 0 {
		offset = 0
	}
	if sinceDays <= 0 {
		sinceDays = 7
	}
	scope := ""
	args := []any{sinceDays, limit, offset}
	if deviceID != nil {
		scope = " AND e.device_id = $4"
		args = append(args, *deviceID)
	}
	rows, err := d.pool.Query(ctx, `
		SELECT e.kind, e.summary,
		       COUNT(*) AS events,
		       COUNT(DISTINCT e.device_id) AS devices,
		       MAX(e.occurred_at) AS last_at,
		       (ARRAY_AGG(dv.serial_number ORDER BY e.occurred_at DESC))[1] AS serial,
		       (ARRAY_AGG(COALESCE(r.name, '') ORDER BY e.occurred_at DESC))[1] AS restaurant,
		       (ARRAY_AGG(e.build_id ORDER BY e.occurred_at DESC))[1] AS build_id,
		       (ARRAY_AGG(e.detail ORDER BY e.occurred_at DESC))[1] AS detail,
		       COUNT(*) OVER() AS total
		FROM device_events e
		JOIN devices dv ON dv.id = e.device_id
		LEFT JOIN restaurants r ON r.id = dv.restaurant_id
		WHERE e.kind NOT IN ('reboot', 'kiosk_exit_offline', 'offline_dropped') AND NOT dv.hidden
		  AND e.occurred_at > now() - make_interval(days => $1)`+scope+`
		GROUP BY e.kind, e.summary
		ORDER BY MAX(e.occurred_at) DESC
		LIMIT $2 OFFSET $3`, args...)
	if err != nil {
		return nil, 0, err
	}
	defer rows.Close()
	var out []CrashEvent
	total := 0
	for rows.Next() {
		var c CrashEvent
		if err := rows.Scan(&c.Kind, &c.Summary, &c.EventCount, &c.DeviceCount, &c.OccurredAt,
			&c.Serial, &c.Restaurant, &c.BuildID, &c.Detail, &total); err != nil {
			return nil, 0, err
		}
		out = append(out, c)
	}
	return out, total, rows.Err()
}

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
		WHERE e.kind NOT IN ('reboot', 'kiosk_exit_offline', 'offline_dropped') AND NOT dv.hidden
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
		WHERE e.device_id = $1 AND e.kind NOT IN ('reboot', 'kiosk_exit_offline', 'offline_dropped')
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
			WHERE e.build_id = $1 AND e.kind NOT IN ('reboot', 'kiosk_exit_offline', 'offline_dropped')
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
		WHERE e.kind NOT IN ('reboot', 'kiosk_exit_offline', 'offline_dropped') AND NOT d.hidden
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
		WHERE e.kind NOT IN ('reboot', 'kiosk_exit_offline', 'offline_dropped') AND NOT d.hidden
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
			WHERE e.kind NOT IN ('reboot', 'kiosk_exit_offline', 'offline_dropped') AND NOT d.hidden AND e.occurred_at >= CURRENT_DATE - 6
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
