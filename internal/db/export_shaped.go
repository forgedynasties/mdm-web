package db

import (
	"context"
	"encoding/json"
	"strconv"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// exportStateKeys are the state-event keys the CSV export can print. Kept narrow on
// purpose: loading every key a device has ever changed would pull in fields no column
// asks for.
var exportStateKeys = []string{"charging", "wlc_status", "wifi", "ip_address", "timezone", "location"}

// StreamExportShaped answers the CSV export from the shaped tables instead of from
// checkins.extra. It produces exactly the same ExportRow stream as
// StreamExportCheckins (or StreamExportCycles when cycles is true) for the same
// window, and it is the last reader that had to move before checkins.extra can stop
// being written.
//
// It works one device at a time. That is not laziness about a single ordered query:
// the state half of a row is carried forward from the last transition, which is a
// per-device walk, and the existing export is already ordered by serial, so a device
// at a time produces the same order with the state stream held in memory (transitions
// are few — nothing is written while a state holds) and the samples still streamed.
//
// The row's `extra` is rebuilt here, as the handful of keys the CSV actually reads,
// rather than the export's formatting being reimplemented against typed columns. That
// is deliberate. Every formatting rule the CSV has — an SSID's Android quotes stripped,
// a boolean printed as 0/1, a temperature at one decimal — then stays in exactly one
// place and cannot drift between the two sources, which is the whole failure this
// migration is trying not to cause. The object is seven small keys against the
// twenty-three-key, TOASTed snapshot it replaces, on a path a human triggers by hand.
func (d *DB) StreamExportShaped(ctx context.Context, deviceIDs []uuid.UUID, start, end time.Time, intervalSec int, cycles bool, fn func(ExportRow) error) error {
	devices, err := d.exportDevices(ctx, deviceIDs)
	if err != nil {
		return err
	}
	for _, dev := range devices {
		if err := d.streamShapedDevice(ctx, dev, start, end, intervalSec, cycles, fn); err != nil {
			return err
		}
	}
	return nil
}

type exportDevice struct {
	ID             uuid.UUID
	Serial         string
	LastSeenAt     time.Time
	PollIntervalMs int
}

// exportDevices resolves the selected devices in serial order, which is the order the
// rows must come out in.
func (d *DB) exportDevices(ctx context.Context, ids []uuid.UUID) ([]exportDevice, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT id, serial_number, last_seen_at, COALESCE(poll_interval_ms, 30000)
		FROM devices WHERE id = ANY($1) ORDER BY serial_number`, ids)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []exportDevice
	for rows.Next() {
		var e exportDevice
		if err := rows.Scan(&e.ID, &e.Serial, &e.LastSeenAt, &e.PollIntervalMs); err != nil {
			return nil, err
		}
		out = append(out, e)
	}
	return out, rows.Err()
}

// buildAt is one entry of a device's build history, in the same shape as a state
// transition so it can be carried forward the same way.
type buildAt struct {
	At    time.Time
	Build string
}

// getBuildTimeline returns the builds this device ran during the window, plus the one
// it was running when the window opened. Builds live in device_build_history rather
// than among the state events: they were already recorded there before any of this,
// and a build change is written from the check-in's build_id column, not from extra.
func (d *DB) getBuildTimeline(ctx context.Context, deviceID uuid.UUID, from, until time.Time) ([]buildAt, error) {
	rows, err := d.pool.Query(ctx, `
		(
			SELECT at, to_build FROM device_build_history
			WHERE device_id = $1 AND at < $2 ORDER BY at DESC LIMIT 1
		)
		UNION ALL
		(
			SELECT at, to_build FROM device_build_history
			WHERE device_id = $1 AND at >= $2 AND at <= $3 ORDER BY at
		)
		ORDER BY at`, deviceID, from, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []buildAt
	for rows.Next() {
		var b buildAt
		if err := rows.Scan(&b.At, &b.Build); err != nil {
			return nil, err
		}
		out = append(out, b)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// A device that has never changed build has no history row at all, so nothing above
	// establishes what it is running. Fall back to the build recorded on the device.
	if len(out) == 0 {
		var cur string
		if err := d.pool.QueryRow(ctx, `SELECT build_id FROM devices WHERE id = $1`, deviceID).Scan(&cur); err != nil && err != pgx.ErrNoRows {
			return nil, err
		}
		if cur != "" {
			out = append(out, buildAt{At: time.Time{}, Build: cur})
		}
	}
	return out, nil
}

func (d *DB) streamShapedDevice(ctx context.Context, dev exportDevice, start, end time.Time, intervalSec int, cycles bool, fn func(ExportRow) error) error {
	states, err := d.GetStateTimeline(ctx, dev.ID, exportStateKeys, start, end)
	if err != nil {
		return err
	}
	builds, err := d.getBuildTimeline(ctx, dev.ID, start, end)
	if err != nil {
		return err
	}

	cur := map[string]string{}
	si, bi := 0, 0
	curBuild := ""
	// advance carries every timeline forward to the given instant. Both streams and the
	// samples are ordered by time, so this is one pass over each rather than a lookup
	// per row.
	advance := func(at time.Time) {
		for si < len(states) && !states[si].At.After(at) {
			cur[states[si].Key] = states[si].Value
			si++
		}
		for bi < len(builds) && !builds[bi].At.After(at) {
			curBuild = builds[bi].Build
			bi++
		}
	}

	emit := func(mark time.Time, s *DeviceSample) error {
		row := ExportRow{
			SerialNumber: dev.Serial,
			Timestamp:    mark,
			LastSeenAt:   dev.LastSeenAt,
		}
		if s == nil {
			row.Empty = true
			row.Extra = json.RawMessage("{}")
			return fn(row)
		}
		advance(s.At)
		row.SampleAt = s.At
		row.BuildID = curBuild
		if s.BatteryPct != nil {
			row.BatteryPct = int(*s.BatteryPct)
		}
		row.Extra = ShapedExtra(*s, cur)
		return fn(row)
	}

	if cycles {
		return d.streamShapedCycles(ctx, dev, start, end, intervalSec, emit)
	}
	return d.streamShapedSamples(ctx, dev, start, end, intervalSec, emit)
}

// streamShapedSamples is the raw and interval-sampled modes: one row per sample, or
// the first sample of each interval bucket. Mirrors exportCheckinsQuery.
func (d *DB) streamShapedSamples(ctx context.Context, dev exportDevice, start, end time.Time, intervalSec int, emit func(time.Time, *DeviceSample) error) error {
	const cols = `at, battery_pct, temp_c, wifi_rssi, ram_used_mb, ram_total_mb, storage_free_gb, uptime_s`
	var q string
	var args []any
	if intervalSec > 0 {
		q = `
			SELECT ` + cols + ` FROM (
				SELECT ` + cols + `, ROW_NUMBER() OVER (
					PARTITION BY floor(EXTRACT(EPOCH FROM (at - $2)) / $4) ORDER BY at
				) AS rn
				FROM device_samples
				WHERE device_id = $1 AND at >= $2 AND at <= $3
			) s WHERE rn = 1 ORDER BY at`
		args = []any{dev.ID, start, end, intervalSec}
	} else {
		q = `SELECT ` + cols + ` FROM device_samples
		     WHERE device_id = $1 AND at >= $2 AND at <= $3 ORDER BY at`
		args = []any{dev.ID, start, end}
	}
	rows, err := d.pool.Query(ctx, q, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var s DeviceSample
		if err := rows.Scan(&s.At, &s.BatteryPct, &s.TempC, &s.WifiRSSI, &s.RAMUsedMB, &s.RAMTotalMB, &s.StorageFreeGB, &s.UptimeS); err != nil {
			return err
		}
		if err := emit(s.At, &s); err != nil {
			return err
		}
	}
	return rows.Err()
}

// streamShapedCycles is the grid mode: a row at every interval mark, carrying the last
// sample at or before it, subject to the same per-device staleness cap as
// StreamExportCycles so a device that goes dark shows a gap rather than a value frozen
// forever.
func (d *DB) streamShapedCycles(ctx context.Context, dev exportDevice, start, end time.Time, intervalSec int, emit func(time.Time, *DeviceSample) error) error {
	rows, err := d.pool.Query(ctx, `
		SELECT g.ts, s.at, s.battery_pct, s.temp_c, s.wifi_rssi, s.ram_used_mb, s.ram_total_mb, s.storage_free_gb, s.uptime_s
		FROM generate_series($2::timestamptz, $3::timestamptz, make_interval(secs => $4)) AS g(ts)
		LEFT JOIN LATERAL (
			SELECT at, battery_pct, temp_c, wifi_rssi, ram_used_mb, ram_total_mb, storage_free_gb, uptime_s
			FROM device_samples
			WHERE device_id = $1 AND at <= g.ts
			  AND at > g.ts - GREATEST(
			        make_interval(secs => $4),
			        make_interval(secs => $5 / 1000.0 * 10),
			        INTERVAL '15 minutes')
			ORDER BY at DESC LIMIT 1
		) s ON true
		ORDER BY g.ts`, dev.ID, start, end, intervalSec, dev.PollIntervalMs)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var mark time.Time
		var at *time.Time
		var s DeviceSample
		if err := rows.Scan(&mark, &at, &s.BatteryPct, &s.TempC, &s.WifiRSSI, &s.RAMUsedMB, &s.RAMTotalMB, &s.StorageFreeGB, &s.UptimeS); err != nil {
			return err
		}
		if at == nil {
			if err := emit(mark, nil); err != nil {
				return err
			}
			continue
		}
		s.At = *at
		if err := emit(mark, &s); err != nil {
			return err
		}
	}
	return rows.Err()
}

// ShapedExtra rebuilds the check-in snapshot's keys that the CSV reads, from the
// sample's columns and the states in force at that instant. A key the device never
// reported is left out entirely rather than written as null, so the export's "is this
// key present" test answers the same as it does against a real snapshot.
func ShapedExtra(s DeviceSample, state map[string]string) json.RawMessage {
	m := map[string]json.RawMessage{}
	put := func(k, v string) {
		if v != "" {
			m[k] = json.RawMessage(v)
		}
	}
	if s.TempC != nil {
		put("battery_temp_c", strconv.FormatFloat(*s.TempC, 'g', -1, 64))
	}
	if s.StorageFreeGB != nil {
		put("storage_free_gb", strconv.FormatFloat(*s.StorageFreeGB, 'g', -1, 64))
	}
	if s.UptimeS != nil {
		put("uptime_seconds", strconv.FormatInt(int64(*s.UptimeS), 10))
	}
	if s.WifiRSSI != nil {
		put("wifi_rssi", strconv.FormatInt(int64(*s.WifiRSSI), 10))
	}
	if s.RAMUsedMB != nil && s.RAMTotalMB != nil {
		put("ram_usage_mb", `{"used":`+strconv.FormatInt(int64(*s.RAMUsedMB), 10)+
			`,"total":`+strconv.FormatInt(int64(*s.RAMTotalMB), 10)+`}`)
	}
	// State values are stored as the plain text jsonScalar produced, so a string has to
	// be quoted back into JSON and a number or boolean must not be.
	for _, k := range []string{"wifi", "ip_address", "timezone"} {
		if v, ok := state[k]; ok && v != "" && v != "null" {
			b, err := json.Marshal(v)
			if err == nil {
				put(k, string(b))
			}
		}
	}
	if v, ok := state["charging"]; ok && (v == "true" || v == "false") {
		put("charging", v)
	}
	if v, ok := state["wlc_status"]; ok {
		if _, isNum := atoiExport(v); isNum {
			put("wlc_status", v)
		}
	}
	if v, ok := state["location"]; ok {
		if lat, lon, ok := parseLocationValue(v); ok {
			put("latitude", strconv.FormatFloat(lat, 'f', -1, 64))
			put("longitude", strconv.FormatFloat(lon, 'f', -1, 64))
		}
	}
	b, err := json.Marshal(m)
	if err != nil {
		return json.RawMessage("{}")
	}
	return b
}

// atoiExport reports whether a stored state value is a plain integer, so a value that
// is not one is left out rather than emitted as invalid JSON.
func atoiExport(s string) (int, bool) {
	n, err := strconv.Atoi(s)
	return n, err == nil
}
