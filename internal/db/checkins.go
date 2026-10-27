package db

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"

	prod "mdm/internal/product"
)

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
//
// The history this report produces — state transitions and, when it stored, a numeric
// sample — comes back as shaped rather than being written here: nothing in the reply
// depends on it, so the caller queues it (WriteShapedBatch) and this transaction only
// holds the device row.
func (d *DB) UpsertCheckin(ctx context.Context, serial, buildID string, batteryPct *int, extra json.RawMessage, mergeExtra bool, product string) (deviceID uuid.UUID, pollIntervalMs int, isNew bool, arrival bool, shaped ShapedTelemetry, err error) {
	if len(extra) == 0 {
		extra = json.RawMessage("{}")
	}
	// Normalize once here so every check-in path (HTTP keyframe, WS delta) stores the
	// same canonical key. Empty is left as-is: a delta frame that omits product must
	// not wipe a product already learned from an earlier keyframe (see COALESCE below).
	product = prod.Normalize(product)

	// Our own hardware carries its category in the product key, so the catalog answers
	// it. A stock device under the DPC agent reports only a model, so guess a category
	// from it — the upsert below uses this only when no class is stored yet, so it is a
	// first guess an admin can correct, never an override. A delta frame that omits the
	// model guesses nothing and leaves the stored value alone.
	var guessedClass string
	if len(extra) > 0 {
		var ident struct {
			AgentType    string `json:"agent_type"`
			Model        string `json:"model"`
			Manufacturer string `json:"manufacturer"`
		}
		if err := json.Unmarshal(extra, &ident); err == nil && prod.IsStockAgent(ident.AgentType) {
			guessedClass = prod.ClassForModel(product, ident.Manufacturer, ident.Model)
		}
	}
	// The class a model is attached to on the Hardware page is the device's class, always:
	// it replaces whatever the device had, where the guess above only fills an empty one.
	var modelClass string
	if product != "" {
		modelClass, _ = d.ProductRole(ctx, product)
	}
	insertClass := guessedClass
	if modelClass != "" {
		insertClass = modelClass
	}

	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return uuid.Nil, 0, false, false, ShapedTelemetry{}, err
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
			return uuid.Nil, 0, false, false, ShapedTelemetry{}, err
		}
		// A new build can carry a different app set — a re-flash wipes the lot. The
		// client only re-sends its app list when ITS OWN hash changes, so a wiped device
		// would keep showing the old apps forever. Clearing our hash marks the inventory
		// unknown; the check-in response then asks for a full list (see NeedsAppInventory).
		if _, err = tx.Exec(ctx, `
			UPDATE devices SET packages_hash = ''
			WHERE serial_number = $1 AND build_id <> '' AND build_id <> $2
		`, serial, buildID); err != nil {
			return uuid.Nil, 0, false, false, ShapedTelemetry{}, err
		}
	}

	// The snapshot as it stands before this report, so a transition can be told from a
	// value simply being reported again. One indexed read on a small table; no row yet
	// for a device enrolling now, which reads as "everything it sends is new".
	var prevExtra json.RawMessage
	var prevSeen *time.Time
	var prevCustody string
	if err := tx.QueryRow(ctx, `SELECT latest_extra, last_seen_at, custody_server FROM devices WHERE serial_number = $1`,
		serial).Scan(&prevExtra, &prevSeen, &prevCustody); err != nil && err != pgx.ErrNoRows {
		return uuid.Nil, 0, false, false, ShapedTelemetry{}, err
	}
	// An arrival is worth telling our peers about: a device we have never seen, one we
	// believed was on another server, or one silent long enough that another server may
	// well have had it. An ordinary check-in is none of those, so announcements stay rare.
	arrival = prevSeen == nil || prevCustody != "" || time.Since(*prevSeen) >= CustodyArrivalGap

	extraExpr := "EXCLUDED.latest_extra"
	if mergeExtra {
		extraExpr = "COALESCE(devices.latest_extra, '{}'::jsonb) || EXCLUDED.latest_extra"
	}
	var merged json.RawMessage
	var battery int
	var at time.Time
	err = tx.QueryRow(ctx, fmt.Sprintf(`
		INSERT INTO devices (serial_number, build_id, last_seen_at, latest_battery_pct, latest_extra, product,
		                     device_class, agent_kind, capabilities, capabilities_degraded)
		VALUES ($1, $2, NOW(), COALESCE($3, 0), $4, $5, $6,
		        CASE WHEN $4::jsonb->>'agent_type' IN ('dpc', 'mdm-lite') THEN 'dpc' ELSE 'firmware' END,
		        CASE WHEN jsonb_typeof($4::jsonb->'capabilities') = 'array' THEN $4::jsonb->'capabilities' ELSE '[]'::jsonb END,
		        CASE WHEN jsonb_typeof($4::jsonb->'capabilities_degraded') = 'array' THEN $4::jsonb->'capabilities_degraded' ELSE '[]'::jsonb END)
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
			    -- The class follows the model: the class its model is attached to on the
			    -- Hardware page ($7) when it has one, else a guess from the model it reports
			    -- that only ever fills an EMPTY class.
			    device_class       = CASE WHEN $7 <> '' THEN $7
			                              WHEN devices.device_class = '' THEN EXCLUDED.device_class
			                              ELSE devices.device_class END,
			    -- Agent facts persist from the frame that carries them: a DPC agent
			    -- announces itself once and stays DPC; a frame without the keys
			    -- (delta / legacy client) leaves what was learned.
			    agent_kind         = CASE WHEN $4::jsonb->>'agent_type' IN ('dpc', 'mdm-lite') THEN 'dpc' ELSE devices.agent_kind END,
			    capabilities       = CASE WHEN jsonb_typeof($4::jsonb->'capabilities') = 'array'
			                              THEN $4::jsonb->'capabilities' ELSE devices.capabilities END,
			    capabilities_degraded = CASE WHEN jsonb_typeof($4::jsonb->'capabilities_degraded') = 'array'
			                              THEN $4::jsonb->'capabilities_degraded' ELSE devices.capabilities_degraded END,
			    -- A retired device that talks again is back in the fleet (its status,
			    -- not its inbox state: it keeps the site/group it had).
			    enrollment_status  = CASE WHEN devices.enrollment_status IN ('retired', 'wiped', 'unenrolled')
			                              THEN CASE WHEN devices.enrolled_via IS NULL THEN 'auto' ELSE 'enrolled' END
			                              ELSE devices.enrollment_status END,
			    latest_extra       = %s,
			    -- Hearing from a device settles where it is: here. Custody clears on the
			    -- same statement that records the check-in, so there is no window where a
			    -- device is both talking to us and filed as living somewhere else.
			    custody_server     = '',
			    custody_url        = '',
			    custody_seen_at    = NULL,
			    custody_source     = '',
			    custody_set_at     = NULL
		RETURNING id, poll_interval_ms, (xmax = 0) AS is_new, latest_battery_pct, latest_extra, NOW()
	`, extraExpr), serial, buildID, batteryPct, extra, product, insertClass, modelClass).Scan(&deviceID, &pollIntervalMs, &isNew, &battery, &merged, &at)
	if err != nil {
		return uuid.Nil, 0, false, false, ShapedTelemetry{}, err
	}
	// A Standard-client device of a mains-only class has no battery, but Android still
	// broadcasts one: the HK1 TV boxes send a frozen 56-81 °C, the Rockchip kiosks and the
	// Sunmi KDS 2.6 °C, the Sunmi POS 37 °C. Shown as the device's temperature it hid
	// the real SoC reading (cpu_temp_c), so it is not kept. Firmware devices keep theirs.
	if err = tx.QueryRow(ctx, `
		UPDATE devices SET latest_extra = latest_extra - 'battery_temp_c'
		WHERE id = $1 AND agent_kind = 'dpc' AND device_class = ANY($2) AND latest_extra ? 'battery_temp_c'
		RETURNING latest_extra
	`, deviceID, prod.MainsOnlyClassList()).Scan(&merged); err != nil && !errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, 0, false, false, ShapedTelemetry{}, err
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
	// The snapshot itself is no longer stored: every reader now takes its values from
	// device_samples and device_state_events, and the column costs about 600 of the
	// row's 665 bytes. What the row still has to carry is the ONE thing the snapshot was
	// being read for here — whether anything non-volatile has changed since the last
	// stored row — so that is kept as a hash of exactly the projection the comparison
	// used to make.
	//
	// A hash rather than comparing against devices.latest_extra, which is also to hand:
	// that would compare against the last REPORT instead of the last STORED row. The two
	// agree by an induction (a coalesced report was non-volatile-equal to the row before
	// it, so the projections match) and the induction is sound, but the sample window
	// would still be measured against one row and the equality against another. Getting
	// this wrong does not break anything visibly — it stores a row per report, silently
	// undoing the coalescing that cut the fleet from 118,869 rows a day to 52,488.
	//
	// jsonb normalises key order, so equal values always render to identical text and
	// the hash is stable. Rows written before this have a NULL hash and so compare
	// unequal, which stores one extra row per device once, and never again.
	// A flapping charger is one state in history, not a row per toggle: the hash and the
	// events compare this projection, while latest_extra above keeps the real report.
	histPrev, histCur := d.flaps.project(deviceID, prevExtra, merged, time.Now())

	sample := int(d.checkinSampleSec.Load())
	//
	// The last stored report's fingerprint lives on the device row (hist_*), not in a
	// checkins row: that table is being retired, and every reader now takes history from
	// device_samples and device_state_events. The devices row is already locked by the
	// upsert above, so reading and moving the fingerprint here is race-free. A device with
	// no fingerprint yet (new, or the first report after this moved) simply stores.
	ct, err := tx.Exec(ctx, `
		UPDATE devices SET
			hist_at         = NOW(),
			hist_battery    = $2,
			hist_build      = $3,
			hist_state_hash = md5(((($4::jsonb) - `+checkinStripKeys+`) - `+checkinVolatileKeys+`)::text)
		WHERE id = $1
		  AND NOT (
			$5 > 0
			AND hist_at IS NOT NULL
			AND hist_at > NOW() - make_interval(secs => $5)
			AND hist_battery = $2
			AND hist_build = $3
			AND hist_state_hash
			  = md5(((($4::jsonb) - `+checkinStripKeys+`) - `+checkinVolatileKeys+`)::text)
		  )
	`, deviceID, battery, buildID, histCur, sample)
	if err != nil {
		return uuid.Nil, 0, false, false, ShapedTelemetry{}, err
	}
	stored := ct.RowsAffected() > 0

	// The shaped rows to write: the state events always, the numeric sample when this
	// report stored (the same condition that used to store a checkins row). Stamped with
	// this transaction's NOW(), so a queued write lands at the instant it described, the
	// same instant hist_at records, however long it waited.
	shaped = ShapedTelemetry{DeviceID: deviceID, At: at, Prev: histPrev, Cur: histCur, Battery: battery, Stored: stored}
	// A charge-climb summary (client 1.10.1) is kept as history in battery_climbs; the key
	// itself is stripped from check-in rows. Keyframes repeat it, which the key dedupes.
	if err := insertBatteryClimb(ctx, tx, deviceID, extra); err != nil {
		return uuid.Nil, 0, false, false, ShapedTelemetry{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return uuid.Nil, 0, false, false, ShapedTelemetry{}, err
	}
	return deviceID, pollIntervalMs, isNew, arrival, shaped, nil
}

// stateKeys are the fields stored as transitions: they describe a state that holds,
// so what matters is when it changed, not its value on every report. Measured over 600
// consecutive rows of the busiest device, every one of these changed 0% of the time
// while costing bytes in each row.
var stateKeys = []string{
	"charging", "charger_type", "wlc_status", "wlc_charging",
	"screen_on", "kiosk_suspended", "boot_id", "boot_reason",
	"ip_address", "wifi", "timezone", "foreground_pkg",
	"adb_enabled", "screen_lock_set", "storage_encrypted",
	"host_app_fg",
}

// withDerivedState adds the state keys that are not report fields of their own but a
// reading of one. host_app_fg is whether the host app's variant in use is in the
// foreground (extra.host_app, firmware client 1.11.0+): host_app itself is a large,
// stripped key, so its one state bit is recorded as a transition here, which is what
// lets the host_app_away alert measure "away for N minutes" the way wlc_continuous
// measures a pad state. Absent when the report has no host app with variants.
func withDerivedState(m map[string]json.RawMessage) {
	raw, ok := m["host_app"]
	if !ok {
		return
	}
	var ha struct {
		Package  string `json:"package"`
		Variants []struct {
			Package    string `json:"package"`
			Foreground bool   `json:"foreground"`
		} `json:"variants"`
	}
	if json.Unmarshal(raw, &ha) != nil || ha.Package == "" {
		return
	}
	for _, v := range ha.Variants {
		if v.Package == ha.Package {
			if v.Foreground {
				m["host_app_fg"] = json.RawMessage("true")
			} else {
				m["host_app_fg"] = json.RawMessage("false")
			}
			return
		}
	}
}

// ShapedTelemetry is the history one check-in produces, computed while the device row
// was locked (so Prev and Cur are the right pair) and written later by WriteShapedBatch.
type ShapedTelemetry struct {
	DeviceID uuid.UUID
	At       time.Time       // the check-in transaction's NOW()
	Prev     json.RawMessage // projection before this report
	Cur      json.RawMessage // projection after it
	Battery  int
	Stored   bool // this report stored a sample; false = coalesced, events only
}

// shapedBatch is a batch of reports flattened into the column arrays one INSERT each
// takes, so a hundred reports cost two statements rather than two hundred.
type shapedBatch struct {
	evDev                    []uuid.UUID
	evAt                     []time.Time
	evKey, evFrom, evTo      []string
	evSeed                   []bool
	smDev                    []uuid.UUID
	smAt                     []time.Time
	smBattery                []int16
	smTemp, smStorage, smCPU []*float64
	smRSSI                   []*int16
	smRAMUsed, smRAMTotal    []*int32
}

// shapeRows flattens reports in order. Transitions: a key absent from the current
// report is not a change — a delta frame simply did not mention it — so only keys
// present now are considered.
//
// A key whose value has NOT changed still needs one event if it has never been
// recorded: without it the stream has no baseline and a reader cannot say what the
// state was, only when it last flipped. That is not hypothetical — comparing against
// devices.latest_extra, which already held every value when dual writing began, meant
// no device that existed then ever got a first event. On the fleet that left timezone
// with 0 events against 150 devices reporting it, wifi with 2 against 147, and
// charging with 44 against 128: only keys that happened to flip afterwards existed at
// all. Such keys are marked as seeds and inserted only where the device has no event
// for them yet, so this costs one indexed probe per key and writes once per device.
//
// One statement cannot see its own rows, so the "no event yet" probe would pass for
// every report of the same device in the batch. Written one at a time, the first
// report's row made the later seeds redundant; here that is done before the insert —
// any seed for a (device, key) an earlier report in the batch already wrote is dropped.
func shapeRows(items []ShapedTelemetry) (shapedBatch, error) {
	var b shapedBatch
	type devKey struct {
		dev uuid.UUID
		key string
	}
	written := map[devKey]bool{}
	for _, it := range items {
		var prevMap, curMap map[string]json.RawMessage
		if len(it.Prev) > 0 {
			_ = json.Unmarshal(it.Prev, &prevMap)
		}
		if err := json.Unmarshal(it.Cur, &curMap); err != nil {
			return b, err
		}
		if prevMap != nil {
			withDerivedState(prevMap)
		}
		withDerivedState(curMap)
		keys, froms, tos, seeds := stateEventRows(prevMap, curMap)
		for i, k := range keys {
			dk := devKey{it.DeviceID, k}
			if seeds[i] && written[dk] {
				continue
			}
			written[dk] = true
			b.evDev = append(b.evDev, it.DeviceID)
			b.evAt = append(b.evAt, it.At)
			b.evKey = append(b.evKey, k)
			b.evFrom = append(b.evFrom, froms[i])
			b.evTo = append(b.evTo, tos[i])
			b.evSeed = append(b.evSeed, seeds[i])
		}
		if !it.Stored {
			continue // the history row was coalesced away; its sample would be too
		}
		used, total := ramUsedTotal(curMap["ram_usage_mb"])
		b.smDev = append(b.smDev, it.DeviceID)
		b.smAt = append(b.smAt, it.At)
		b.smBattery = append(b.smBattery, int16(it.Battery))
		b.smTemp = append(b.smTemp, jsonFloat(curMap["battery_temp_c"]))
		b.smRSSI = append(b.smRSSI, scaledInt(curMap["wifi_rssi"], 1))
		b.smRAMUsed = append(b.smRAMUsed, used)
		b.smRAMTotal = append(b.smRAMTotal, total)
		b.smStorage = append(b.smStorage, jsonFloat(curMap["storage_free_gb"]))
		b.smCPU = append(b.smCPU, jsonFloat(curMap["cpu_temp_c"]))
	}
	return b, nil
}

// WriteShapedBatch records the state transitions and numeric samples of a batch of
// check-ins in one transaction, so a sample and the events around it cannot disagree.
// Items for one device must be in the order they were reported.
func (d *DB) WriteShapedBatch(ctx context.Context, items []ShapedTelemetry) error {
	if len(items) == 0 {
		return nil
	}
	b, err := shapeRows(items)
	if err != nil {
		return err
	}
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)
	if len(b.evKey) > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO device_state_events (device_id, at, key, from_val, to_val)
			SELECT u.d, u.a, u.k, u.f, u.t
			FROM unnest($1::uuid[], $2::timestamptz[], $3::text[], $4::text[], $5::text[], $6::bool[])
			     AS u(d, a, k, f, t, seed)
			WHERE NOT u.seed
			   OR NOT EXISTS (SELECT 1 FROM device_state_events e
			                  WHERE e.device_id = u.d AND e.key = u.k)
			ON CONFLICT DO NOTHING`, b.evDev, b.evAt, b.evKey, b.evFrom, b.evTo, b.evSeed); err != nil {
			return err
		}
	}
	if len(b.smDev) > 0 {
		if _, err := tx.Exec(ctx, `
			INSERT INTO device_samples (device_id, at, battery_pct, temp_c, wifi_rssi, ram_used_mb, ram_total_mb, storage_free_gb, cpu_temp_c)
			SELECT * FROM unnest($1::uuid[], $2::timestamptz[], $3::int2[], $4::float8[], $5::int2[], $6::int4[], $7::int4[], $8::float8[], $9::float8[])
			ON CONFLICT (device_id, at) DO NOTHING`,
			b.smDev, b.smAt, b.smBattery, b.smTemp, b.smRSSI, b.smRAMUsed, b.smRAMTotal, b.smStorage, b.smCPU); err != nil {
			return err
		}
	}
	return tx.Commit(ctx)
}

// stateEventRows decides which state events a report produces. Split out from the
// write so the decision can be tested directly: it is where the missing-baseline bug
// lived, and a bug here is invisible until a reader asks what a state was.
//
// A key returns a seed flag rather than being filtered out, so the caller can insert
// changes unconditionally and seeds only where nothing has been recorded yet.
func stateEventRows(prevMap, curMap map[string]json.RawMessage) (keys, froms, tos []string, seeds []bool) {
	for _, k := range stateKeys {
		c, ok := curMap[k]
		if !ok {
			continue
		}
		p, had := prevMap[k]
		unchanged := had && string(p) == string(c)
		keys = append(keys, k)
		// A seed records the value as it stands, with no claim about what preceded it.
		if unchanged {
			froms = append(froms, "")
		} else {
			froms = append(froms, jsonScalar(p))
		}
		tos = append(tos, jsonScalar(c))
		seeds = append(seeds, unchanged)
	}
	return keys, froms, tos, seeds
}

// jsonScalar renders a jsonb value as the plain text an event row stores: "true",
// "1", "wlan0" — without the quotes a JSON string would carry, so a reader never has
// to know which kind it was.
func jsonScalar(raw json.RawMessage) string {
	if len(raw) == 0 {
		return "" // the key was not in this report at all
	}
	// Explicit null is a reported value and stays distinguishable from absent:
	// unmarshalling it into a string succeeds and leaves "" behind.
	if string(raw) == "null" {
		return "null"
	}
	var s string
	if err := json.Unmarshal(raw, &s); err == nil {
		return s
	}
	return string(raw)
}

// scaledInt turns a JSON number into a scaled smallint (temperature ×10, and so on),
// or nil when the field is absent or out of a smallint's range — a stored NULL says
// "not measured", which is not the same as zero.
func scaledInt(raw json.RawMessage, scale float64) *int16 {
	// Explicit null must stay null: json.Unmarshal accepts it into a float and leaves
	// zero behind, which would store "not measured" as a real reading of 0.
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil
	}
	v := f * scale
	if v > 32767 || v < -32768 {
		return nil
	}
	out := int16(v)
	return &out
}

// ramUsedTotal pulls the two numbers a reader needs out of the reported object. Kept
// as used and total rather than a percentage so the chart and the CSV export — which
// prints ram_used_mb and ram_total_mb — can never quote different figures for the same
// reading.
func ramUsedTotal(raw json.RawMessage) (*int32, *int32) {
	if len(raw) == 0 {
		return nil, nil
	}
	var m struct {
		Used  *float64 `json:"used"`
		Total *float64 `json:"total"`
	}
	if err := json.Unmarshal(raw, &m); err != nil || m.Used == nil || m.Total == nil {
		return nil, nil
	}
	u, t := int32(*m.Used), int32(*m.Total)
	return &u, &t
}

// jsonFloat returns a JSON number unchanged, so the value stored is the value sent and
// no reader can quote a different figure from the CSV, which prints the same field out
// of checkins. Explicit null stays null: unmarshalling it into a float succeeds and
// leaves zero behind, which would store "not measured" as a reading.
func jsonFloat(raw json.RawMessage) *float64 {
	if len(raw) == 0 || string(raw) == "null" {
		return nil
	}
	var f float64
	if err := json.Unmarshal(raw, &f); err != nil {
		return nil
	}
	return &f
}

// checkinStripKeys are jsonb keys never persisted in checkins.extra (kept only in
// devices.latest_extra): bulky, re-sent every frame, and already stored elsewhere
// (device_events / the learned Wi-Fi index).
//
// charger_voltage_mv, mic_gain and location_address were added after measuring a week
// of real rows: nothing on the server reads any of them out of history — the mic chip,
// the map pin and the device page all read devices.latest_extra — yet together they
// cost ~180 bytes on every stored row. charger_voltage_mv was the expensive one for a
// second reason: it is absent from checkinVolatileKeys, so its last-digit jitter read
// as a state change and forced a new history row on roughly 88% of samples. Stripping
// a key removes it from the duplicate comparison below as well, so that stops too.
//
// battery_temp_c and ram_usage_mb deliberately stay: they feed the device chart, the
// live SSE payload, a CSV column, the temp_elevated/memory_low rules and the daily
// rollup. They are quantised on the device instead.
//
// wifi_networks (client 1.12.0, DPC 0.3.0) lists the configured networks with a
// `connected` flag that flips on every Wi-Fi drop, so it would read as a state change on
// each one; nothing reads it from history.
//
// ble_sightings / ble_scan / lost_beacon (18 Oct, lost-device reporting) are what a scan
// heard and how the radio fared: they go to their own tables, and a listener reports them
// every scan while something is heard.
const checkinStripKeys = `'crash_events' - 'wifi_scan' - 'charger_voltage_mv' - 'mic_gain' - 'location_address' - 'battery_cc' - 'host_app' - 'wifi_networks' - 'ble_sightings' - 'ble_scan' - 'lost_beacon'`

// checkinVolatileKeys are readings that drift every frame without meaning a state
// change; two rows equal on everything else within the sample window are one sample.
const checkinVolatileKeys = `ARRAY['uptime_seconds','wifi_rssi','ram_usage_mb','battery_temp_c','storage_free_gb','ota_progress','wifi_disconnects_1h','location_accuracy','charger_flaps_5m']`

// SetCheckinSampleSec sets the coalescing window used by UpsertCheckin (0 = off).
func (d *DB) SetCheckinSampleSec(sec int) { d.checkinSampleSec.Store(int32(sec)) }


// DeviceSample is one row of the numeric series. Pointers where the device may not
// report the field at all: nil means "not measured", which no reader should draw as a
// zero.
type DeviceSample struct {
	At            time.Time
	BatteryPct    *int16
	TempC         *float64
	WifiRSSI      *int16
	RAMUsedMB     *int32
	RAMTotalMB    *int32
	StorageFreeGB *float64
	CPUTempC      *float64 // SoC reading from battery-less TV boxes; see the schema note
	Late          bool     // kept on the device while offline and sent later
}

// StateAt is a state key's value from a point in time until the next event for that key.
type StateAt struct {
	At    time.Time
	Key   string
	Value string
}

// ShapedCoverage reports whether this device has history in the shaped tables, and
// from when (always "from the beginning": ok means covered). They hold every device's
// whole life — samples were backfilled to its first check-in and state history derived
// from the old snapshots before the checkins table was retired — so the only question
// left is whether the device has ever stored a sample at all.
func (d *DB) ShapedCoverage(ctx context.Context, deviceID uuid.UUID) (from time.Time, ok bool, err error) {
	err = d.pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM device_samples WHERE device_id = $1)`, deviceID).Scan(&ok)
	return time.Time{}, ok, err
}

// GetDeviceSamples returns the numeric series for a window, oldest first — the order
// every chart wants, and the order the (device_id, at) primary key already stores.
func (d *DB) GetDeviceSamples(ctx context.Context, deviceID uuid.UUID, from, until time.Time) ([]DeviceSample, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT at, battery_pct, temp_c, wifi_rssi, ram_used_mb, ram_total_mb, storage_free_gb, cpu_temp_c, late
		FROM device_samples
		WHERE device_id = $1 AND at >= $2 AND at <= $3
		ORDER BY at`, deviceID, from, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []DeviceSample
	for rows.Next() {
		var s DeviceSample
		if err := rows.Scan(&s.At, &s.BatteryPct, &s.TempC, &s.WifiRSSI, &s.RAMUsedMB, &s.RAMTotalMB, &s.StorageFreeGB, &s.CPUTempC, &s.Late); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// cycleFull / cycleEmpty mirror CYCLE_FULL / CYCLE_EMPTY in templates/device.html. They
// are the two marks a charge cycle is measured between, and the only readings that change
// the walk's state.
const (
	cycleFull  = 95
	cycleEmpty = 10
)

// LastBatteryMarkBefore returns the newest device_samples row strictly before t that
// crossed either cycle mark. The chart seeds its cycle walk with it.
//
// It is deliberately the last *crossing*, not the last reading: a pack charged to full
// and then allowed to fall to 80% before the window opened is still armed, and the last
// reading (80) would say otherwise, dropping a cycle whose discharge lands on screen.
// Whichever mark came most recently decides — a full mark arms the walk, an empty mark
// clears it — so the returned value is >= cycleFull exactly when a cycle is open.
func (d *DB) LastBatteryMarkBefore(ctx context.Context, deviceID uuid.UUID, t time.Time) (time.Time, int, bool, error) {
	var at time.Time
	var pct int
	err := d.pool.QueryRow(ctx, `
		SELECT at, battery_pct FROM device_samples
		WHERE device_id = $1 AND at < $2
		  AND (battery_pct >= $3 OR battery_pct <= $4)
		ORDER BY at DESC LIMIT 1`, deviceID, t, cycleFull, cycleEmpty).Scan(&at, &pct)
	if errors.Is(err, pgx.ErrNoRows) {
		return time.Time{}, 0, false, nil
	}
	if err != nil {
		return time.Time{}, 0, false, err
	}
	return at, pct, true, nil
}

// GetStateTimeline returns the transitions of the given keys inside a window, plus the
// last transition of each key BEFORE it. That leading row is what makes the series
// correct at its left edge: a device that went on charge yesterday and has not changed
// since has no event inside today's window, and without it the chart would show the
// state as unknown until the next flip.
func (d *DB) GetStateTimeline(ctx context.Context, deviceID uuid.UUID, keys []string, from, until time.Time) ([]StateAt, error) {
	rows, err := d.pool.Query(ctx, `
		(
			SELECT DISTINCT ON (key) at, key, to_val
			FROM device_state_events
			WHERE device_id = $1 AND key = ANY($2) AND at < $3
			ORDER BY key, at DESC
		)
		UNION ALL
		(
			SELECT at, key, to_val
			FROM device_state_events
			WHERE device_id = $1 AND key = ANY($2) AND at >= $3 AND at <= $4
		)
		ORDER BY at`, deviceID, keys, from, until)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []StateAt
	for rows.Next() {
		var s StateAt
		if err := rows.Scan(&s.At, &s.Key, &s.Value); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
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
// at is when the report arrived — the check-in's own timestamp, since this may run from
// the ingest queue a moment later and a reboot's time is derived from it and uptime.
func (d *DB) IngestDeviceEvents(ctx context.Context, deviceID uuid.UUID, buildID string, extra json.RawMessage, at time.Time) {
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
	now := at.UTC()
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
