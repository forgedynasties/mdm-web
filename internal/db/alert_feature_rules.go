package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ── Recent-tier rules of 17 Oct: host app away, compliance, lost devices ──────
//
// Evaluated by EvaluateRecentAlerts like the rest of the recent tier; the window and
// deployed-only gating is the caller's.

// detectHostAppAway: the host app's variant in use has been out of the foreground for
// at least `minutes`, while the screen has been on for as long and the device kept
// reporting. Both states are device_state_events transitions — host_app_fg is derived
// from extra.host_app at ingest (withDerivedState), screen_on is a report field — so
// "for N minutes" is "the latest transition is that old", the way wlc_continuous reads
// its pad state. device_samples proves the device was reporting across the span: a
// device that went dark keeps its last known state, which is not the same as away.
func (d *DB) detectHostAppAway(ctx context.Context, p map[string]float64, connected []uuid.UUID) ([]alertHit, string, error) {
	mins := param(p, "minutes", 5)
	rows, err := d.pool.Query(ctx, `
		WITH pres AS (
			SELECT device_id, MIN(at) AS mn, MAX(at) AS mx
			FROM device_samples WHERE at > NOW() - (($1 + 5) * INTERVAL '1 minute')
			GROUP BY device_id
		)
		SELECT p.device_id, dv.serial_number,
		       COALESCE(dv.latest_extra->'host_app'->>'package', ''),
		       EXTRACT(EPOCH FROM (NOW() - fg.at))/60
		FROM pres p
		JOIN devices dv ON dv.id = p.device_id
		JOIN LATERAL (
			SELECT e.to_val, e.at FROM device_state_events e
			WHERE e.device_id = p.device_id AND e.key = 'host_app_fg'
			ORDER BY e.at DESC LIMIT 1
		) fg ON true
		JOIN LATERAL (
			SELECT e.to_val, e.at FROM device_state_events e
			WHERE e.device_id = p.device_id AND e.key = 'screen_on'
			ORDER BY e.at DESC LIMIT 1
		) scr ON true
		WHERE NOT dv.hidden AND dv.custody_server = ''
		  AND (dv.last_seen_at > NOW() - INTERVAL '`+recentReportingCutoff+`' OR dv.id = ANY($2::uuid[]))
		  AND fg.to_val = 'false' AND fg.at <= NOW() - ($1 * INTERVAL '1 minute')
		  AND scr.to_val = 'true' AND scr.at <= NOW() - ($1 * INTERVAL '1 minute')
		  -- lost mode puts the client's own screen up on purpose
		  AND COALESCE(dv.latest_extra->'lost_mode'->>'on', '') <> 'true'
		  AND (p.mx - p.mn) >= ($1 * INTERVAL '1 minute')`, mins, connected)
	if err != nil {
		return nil, "warning", err
	}
	defer rows.Close()
	var hits []alertHit
	for rows.Next() {
		var id uuid.UUID
		var serial, pkg string
		var away float64
		if err := rows.Scan(&id, &serial, &pkg, &away); err != nil {
			return nil, "warning", err
		}
		hits = append(hits, alertHit{id, serial,
			fmt.Sprintf("Host app not on screen for %.0f min while the screen is on (%s)", away, pkg),
			map[string]any{"package": pkg, "away_min": away, "limit_min": mins}})
	}
	return hits, "warning", rows.Err()
}

// detectComplianceFailed: a device has failed at least one required compliance check
// for `minutes` (compliance_results.since is when the state last changed). One alert
// per device, naming every required check it fails; it clears when none is left.
// Check ids, not names: the registry lives in the dashboard (alert_humanize names them).
func (d *DB) detectComplianceFailed(ctx context.Context, p map[string]float64) ([]alertHit, string, error) {
	mins := param(p, "minutes", 30)
	rows, err := d.pool.Query(ctx, `
		SELECT r.device_id, dv.serial_number,
		       array_agg(r.check_id ORDER BY r.check_id), array_agg(r.detail ORDER BY r.check_id),
		       MIN(r.since)
		FROM compliance_results r
		JOIN devices dv ON dv.id = r.device_id
		WHERE NOT dv.hidden AND r.state = $2 AND r.level = $3
		  AND r.since <= NOW() - ($1 * INTERVAL '1 minute')
		GROUP BY r.device_id, dv.serial_number`, mins, ComplianceFail, ComplianceRequired)
	if err != nil {
		return nil, "warning", err
	}
	defer rows.Close()
	var hits []alertHit
	for rows.Next() {
		var id uuid.UUID
		var serial string
		var checks, details []string
		var since any
		if err := rows.Scan(&id, &serial, &checks, &details, &since); err != nil {
			return nil, "warning", err
		}
		summary := fmt.Sprintf("Fails %d required compliance check(s): %s", len(checks), strings.Join(checks, ", "))
		if len(checks) == 1 {
			summary = "Fails a required compliance check: " + checks[0]
		}
		hits = append(hits, alertHit{id, serial, summary,
			map[string]any{"checks": checks, "details": details, "since": since, "limit_min": mins}})
	}
	return hits, "warning", rows.Err()
}

// detectLostDeviceSeen: a device in lost mode (extra.lost_mode.on) is reporting. It is
// critical whatever the hour, and carries the last location the server resolved for
// it (latest_extra latitude/longitude, from its Wi-Fi scan), so whoever is paged knows
// where to look. It stays open while the device keeps reporting in lost mode.
func (d *DB) detectLostDeviceSeen(ctx context.Context, connected []uuid.UUID) ([]alertHit, string, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT dv.id, dv.serial_number, dv.last_seen_at,
		       dv.latest_extra->'latitude', dv.latest_extra->'longitude',
		       dv.latest_extra->'location_accuracy', dv.latest_extra->'location_address',
		       COALESCE(dv.latest_extra->>'ip_address', '')
		FROM devices dv
		WHERE NOT dv.hidden AND dv.custody_server = ''
		  AND dv.latest_extra->'lost_mode'->>'on' = 'true'
		  AND COALESCE(dv.latest_extra->'lost_mode'->>'self', 'false') <> 'true' -- self_locked covers those
		  AND (dv.last_seen_at > NOW() - INTERVAL '`+recentReportingCutoff+`' OR dv.id = ANY($1::uuid[]))`, connected)
	if err != nil {
		return nil, "critical", err
	}
	defer rows.Close()
	var hits []alertHit
	for rows.Next() {
		var id uuid.UUID
		var serial, ip string
		var seen any
		var lat, lon, acc, addr json.RawMessage
		if err := rows.Scan(&id, &serial, &seen, &lat, &lon, &acc, &addr, &ip); err != nil {
			return nil, "critical", err
		}
		detail := map[string]any{"last_seen_at": seen}
		summary := "Lost device checked in"
		if len(lat) > 0 && string(lat) != "null" && len(lon) > 0 && string(lon) != "null" {
			detail["latitude"], detail["longitude"] = lat, lon
			if len(acc) > 0 && string(acc) != "null" {
				detail["location_accuracy"] = acc
			}
			var a string
			if json.Unmarshal(addr, &a) == nil && a != "" {
				detail["location_address"] = a
				summary += " near " + a
			} else {
				summary += fmt.Sprintf(" at %s, %s", lat, lon)
			}
		} else {
			summary += " (no location yet)"
		}
		if ip != "" {
			detail["ip_address"] = ip
		}
		hits = append(hits, alertHit{id, serial, summary, detail})
	}
	return hits, "critical", rows.Err()
}

// ── Lost-device reporting (18 Oct): left the venue, heard while lost, locked itself ──

// detectLeftVenue (A): a battery device at a venue that dropped off the Wi-Fi while off its
// charger — the moment a tablet walks out, before anyone notices it is gone. "Off its
// charger" is the last report's charging=false (the firmware client reports true whenever
// anything is plugged in) and charger_type none. One alert per departure: it is not raised
// again for the same silence once resolved (as offline), and clears when the device is back
// or someone marks it lost (lost_device_seen / lost_device_heard take over then). The
// signal readings before it went quiet are attached, and named when they were falling.
func (d *DB) detectLeftVenue(ctx context.Context, p map[string]float64, connected []uuid.UUID) ([]alertHit, string, error) {
	mins := param(p, "offline_minutes", 3)
	rows, err := d.pool.Query(ctx, `
		SELECT d.id, d.serial_number, d.last_seen_at, COALESCE(r.name, ''), COALESCE(n.name, ''),
		       COALESCE((SELECT array_agg(x.wifi_rssi ORDER BY x.at) FROM (
		           SELECT s.wifi_rssi, s.at FROM device_samples s
		           WHERE s.device_id = d.id AND NOT s.late AND s.wifi_rssi IS NOT NULL
		             AND s.at > d.last_seen_at - INTERVAL '20 minutes' AND s.at <= d.last_seen_at + INTERVAL '1 minute'
		           ORDER BY s.at DESC LIMIT 8) x), '{}')
		FROM devices d
		JOIN restaurants r ON r.id = d.restaurant_id
		LEFT JOIN device_nicknames n ON n.device_id = d.id
		WHERE NOT d.hidden AND d.custody_server = '' AND d.id <> ALL(COALESCE($2::uuid[], '{}'))
		  AND NOT `+LostPausedSQL+` -- the venue paused lost-device watching (venue_lost_pause.go)
		  AND d.last_seen_at < NOW() - ($1 * INTERVAL '1 minute')
		  AND d.last_seen_at > NOW() - INTERVAL '6 hours'
		  AND d.latest_extra->>'charging' = 'false'
		  AND COALESCE(d.latest_extra->>'charger_type', 'none') IN ('none', '')
		  AND `+hasBatteryD+`
		  AND COALESCE(d.latest_extra->'lost_mode'->>'on', 'false') <> 'true'
		  AND NOT EXISTS (SELECT 1 FROM lost_sessions ls WHERE ls.device_id = d.id AND ls.ended_at IS NULL)
		  AND NOT EXISTS (SELECT 1 FROM alerts a WHERE a.device_id = d.id AND a.type = 'left_venue'
		                    AND a.status = 'resolved' AND a.fired_at >= d.last_seen_at)`, mins, connected)
	if err != nil {
		return nil, "critical", err
	}
	defer rows.Close()
	now := time.Now().UTC()
	var hits []alertHit
	for rows.Next() {
		var id uuid.UUID
		var serial, venue, nick string
		var last time.Time
		var rssi []int16
		if err := rows.Scan(&id, &serial, &last, &venue, &nick, &rssi); err != nil {
			return nil, "critical", err
		}
		detail := map[string]any{"last_seen": last, "offline_minutes": int(now.Sub(last).Minutes()),
			"venue": venue, "serial": serial, "mark_lost": true}
		if nick != "" {
			detail["nickname"] = nick
		}
		summary := "Dropped off the Wi-Fi away from its charger"
		if len(rssi) >= 2 {
			from, to := rssi[0], rssi[len(rssi)-1]
			detail["rssi_from"], detail["rssi_to"] = from, to
			if from-to >= 8 {
				detail["rssi_falling"] = true
				summary += fmt.Sprintf(" as its signal faded (%d → %d dBm)", from, to)
			}
		}
		hits = append(hits, alertHit{id, serial, summary, detail})
	}
	return hits, "critical", rows.Err()
}

// detectLostDeviceHeard (B): a lost device that is out of reach itself (no socket, not
// reporting) but that a neighbour at its venue heard on Bluetooth in the last `minutes`.
// The detail names the neighbour that heard it loudest — "near Table 7".
func (d *DB) detectLostDeviceHeard(ctx context.Context, p map[string]float64, connected []uuid.UUID) ([]alertHit, string, error) {
	mins := param(p, "minutes", 10)
	rows, err := d.pool.Query(ctx, `
		SELECT ls.device_id, dv.serial_number, b.heard_by, h.serial_number, COALESCE(NULLIF(n.name, ''), h.serial_number),
		       b.rssi, b.at, cnt.c
		FROM lost_sessions ls
		JOIN devices dv ON dv.id = ls.device_id
		JOIN LATERAL (
			SELECT x.heard_by, x.rssi, x.at FROM ble_sightings x
			WHERE x.session_id = ls.id AND x.at > NOW() - ($1 * INTERVAL '1 minute')
			ORDER BY x.rssi DESC, x.at DESC LIMIT 1) b ON true
		JOIN LATERAL (
			SELECT COUNT(DISTINCT y.heard_by) AS c FROM ble_sightings y
			WHERE y.session_id = ls.id AND y.at > NOW() - ($1 * INTERVAL '1 minute')) cnt ON true
		JOIN devices h ON h.id = b.heard_by
		LEFT JOIN device_nicknames n ON n.device_id = h.id
		WHERE ls.ended_at IS NULL AND NOT dv.hidden
		  AND dv.id <> ALL(COALESCE($2::uuid[], '{}'))
		  AND dv.last_seen_at < NOW() - INTERVAL '3 minutes'`, mins, connected)
	if err != nil {
		return nil, "critical", err
	}
	defer rows.Close()
	var hits []alertHit
	for rows.Next() {
		var id, by uuid.UUID
		var serial, bySerial, byName string
		var rssi int16
		var at time.Time
		var n int
		if err := rows.Scan(&id, &serial, &by, &bySerial, &byName, &rssi, &at, &n); err != nil {
			return nil, "critical", err
		}
		summary := fmt.Sprintf("Lost device heard by %s (%s)", byName, SignalWord(int(rssi)))
		if n > 1 {
			summary += fmt.Sprintf(" and %d more", n-1)
		}
		hits = append(hits, alertHit{id, serial, summary, map[string]any{
			"heard_by": bySerial, "heard_by_name": byName, "rssi": rssi, "signal": SignalWord(int(rssi)),
			"heard_at": at, "event_at": at, "neighbours": n}})
	}
	return hits, "critical", rows.Err()
}

// detectSelfLocked (D): a device locked itself after leaving its venue, and has not lifted
// it. Open while its self-started lost session is.
func (d *DB) detectSelfLocked(ctx context.Context) ([]alertHit, string, error) {
	rows, err := d.pool.Query(ctx, `
		SELECT ls.device_id, dv.serial_number, ls.started_at, COALESCE(r.name, '')
		FROM lost_sessions ls
		JOIN devices dv ON dv.id = ls.device_id
		LEFT JOIN restaurants r ON r.id = dv.restaurant_id
		WHERE ls.ended_at IS NULL AND ls.started_by = 'self' AND NOT dv.hidden`)
	if err != nil {
		return nil, "critical", err
	}
	defer rows.Close()
	var hits []alertHit
	for rows.Next() {
		var id uuid.UUID
		var serial, venue string
		var at time.Time
		if err := rows.Scan(&id, &serial, &at, &venue); err != nil {
			return nil, "critical", err
		}
		hits = append(hits, alertHit{id, serial, "Locked itself after leaving its venue",
			map[string]any{"locked_at": at, "event_at": at, "venue": venue}})
	}
	return hits, "critical", rows.Err()
}

// SignalWord says how close a Bluetooth reading puts the device, for people: a rough guide
// indoors, not a distance.
func SignalWord(rssi int) string {
	switch {
	case rssi >= -60:
		return "very close"
	case rssi >= -75:
		return "close"
	case rssi >= -88:
		return "in range"
	}
	return "faint"
}
