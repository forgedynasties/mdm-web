package db

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// ── Lost-device reporting (18 Oct; the user picked A+B+C+D) ───────────────────
//
// A T7 has only Wi-Fi, so once it leaves its venue's network it cannot report. This is
// what the server does for the time it is out of reach:
//
//	A  left_venue alert: dropped off the Wi-Fi off its charger, during service
//	   (alert_feature_rules.go).
//	B  a lost session hands the device a random beacon (Bluetooth LE); the other devices
//	   at its venue are told to listen for it (config lost_listen) and report what they
//	   hear (extra.ble_sightings → ble_sightings).
//	C  the Wi-Fi scans a device kept while offline (backfill wifi_scan) become a trail,
//	   geolocated only for lost or departed devices to bound the Google cost.
//	D  a venue's opt-in (self_lock_minutes) lets its devices lock themselves when the
//	   venue's Wi-Fi vanishes and they are off their charger (config self_lock).

// lostConfigGen moves whenever something that changes a device's lost_listen or self_lock
// config does (a lost session starts or ends, a venue's self-lock setting changes), so a
// per-device cache of that config knows to look again. See LostConfigGen.
var lostConfigGen atomic.Uint64

// LostConfigGen is the current generation of lost-reporting config.
func (d *DB) LostConfigGen() uint64 { return lostConfigGen.Load() }

// newBeacon is a fresh 8-byte token as 16 hex characters: random, not derived from the
// device, so nobody outside the MDM can recognise a device from its beacon.
func newBeacon() string {
	b := make([]byte, 8)
	_, _ = rand.Read(b)
	return hex.EncodeToString(b)
}

// withLostBeacon returns a lost_mode {"on": true} payload with a fresh beacon added. The
// payload has been normalised already, so it is an object.
func withLostBeacon(payload json.RawMessage) (json.RawMessage, string) {
	var m map[string]any
	if json.Unmarshal(payload, &m) != nil {
		return payload, ""
	}
	if on, _ := m["on"].(bool); !on {
		return payload, ""
	}
	b := newBeacon()
	m["beacon"] = b
	out, _ := json.Marshal(m)
	return out, b
}

// recordLostSessions starts (on) or ends (off) the lost sessions of a lost_mode command's
// target devices, inside the command's own transaction. A new "on" ends any session the
// device already had: one device, one live beacon.
func recordLostSessions(ctx context.Context, tx pgx.Tx, payload json.RawMessage, beacon string, cmdID uuid.UUID, by string, deviceIDs []uuid.UUID) error {
	var p struct {
		On bool `json:"on"`
	}
	_ = json.Unmarshal(payload, &p)
	for _, id := range deviceIDs {
		if _, err := tx.Exec(ctx, `UPDATE lost_sessions SET ended_at = NOW() WHERE device_id = $1 AND ended_at IS NULL`, id); err != nil {
			return err
		}
		if !p.On {
			continue
		}
		var b any
		if beacon != "" {
			b = beacon
		}
		if _, err := tx.Exec(ctx, `INSERT INTO lost_sessions (device_id, beacon, command_id, started_by) VALUES ($1, $2, $3, $4)`,
			id, b, cmdID, by); err != nil {
			return err
		}
	}
	lostConfigGen.Add(1)
	return nil
}

// LostSession is one "marked lost" period.
type LostSession struct {
	ID        uuid.UUID
	DeviceID  uuid.UUID
	Beacon    string
	StartedBy string
	StartedAt time.Time
	EndedAt   *time.Time
}

// ActiveLostSession returns a device's open lost session, or nil.
func (d *DB) ActiveLostSession(ctx context.Context, deviceID uuid.UUID) (*LostSession, error) {
	var s LostSession
	err := d.pool.QueryRow(ctx, `
		SELECT id, device_id, COALESCE(beacon, ''), started_by, started_at, ended_at
		FROM lost_sessions WHERE device_id = $1 AND ended_at IS NULL
		ORDER BY started_at DESC LIMIT 1`, deviceID).Scan(&s.ID, &s.DeviceID, &s.Beacon, &s.StartedBy, &s.StartedAt, &s.EndedAt)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &s, nil
}

// LostConfig is what a device's config answer carries for lost-device reporting.
type LostConfig struct {
	VenueID         *uuid.UUID
	Venue           string
	SelfLockMinutes int
	Beacons         []string // active beacons of other lost devices at the same venue
	Lost            bool     // this device has an open lost session
	DPC             bool     // the Standard client, which has no self-lock
}

// Lost-listen scan cadence the listeners are told: scan_s seconds of Bluetooth LE every
// every_s seconds, only while a device at their venue is lost.
const (
	LostListenScanS  = 8
	LostListenEveryS = 120
)

// LostConfigKeys is the lost-reporting part of a device's config: lost_listen and, for the
// firmware client, self_lock. Every config answer and every config frame carries both,
// because a frame can be partial and the clients read a missing key as "unchanged"
// (lost_listen) or "off" (self_lock): lost_listen is null when nothing nearby is lost, and
// self_lock is {"enabled": false} when the venue has it off. phone is the AIO support
// number for the lost screen.
func LostConfigKeys(c LostConfig, phone string) map[string]any {
	out := map[string]any{"lost_listen": nil}
	if len(c.Beacons) > 0 {
		out["lost_listen"] = map[string]any{"beacons": c.Beacons, "scan_s": LostListenScanS, "every_s": LostListenEveryS}
	}
	if c.DPC {
		return out
	}
	if c.SelfLockMinutes <= 0 {
		out["self_lock"] = map[string]any{"enabled": false}
		return out
	}
	msg := LostMessage(phone)
	if c.Venue != "" {
		msg = "It belongs to " + c.Venue + ". " + msg
	}
	out["self_lock"] = map[string]any{"enabled": true, "minutes": c.SelfLockMinutes, "message": msg, "phone": phone}
	return out
}

// LostConfigFor reads a device's lost-reporting config: its venue's self-lock setting and
// the beacons of the other devices there that are lost right now. One query per check-in;
// the api caches it per device by LostConfigGen.
func (d *DB) LostConfigFor(ctx context.Context, deviceID uuid.UUID) (LostConfig, error) {
	var c LostConfig
	err := d.pool.QueryRow(ctx, `
		SELECT d.restaurant_id, COALESCE(r.name, ''), COALESCE(r.self_lock_minutes, 0),
		       COALESCE((SELECT array_agg(s.beacon ORDER BY s.started_at)
		                 FROM lost_sessions s JOIN devices o ON o.id = s.device_id
		                 WHERE s.ended_at IS NULL AND s.beacon IS NOT NULL
		                   AND o.restaurant_id = d.restaurant_id AND o.id <> d.id), '{}'),
		       EXISTS (SELECT 1 FROM lost_sessions s WHERE s.device_id = d.id AND s.ended_at IS NULL),
		       COALESCE(d.agent_kind, '') = 'dpc'
		FROM devices d LEFT JOIN restaurants r ON r.id = d.restaurant_id
		WHERE d.id = $1`, deviceID).Scan(&c.VenueID, &c.Venue, &c.SelfLockMinutes, &c.Beacons, &c.Lost, &c.DPC)
	return c, err
}

// VenueDeviceIDs returns the other devices at the venues of the given devices — the ones
// that should start or stop listening when one of them is marked lost or lifted.
func (d *DB) VenueDeviceIDs(ctx context.Context, deviceIDs []uuid.UUID) ([]uuid.UUID, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT o.id FROM devices o
		WHERE NOT o.hidden AND o.id <> ALL($1::uuid[])
		  AND o.restaurant_id IN (SELECT restaurant_id FROM devices WHERE id = ANY($1::uuid[]) AND restaurant_id IS NOT NULL)`, deviceIDs)
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

// SetRestaurantSelfLock sets a venue's self-lock opt-in (0 = off).
func (d *DB) SetRestaurantSelfLock(ctx context.Context, restaurantID uuid.UUID, minutes int) error {
	_, err := d.pool.Exec(ctx, `UPDATE restaurants SET self_lock_minutes = $2 WHERE id = $1`, restaurantID, minutes)
	if err == nil {
		lostConfigGen.Add(1)
	}
	return err
}

// ── B: what the neighbours heard ──────────────────────────────────────────────

// IngestBLESightings stores extra.ble_sightings from a report: each beacon the reporting
// device heard, matched to the lost session it belongs to. Beacons of sessions that have
// ended, or that no session ever had, are ignored. Best-effort, like IngestDeviceEvents.
func (d *DB) IngestBLESightings(ctx context.Context, heardBy uuid.UUID, extra json.RawMessage, at time.Time) {
	if len(extra) == 0 || !strings.Contains(string(extra), "ble_sightings") {
		return
	}
	var e struct {
		S []struct {
			Beacon string `json:"beacon"`
			RSSI   int    `json:"rssi"`
			AtMs   int64  `json:"at_ms"`
		} `json:"ble_sightings"`
	}
	if json.Unmarshal(extra, &e) != nil || len(e.S) == 0 {
		return
	}
	if len(e.S) > 50 {
		e.S = e.S[:50]
	}
	for _, s := range e.S {
		b := strings.ToLower(strings.TrimSpace(s.Beacon))
		if len(b) != 16 || s.RSSI < -127 || s.RSSI > 20 {
			continue
		}
		when := at
		// The listener's own clock for when it heard it, unless it is far off ours.
		if s.AtMs > 0 {
			if t := time.UnixMilli(s.AtMs); t.Before(at.Add(time.Minute)) && t.After(at.Add(-30*time.Minute)) {
				when = t
			}
		}
		// A listener reports every scan (every couple of minutes) while it hears something:
		// one row per neighbour per minute, the strongest reading kept.
		_, _ = d.pool.Exec(ctx, `
			INSERT INTO ble_sightings (session_id, heard_by, rssi, at)
			SELECT s.id, $2, $3, $4 FROM lost_sessions s
			WHERE s.beacon = $1 AND s.device_id <> $2
			  AND (s.ended_at IS NULL OR s.ended_at > $4)
			ON CONFLICT (session_id, heard_by, at) DO UPDATE SET rssi = GREATEST(ble_sightings.rssi, EXCLUDED.rssi)`,
			b, heardBy, s.RSSI, when.UTC().Truncate(time.Minute))
	}
}

// BLESighting is the strongest a lost device was heard by one neighbour, and when last.
type BLESighting struct {
	HeardBy       uuid.UUID
	HeardBySerial string
	HeardByName   string // nickname, else serial
	RSSI          int    // strongest in the window
	LastAt        time.Time
	Count         int
}

// LostSightings returns, for a device's current (or most recent) lost session, each
// neighbour that heard it since `since`, strongest first.
func (d *DB) LostSightings(ctx context.Context, deviceID uuid.UUID, since time.Time) ([]BLESighting, error) {
	rows, err := d.pool.Query(ctx, `
		WITH s AS (
			SELECT id FROM lost_sessions WHERE device_id = $1
			ORDER BY (ended_at IS NULL) DESC, started_at DESC LIMIT 1
		)
		SELECT b.heard_by, h.serial_number, COALESCE(NULLIF(n.name, ''), h.serial_number),
		       MAX(b.rssi), MAX(b.at), COUNT(*)
		FROM ble_sightings b JOIN s ON s.id = b.session_id
		JOIN devices h ON h.id = b.heard_by
		LEFT JOIN device_nicknames n ON n.device_id = h.id
		WHERE b.at >= $2
		GROUP BY b.heard_by, h.serial_number, n.name
		ORDER BY MAX(b.rssi) DESC, MAX(b.at) DESC`, deviceID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []BLESighting
	for rows.Next() {
		var s BLESighting
		if err := rows.Scan(&s.HeardBy, &s.HeardBySerial, &s.HeardByName, &s.RSSI, &s.LastAt, &s.Count); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// ── C: the trail ──────────────────────────────────────────────────────────────

// TrailScan is one Wi-Fi scan a device kept while out of reach.
type TrailScan struct {
	At   time.Time
	Scan json.RawMessage // [{"bssid","ssid","rssi"}, …]
}

// InsertTrailScans stores the scans of a backfill batch as unplaced trail points. A scan
// already stored (same device and time) is skipped. Returns how many were new.
func (d *DB) InsertTrailScans(ctx context.Context, deviceID uuid.UUID, scans []TrailScan) (int, error) {
	n := 0
	for _, s := range scans {
		tag, err := d.pool.Exec(ctx, `
			INSERT INTO device_trail (device_id, at, source, scan) VALUES ($1, $2, 'scan', $3)
			ON CONFLICT (device_id, at, source) DO NOTHING`, deviceID, s.At.UTC(), s.Scan)
		if err != nil {
			return n, err
		}
		n += int(tag.RowsAffected())
	}
	return n, nil
}

// InsertTrailFix records where a lost device was while it was online (the location its
// check-in resolved), at most one point per `every`.
func (d *DB) InsertTrailFix(ctx context.Context, deviceID uuid.UUID, at time.Time, lat, lon, accuracy float64, every time.Duration) error {
	_, err := d.pool.Exec(ctx, `
		INSERT INTO device_trail (device_id, at, source, lat, lon, accuracy, resolved_at)
		SELECT $1, $2, 'live', $3, $4, $5, NOW()
		WHERE NOT EXISTS (
			SELECT 1 FROM device_trail WHERE device_id = $1 AND source = 'live'
			  AND at > $2::timestamptz - make_interval(secs => $6::double precision))
		ON CONFLICT DO NOTHING`, deviceID, at.UTC(), lat, lon, accuracy, every.Seconds())
	return err
}

// PendingTrailScan is a stored scan not yet placed.
type PendingTrailScan struct {
	ID       int64
	DeviceID uuid.UUID
	Scan     json.RawMessage
}

// PendingTrailScans returns scans worth placing: only for devices that are lost (an open
// lost session, or reporting lost mode) or that set off left_venue in the last day — the
// Google lookups are paid for, and a diary of a device nobody is looking for is not worth
// it. Newest first, at most limit.
func (d *DB) PendingTrailScans(ctx context.Context, limit int) ([]PendingTrailScan, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT t.id, t.device_id, t.scan FROM device_trail t
		JOIN devices dv ON dv.id = t.device_id
		WHERE t.resolved_at IS NULL AND t.source = 'scan' AND t.scan IS NOT NULL
		  AND t.at > NOW() - INTERVAL '8 days'
		  AND (EXISTS (SELECT 1 FROM lost_sessions s WHERE s.device_id = t.device_id AND s.ended_at IS NULL)
		       OR dv.latest_extra->'lost_mode'->>'on' = 'true'
		       OR EXISTS (SELECT 1 FROM alerts a WHERE a.device_id = t.device_id AND a.type = 'left_venue'
		                    AND a.fired_at > NOW() - INTERVAL '24 hours'))
		ORDER BY t.at DESC LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []PendingTrailScan
	for rows.Next() {
		var p PendingTrailScan
		if err := rows.Scan(&p.ID, &p.DeviceID, &p.Scan); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// ResolveTrailScan places a stored scan (ok=false: tried, could not be placed).
func (d *DB) ResolveTrailScan(ctx context.Context, id int64, lat, lon, accuracy float64, ok bool) error {
	if !ok {
		_, err := d.pool.Exec(ctx, `UPDATE device_trail SET resolved_at = NOW() WHERE id = $1`, id)
		return err
	}
	_, err := d.pool.Exec(ctx, `UPDATE device_trail SET lat = $2, lon = $3, accuracy = $4, resolved_at = NOW() WHERE id = $1`,
		id, lat, lon, accuracy)
	return err
}

// TrailPoint is one placed point of a device's trail.
type TrailPoint struct {
	At       time.Time
	Source   string // scan | live
	Lat, Lon float64
	Accuracy float64
}

// DeviceTrail returns a device's placed trail points since `since`, oldest first.
func (d *DB) DeviceTrail(ctx context.Context, deviceID uuid.UUID, since time.Time) ([]TrailPoint, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT at, source, lat, lon, COALESCE(accuracy, 0) FROM device_trail
		WHERE device_id = $1 AND at >= $2 AND lat IS NOT NULL
		ORDER BY at`, deviceID, since)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []TrailPoint
	for rows.Next() {
		var p TrailPoint
		if err := rows.Scan(&p.At, &p.Source, &p.Lat, &p.Lon, &p.Accuracy); err != nil {
			return nil, err
		}
		out = append(out, p)
	}
	return out, rows.Err()
}

// RecordSelfLock notes that a device locked itself after leaving its venue (D): a lost
// session started by the device ("self"), from the time it says it locked, with no beacon.
// Kept with the dashboard's lost sessions rather than as a device event so the timeline,
// the trail and the alerts treat it the same way — device_events is read as crashes by
// every crash query. Idempotent: an open session (self or not) means nothing to add.
func (d *DB) RecordSelfLock(ctx context.Context, deviceID uuid.UUID, at time.Time) {
	tag, err := d.pool.Exec(ctx, `
		INSERT INTO lost_sessions (device_id, started_by, started_at)
		SELECT $1, 'self', $2
		WHERE NOT EXISTS (SELECT 1 FROM lost_sessions WHERE device_id = $1 AND ended_at IS NULL)`,
		deviceID, at.UTC().Truncate(time.Second))
	if err == nil && tag.RowsAffected() > 0 {
		lostConfigGen.Add(1)
	}
}

// EndSelfLock closes a device's self-started lost session once it reports it has lifted it
// (it saw its venue again). A session the dashboard started is never ended by the device.
func (d *DB) EndSelfLock(ctx context.Context, deviceID uuid.UUID) {
	tag, err := d.pool.Exec(ctx, `UPDATE lost_sessions SET ended_at = NOW()
		WHERE device_id = $1 AND ended_at IS NULL AND started_by = 'self'`, deviceID)
	if err == nil && tag.RowsAffected() > 0 {
		lostConfigGen.Add(1)
	}
}

// ── The device page's timeline (H1, picked 18 Oct) ─────────────────────────────

// LostEvent is one line of a lost device's timeline. Kind is left_venue | lost_marked |
// self_locked | heard | trail | lost_lifted. Lat/Lon are set for points that have a place
// (Wi-Fi diary and live fixes); Late marks what the device sent after it came back.
type LostEvent struct {
	At       time.Time
	Kind     string
	Title    string // "Left the venue", "Marked as lost", "Heard by Table 7" …
	Detail   string // "by Sara", "very close", "Wi-Fi diary", "signal faded -55 → -79 dBm" …
	By       string // who marked or lifted it, the neighbour that heard it
	Lat, Lon *float64
	Accuracy float64
	Late     bool
}

// lostTimelineSpan picks what the timeline covers: from a little before the device's most
// recent lost session (or a departure alert, whichever is earlier) to the session's end or
// now. Nothing in the last 7 days → zero time.
func (d *DB) lostTimelineSpan(ctx context.Context, deviceID uuid.UUID) (from, to time.Time, err error) {
	var start, alertAt *time.Time
	var end *time.Time
	err = d.pool.QueryRow(ctx, `
		SELECT (SELECT started_at FROM lost_sessions WHERE device_id = $1 AND started_at > NOW() - INTERVAL '7 days'
		        ORDER BY started_at DESC LIMIT 1),
		       (SELECT ended_at FROM lost_sessions WHERE device_id = $1 AND started_at > NOW() - INTERVAL '7 days'
		        ORDER BY started_at DESC LIMIT 1),
		       (SELECT fired_at FROM alerts WHERE device_id = $1 AND type = 'left_venue' AND fired_at > NOW() - INTERVAL '7 days'
		        ORDER BY fired_at DESC LIMIT 1)`, deviceID).Scan(&start, &end, &alertAt)
	if err != nil || (start == nil && alertAt == nil) {
		return time.Time{}, time.Time{}, err
	}
	from = time.Now()
	if start != nil {
		from = *start
	}
	// A departure up to a day before it was marked lost is part of the same story.
	if alertAt != nil && (start == nil || (alertAt.Before(from) && from.Sub(*alertAt) < 24*time.Hour)) {
		from = *alertAt
	}
	from = from.Add(-30 * time.Minute)
	to = time.Now()
	if end != nil && start != nil && alertAt != nil && alertAt.After(*end) {
		// A newer departure after the last session ended: that is the story now.
		from, end = alertAt.Add(-30*time.Minute), nil
	}
	if end != nil {
		to = end.Add(30 * time.Minute)
	}
	return from, to, nil
}

// LostTimeline returns a device's lost story, oldest first, and when it starts: the
// departure alert, being marked lost (or locking itself), each neighbour that heard it (one
// line per neighbour per 5 minutes, the strongest reading), the places its Wi-Fi diary and
// live fixes put it, and being lifted. Points the device sent after it came back slot in by
// the time they were taken, marked Late.
func (d *DB) LostTimeline(ctx context.Context, deviceID uuid.UUID) ([]LostEvent, time.Time, error) {
	from, to, err := d.lostTimelineSpan(ctx, deviceID)
	if err != nil || from.IsZero() {
		return nil, from, err
	}
	rows, err := d.pool.Query(ctx, `
		SELECT a.fired_at, 'left_venue', a.summary, '', NULL::float8, NULL::float8, 0::float8, false
		FROM alerts a WHERE a.device_id = $1 AND a.type = 'left_venue' AND a.fired_at BETWEEN $2 AND $3
		UNION ALL
		SELECT s.started_at, CASE WHEN s.started_by = 'self' THEN 'self_locked' ELSE 'lost_marked' END,
		       '', CASE WHEN s.started_by = 'self' THEN '' ELSE s.started_by END, NULL, NULL, 0, false
		FROM lost_sessions s WHERE s.device_id = $1 AND s.started_at BETWEEN $2 AND $3
		UNION ALL
		SELECT s.ended_at, 'lost_lifted', '', '', NULL, NULL, 0, false
		FROM lost_sessions s WHERE s.device_id = $1 AND s.ended_at BETWEEN $2 AND $3
		UNION ALL
		-- Sightings are kept per minute; one in the minute it was marked lost must not read
		-- as heard before it.
		SELECT MAX(GREATEST(b.at, s.started_at)), 'heard', MAX(b.rssi)::text, COALESCE(NULLIF(n.name, ''), h.serial_number), NULL, NULL, 0, false
		FROM ble_sightings b JOIN lost_sessions s ON s.id = b.session_id
		JOIN devices h ON h.id = b.heard_by LEFT JOIN device_nicknames n ON n.device_id = h.id
		WHERE s.device_id = $1 AND b.at BETWEEN $2 AND $3
		GROUP BY b.heard_by, h.serial_number, n.name, date_trunc('hour', b.at), floor(extract(minute FROM b.at) / 5)
		UNION ALL
		SELECT t.at, 'trail', t.source, '', t.lat, t.lon, COALESCE(t.accuracy, 0), t.source = 'scan'
		FROM device_trail t WHERE t.device_id = $1 AND t.lat IS NOT NULL AND t.at BETWEEN $2 AND $3
		ORDER BY 1`, deviceID, from, to)
	if err != nil {
		return nil, from, err
	}
	defer rows.Close()
	var out []LostEvent
	for rows.Next() {
		var e LostEvent
		var a, b string
		if err := rows.Scan(&e.At, &e.Kind, &a, &b, &e.Lat, &e.Lon, &e.Accuracy, &e.Late); err != nil {
			return nil, from, err
		}
		switch e.Kind {
		case "left_venue":
			e.Title, e.Detail = "Left the venue", a
		case "lost_marked":
			e.Title, e.By = "Marked as lost", b
			if b != "" {
				e.Detail = "by " + b
			}
		case "self_locked":
			e.Title, e.Detail = "Locked itself", "its venue's Wi-Fi was gone and it was off its charger"
		case "lost_lifted":
			e.Title = "Lost mode lifted"
		case "heard":
			rssi, _ := strconv.Atoi(a)
			e.Title, e.By, e.Detail = "Heard by "+b, b, SignalWord(rssi)
		case "trail":
			e.Title = "Seen here"
			if a == "scan" {
				e.Detail = "from its Wi-Fi diary, sent when it came back"
			} else {
				e.Detail = "while lost and online"
			}
		}
		out = append(out, e)
	}
	return out, from, rows.Err()
}

// RestaurantSelfLockMinutes is a venue's self-lock time, 0 when it is off.
func (d *DB) RestaurantSelfLockMinutes(ctx context.Context, restaurantID uuid.UUID) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `SELECT self_lock_minutes FROM restaurants WHERE id = $1`, restaurantID).Scan(&n)
	return n, err
}

// LostMessage is the lost screen's default request. It only asks for a call when there is
// a number to call: with no AIO support number set, "…or call:" ended in nothing (seen
// 19 Oct on AT070AA2600030).
func LostMessage(phone string) string {
	if strings.TrimSpace(phone) == "" {
		return "Please hand it to a member of staff."
	}
	return "Please hand it to a member of staff, or call:"
}
