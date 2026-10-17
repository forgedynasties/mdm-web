package db

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

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
