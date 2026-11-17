package db

import (
	"context"
	"fmt"
	"strings"

	"github.com/jackc/pgx/v5"
)

// Fleet reset (16 Nov 2026; user: "remove all the data from prod and then we can just do
// device by device so that it does not have stale data … without db access"). Prod
// (mdm.aioapp.com) started from a 5 Nov copy of the fleet server; before devices move to it
// one by one, everything describing the fleet goes, and what makes the server usable stays.
//
// Cleared: devices and all their history, venues, groups, lock rules, enrollment profiles,
// rollouts, alerts, commands, setup visits, the activity log copy.
// Kept: people, sessions and role-wide access, Lineup (models, roles, role apps, firmware
// channels), the app library, firmware releases and packages, settings, alert rules, the
// fleet-wide service window, geofences, caches learned from devices (app icons, Wi-Fi AP
// locations) and the AIO backend copy (it re-syncs itself).
//
// Only reachable through the admin API on a server whose .env says ALLOW_FLEET_RESET=1
// (api/fleet_reset.go); the fleet server never sets it.

// fleetTruncate is emptied whole in one TRUNCATE: history and fleet tables that nothing
// kept refers to. Every table another table refers to is listed together with it.
var fleetTruncate = []string{
	// commands and everything hanging off them
	"commands", "command_results", "command_status", "command_targets", "command_secrets",
	"dismissed_commands", "command_events",
	// device history
	"device_samples", "battery_samples", "app_usage", "device_state_events", "device_events",
	"device_packages", "device_daily_stats", "compliance_events", "compliance_results",
	"device_trail", "device_build_history", "battery_climbs", "device_temp_fast",
	"temp_fast_devices", "logcat_requests", "logcat_results", "lost_sessions", "ble_sightings",
	"device_samples_unpart", "battery_samples_unpart", "app_usage_unpart", // pre-partition copies, if still there
	"device_ai_analyses", "ws_sessions", "remote_tokens", "device_owners",
	// device settings and identity kept outside the devices row
	"device_config", "device_nicknames", "device_app_follows", "device_app_wants", "device_groups", "device_lab",
	"lab_installs", "key_clash_retries", "device_adb_key", "hardware_serials",
	"device_role_requests", "refused_checkins", "device_queries", "peer_outbox",
	"group_serial_watch",
	// alerts, rollouts' devices, legacy OTA
	"alerts", "alert_digests", "update_devices",
	"legacy_ota_deployment_devices", "legacy_ota_deployments", "legacy_ota_group_devices",
	"legacy_ota_groups", "legacy_ota_devices",
	// venues' own tables
	"sightings", "backend_hours", "backend_ignores", "kiosk_policies", "peak_windows",
	"setup_visit_people", "setup_visits", "compliance_report_sends", "venue_wifi_networks",
	"venue_wifi_pushes",
	// the activity log (a copy of the fleet server's) and the fleet summary built from it
	"audit_log", "ai_summary",
}

// fleetDelete is then deleted row by row, in this order, because tables that stay refer to
// them: access grants on a venue, group or device go with it (CASCADE), role-wide grants
// stay; the fleet-wide service window has no venue or group and stays.
var fleetDelete = []string{"updates", "devices", "groups", "enrollment_profiles", "restaurants"}

// DevicesSeenSince counts devices heard from in the last minutes (a fleet reset refuses
// while any are, since an HTTP-only client holds no socket to count).
func (d *DB) DevicesSeenSince(ctx context.Context, minutes int) (int, error) {
	var n int
	err := d.pool.QueryRow(ctx, `SELECT count(*) FROM devices WHERE last_seen_at > NOW() - make_interval(mins => $1)`, minutes).Scan(&n)
	return n, err
}

// FleetReset counts (dryRun) or clears the fleet's data. It returns rows per table: those
// present before, for a dry run; those removed, for a real one. Tables this server never
// created are skipped. All or nothing: one transaction.
func (d *DB) FleetReset(ctx context.Context, dryRun bool) (map[string]int64, error) {
	tx, err := d.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)

	exists := func(t string) (bool, error) {
		var ok bool
		err := tx.QueryRow(ctx, `SELECT to_regclass('public.' || $1) IS NOT NULL`, t).Scan(&ok)
		return ok, err
	}
	count := func(t string) (int64, error) {
		var n int64
		err := tx.QueryRow(ctx, fmt.Sprintf(`SELECT count(*) FROM %s`, pgx.Identifier{t}.Sanitize())).Scan(&n)
		return n, err
	}

	out := map[string]int64{}
	var trunc []string
	for _, t := range fleetTruncate {
		ok, err := exists(t)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		n, err := count(t)
		if err != nil {
			return nil, fmt.Errorf("count %s: %w", t, err)
		}
		out[t] = n
		trunc = append(trunc, pgx.Identifier{t}.Sanitize())
	}
	for _, t := range fleetDelete {
		ok, err := exists(t)
		if err != nil {
			return nil, err
		}
		if !ok {
			continue
		}
		n, err := count(t)
		if err != nil {
			return nil, fmt.Errorf("count %s: %w", t, err)
		}
		out[t] = n
	}
	if dryRun {
		return out, nil
	}

	if len(trunc) > 0 {
		if _, err := tx.Exec(ctx, `TRUNCATE `+strings.Join(trunc, ", ")); err != nil {
			return nil, fmt.Errorf("truncate: %w", err)
		}
	}
	for _, t := range fleetDelete {
		if _, ok := out[t]; !ok {
			continue
		}
		if _, err := tx.Exec(ctx, fmt.Sprintf(`DELETE FROM %s`, pgx.Identifier{t}.Sanitize())); err != nil {
			return nil, fmt.Errorf("delete %s: %w", t, err)
		}
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return out, nil
}
