package db

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"time"

	"github.com/google/uuid"
)

// EvaluateAlerts runs every enabled fleet-scoped rule against device_daily_stats,
// creating alerts for violators (deduped) and resolving alerts whose condition has
// cleared. Returns counts of created and resolved alerts. Called from housekeeping.
func (d *DB) EvaluateAlerts(ctx context.Context, connected []uuid.UUID) (created []AlertNotification, resolved int, err error) {
	rules, err := d.ListAlertRules(ctx, true)
	if err != nil {
		return nil, 0, err
	}
	deployed, err := d.deployedDeviceSet(ctx)
	if err != nil {
		return nil, 0, err
	}
	windows, err := d.effectiveWindows(ctx)
	if err != nil {
		return nil, 0, err
	}
	now := time.Now().UTC()
	for _, r := range rules {
		if r.ScopeType != "fleet" {
			continue // group/device scoping not implemented yet
		}
		if isRecentType(r.Type) {
			continue // owned by the recent tier (EvaluateRecentAlerts)
		}
		var p map[string]float64
		if len(r.Params) > 0 {
			_ = json.Unmarshal(r.Params, &p)
		}
		hits, severity, e := d.detectRule(ctx, r.Type, p, connected)
		if e != nil {
			return created, resolved, e
		}

		ids := make([]uuid.UUID, 0, len(hits))
		ruleID := r.ID
		for _, h := range hits {
			// Same deployed-only gate as the recent tier (see EvaluateRecentAlerts).
			if r.DeployedOnly && !deployed[h.DeviceID] {
				continue
			}
			ids = append(ids, h.DeviceID)
			sev := hitSeverity(now, r.Type, severity, h, windowFor(windows, h.DeviceID))
			u, e := d.upsertAlert(ctx, &ruleID, r.Type, h.DeviceID, sev, h.Summary, h.Detail)
			if e != nil {
				return created, resolved, e
			}
			if u.Inserted || u.Escalated {
				at, tz := notifyTimeFrom(h.Detail)
				created = append(created, AlertNotification{Type: r.Type, Severity: sev, Summary: h.Summary, Serial: h.Serial, DeviceID: h.DeviceID, EventAt: at, Timezone: tz,
					AlertID: u.ID, Escalated: u.Escalated, Detail: h.Detail})
			}
		}
		// The device stopped violating — start the clear window rather than resolving at
		// once, so a reading that dips below the line for one pass does not end the
		// episode and start a new one a minute later. See resolveClearedAlerts.
		tag, e := d.pool.Exec(ctx, `
			UPDATE alerts SET cleared_at = COALESCE(cleared_at, NOW()), updated_at = NOW()
			WHERE type = $1 AND status <> 'resolved' AND device_id <> ALL($2::uuid[])
			  AND cleared_at IS NULL
		`, r.Type, ids)
		if e != nil {
			return created, resolved, e
		}
		resolved += int(tag.RowsAffected())
	}
	return created, resolved, nil
}

// detectRule returns the devices currently violating a rule type plus the severity
// to record. Unknown rule types return no hits.
func (d *DB) detectRule(ctx context.Context, typ string, p map[string]float64, connected []uuid.UUID) ([]alertHit, string, error) {
	switch typ {
	case "offline":
		mins := int(param(p, "offline_minutes", 30))
		qs := int(param(p, "quiet_start", 0))
		qe := int(param(p, "quiet_end", 6))
		// A device with a live WebSocket is not offline even if last_seen_at is stale —
		// exclude the connected set so this matches the recent-tier rule and the
		// dashboard's WS-based online indicator (was inconsistent before).
		rows, err := d.pool.Query(ctx, `
			SELECT id, serial_number, last_seen_at, COALESCE(latest_extra->>'timezone', '')
			FROM devices
			WHERE NOT hidden AND last_seen_at < NOW() - ($1 * INTERVAL '1 minute')
			  -- Not offline: reporting to another MDM. See internal/db/custody.go.
			  AND custody_server = ''
			  AND id <> ALL($2::uuid[])
			  AND NOT EXISTS (SELECT 1 FROM alerts p WHERE p.device_id = devices.id
			    AND p.type = 'offline_peak' AND p.status <> 'resolved')`, mins, connected)
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		now := time.Now().UTC()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial, tz string
			var lastSeen time.Time
			if err := rows.Scan(&id, &serial, &lastSeen, &tz); err != nil {
				return nil, "critical", err
			}
			// Skip devices in their local quiet/overnight window (expected offline).
			localHour := localTime(now, tz).Hour()
			if inQuiet(localHour, qs, qe) {
				continue
			}
			down := int(now.Sub(lastSeen).Minutes())
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Offline — last check-in %dm ago", down),
				map[string]any{"offline_minutes": down, "last_seen": lastSeen, "timezone": tz}})
		}
		// One offline alert per device at warning severity — a plain outage isn't a
		// page. The genuinely urgent case (offline during peak service) is a separate
		// critical alert (offline_peak). offline_long is retired (redundant).
		return hits, "warning", rows.Err()

	case "memory_pressure":
		limit := param(p, "ram_pct", 85)
		rows, err := d.pool.Query(ctx, `
			SELECT s.device_id, dv.serial_number, s.ram_pct_peak
			FROM device_daily_stats s JOIN devices dv ON dv.id = s.device_id
			WHERE s.day = CURRENT_DATE AND s.ram_pct_peak >= $1`, limit)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var ram int
			if err := rows.Scan(&id, &serial, &ram); err != nil {
				return nil, "warning", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Peak RAM hit %d%% today (limit %.0f%%)", ram, limit),
				map[string]any{"ram_pct": ram, "limit_pct": limit}})
		}
		return hits, "warning", rows.Err()

	case "storage_filling":
		dropGB := param(p, "drop_gb", 0.2)
		floorGB := d.criticalStorageFloor(ctx)
		// Pure rate rule: free storage fell by more than drop_gb since yesterday. "How low"
		// is owned by storage_low / storage_warning; this one only watches how FAST it drops.
		// Skip devices already at/below the critical floor — storage_low owns those, so a
		// fast-dropping unit doesn't double-fire once it's also critically low.
		rows, err := d.pool.Query(ctx, `
			SELECT t.device_id, dv.serial_number, t.today, t.yday FROM (
				SELECT today.device_id,
				       today.storage_free_last_gb AS today,
				       yday.storage_free_last_gb  AS yday
				FROM device_daily_stats today
				LEFT JOIN device_daily_stats yday
				  ON yday.device_id = today.device_id AND yday.day = CURRENT_DATE - 1
				WHERE today.day = CURRENT_DATE
			) t JOIN devices dv ON dv.id = t.device_id
			WHERE t.today IS NOT NULL AND t.today >= $1
			  AND t.yday IS NOT NULL AND (t.yday - t.today) > $2`, floorGB, dropGB)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var today, yday float64
			if err := rows.Scan(&id, &serial, &today, &yday); err != nil {
				return nil, "warning", err
			}
			drop := yday - today
			summary := fmt.Sprintf("Storage dropped %.1f→%.1f GB in 24h", yday, today)
			// Store the measured 24h drop (not the threshold) so the humanized card shows
			// the real number, plus today's free level for context.
			hits = append(hits, alertHit{id, serial, summary,
				map[string]any{"today_gb": today, "drop_gb": drop}})
		}
		return hits, "warning", rows.Err()

	case "wlc_dead":
		// The wireless charger was never readable for the whole of the last complete day
		// (pad_readable=false), on a deployed unit whose pad WAS working within the prior
		// week — i.e. a genuine regression, not a device that simply has no pad. Daily tier.
		rows, err := d.pool.Query(ctx, `
			SELECT s.device_id, dv.serial_number
			FROM device_daily_stats s JOIN devices dv ON dv.id = s.device_id
			WHERE s.day = CURRENT_DATE - 1 AND s.pad_readable = false
			  AND NOT dv.hidden AND dv.restaurant_id IS NOT NULL
			  AND EXISTS (
			    SELECT 1 FROM device_daily_stats p
			    WHERE p.device_id = s.device_id
			      AND p.day >= CURRENT_DATE - 8 AND p.day < CURRENT_DATE - 1
			      AND p.pad_readable = true)`)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			if err := rows.Scan(&id, &serial); err != nil {
				return nil, "warning", err
			}
			hits = append(hits, alertHit{id, serial,
				"Wireless charger not functional all day (pad never readable)",
				map[string]any{"day": "yesterday"}})
		}
		return hits, "warning", rows.Err()
	}
	return nil, "warning", nil
}

// ── Recent-tier alert evaluation (Tier 5 §10) ───────────────────────────────────
//
// The daily evaluator (EvaluateAlerts) runs hourly over device_daily_stats and serves
// trend rules. Point-in-time and short-window rate/sustained rules from the T7 matrix
// need finer granularity, so they run here on a 1-minute ticker over devices.latest_extra
// and recent `checkins` rows. Both tiers feed the same `alerts` table and dedupe/resolve
// machinery; each rule type belongs to exactly one tier (recentRuleTypes below) so a rule
// is never evaluated — and spuriously resolved — by the wrong tier.

// recentRuleTypes is the set of rule types evaluated by the recent tier.
var recentRuleTypes = map[string]bool{
	"offline":            true,
	"offline_long":       true,
	"offline_peak":       true,
	"overheating":        true,
	"battery_low":        true,
	"storage_low":        true,
	"storage_warning":    true,
	"temp_elevated":      true,
	"memory_low":         true,
	"wifi_weak":          true,
	"wifi_unstable":      true,
	"battery_high_night": true,
	"wlc_continuous":     true,
	"charger_flapping":   true,
	"device_crash":       true,
	"slow_charge_night":  true,
	"host_app_away":      true,
	"compliance_failed":  true,
	"lost_device_seen":   true,
	"left_venue":         true,
	"lost_device_heard":  true,
	"self_locked":        true,
}

func isRecentType(typ string) bool { return recentRuleTypes[typ] }

// defaultActiveWindow is the window a recent rule is gated to when its active_window
// column is unset. Most rules are fleet-wide ("always"); a few are inherently tied to a
// time-of-day window (e.g. the overnight battery-high check, peak-hours offline).
func defaultActiveWindow(typ string) string {
	switch typ {
	case "battery_high_night", "slow_charge_night":
		return "overnight"
	case "offline_peak", "battery_low":
		return "peak"
	case "host_app_away", "left_venue":
		return "service"
	}
	return "always"
}

// EvaluateRecentAlerts runs every enabled recent-tier rule against current telemetry,
// gating each hit to the rule's active window (per-venue service hours), then creating
// and auto-resolving alerts exactly like EvaluateAlerts. Called from the 1-minute ticker.
// EvaluateRecentAlerts runs the recent-tier rules. `connected` is the set of
// device ids with a live WebSocket, used so the offline family doesn't page a
// device that is still connected (see offlineHitsQuery).
func (d *DB) EvaluateRecentAlerts(ctx context.Context, connected []uuid.UUID) (created []AlertNotification, resolved int, err error) {
	rules, err := d.ListAlertRules(ctx, true)
	if err != nil {
		return nil, 0, err
	}
	windows, err := d.effectiveWindows(ctx)
	if err != nil {
		return nil, 0, err
	}
	peaks, err := d.effectivePeakWindows(ctx)
	if err != nil {
		return nil, 0, err
	}
	deployed, err := d.deployedDeviceSet(ctx)
	if err != nil {
		return nil, 0, err
	}
	now := time.Now().UTC()
	for _, r := range rules {
		if !isRecentType(r.Type) || r.ScopeType != "fleet" {
			continue
		}
		var p map[string]float64
		if len(r.Params) > 0 {
			_ = json.Unmarshal(r.Params, &p)
		}
		// A once-a-night rule only queries when some venue is due its check, and only
		// that venue's devices can fire; see alert_nightly.go.
		var nightly *nightlyPlan
		if onceNightlyRules[r.Type] {
			pl := d.nightly.plan(r.ID, time.Duration(param(p, "window_hours", 2)*float64(time.Hour)), windows, now)
			nightly = &pl
		}
		var hits []alertHit
		var severity string
		if nightly == nil || len(nightly.due) > 0 {
			var e error
			hits, severity, e = d.detectRecentRule(ctx, r.Type, p, connected)
			if e != nil {
				return created, resolved, e
			}
		}
		aw := r.ActiveWindow
		if aw == "" {
			aw = defaultActiveWindow(r.Type)
		}
		windowed := aw == "service" || aw == "overnight" || aw == "peak"
		// A window-gated rule is operational by nature (it assumes a unit live in a
		// restaurant on a service/charge schedule), so it stays deployed-only whatever
		// the flag says. Everything else follows the rule's own deployed_only setting,
		// which defaults to on: a bench unit left on a pad, switched off or filling its
		// disk is expected, and nobody is on call for it.
		deployedOnly := windowed || r.DeployedOnly
		ids := make([]uuid.UUID, 0, len(hits))
		ruleID := r.ID
		for _, h := range hits {
			if deployedOnly && !deployed[h.DeviceID] {
				continue
			}
			if nightly != nil {
				rid := windowFor(windows, h.DeviceID).RestaurantID
				if rid == nil || nightly.due[*rid] == "" {
					continue
				}
			}
			// Skip devices outside the rule's active window; they auto-resolve below.
			// Peak windows are a separate list of ranges; service/overnight use the span.
			var inWin bool
			if aw == "peak" {
				inWin = inAnyPeak(now, windowFor(windows, h.DeviceID).TZ, peaks[h.DeviceID])
			} else {
				inWin = inActiveWindow(now, "", windowFor(windows, h.DeviceID), aw)
			}
			if !inWin {
				continue
			}
			ids = append(ids, h.DeviceID)
			sev := hitSeverity(now, r.Type, severity, h, windowFor(windows, h.DeviceID))
			u, e := d.upsertAlert(ctx, &ruleID, r.Type, h.DeviceID, sev, h.Summary, h.Detail)
			if e != nil {
				return created, resolved, e
			}
			if u.Inserted || u.Escalated {
				at, tz := notifyTimeFrom(h.Detail)
				created = append(created, AlertNotification{Type: r.Type, Severity: sev, Summary: h.Summary, Serial: h.Serial, DeviceID: h.DeviceID, EventAt: at, Timezone: tz,
					AlertID: u.ID, Escalated: u.Escalated, Detail: h.Detail})
			}
		}
		// Mid-night devices of venues not due this tick keep whatever tonight's check
		// found; only a check (or the night ending) clears them.
		if nightly != nil {
			ids = append(ids, nightly.keep...)
		}
		// Same clear-window treatment as the daily tier above.
		tag, e := d.pool.Exec(ctx, `
			UPDATE alerts SET cleared_at = COALESCE(cleared_at, NOW()), updated_at = NOW()
			WHERE type = $1 AND status <> 'resolved' AND device_id <> ALL($2::uuid[])
			  AND cleared_at IS NULL
		`, r.Type, ids)
		if e != nil {
			return created, resolved, e
		}
		resolved += int(tag.RowsAffected())
		if nightly != nil {
			d.nightly.mark(r.ID, nightly.due)
		}
	}
	// Close out anything whose clear window has now passed.
	// Logged, not returned: returning would drop this tick's new-alert notifications.
	// It was silently dropped before, which hid a query that failed every minute and
	// left every cleared alert open.
	if n, e := d.resolveClearedAlerts(ctx); e != nil {
		log.Printf("[alerts] resolve cleared: %v", e)
	} else {
		resolved += int(n)
	}
	return created, resolved, nil
}

// recentReportingCutoff bounds "current" telemetry: a device silent longer than this is
// handled by the offline rule, not the SoC/pad/storage rules (whose latest_extra is stale).
const recentReportingCutoff = "15 minutes"

// offlineHitsQuery finds devices offline longer than $1 minutes for the offline-family
// rule $2, EXCLUDING those already alerted for the current offline stretch. A resolved
// alert whose fired_at is at/after the device's last_seen_at means we alerted since the
// device was last seen — so it must not re-fire (even if an operator resolved it while
// still offline). last_seen_at only advances when the device comes back online, so a
// genuine recover-then-drop resets this and a fresh alert can fire. An OPEN alert is not
// 'resolved', so it stays in the hit set (deduped by CreateAlertIfAbsent, not re-notified).
const offlineHitsQuery = `
	SELECT d.id, d.serial_number, d.last_seen_at FROM devices d
	WHERE NOT d.hidden AND d.last_seen_at < NOW() - ($1 * INTERVAL '1 minute')
	  -- A device reporting to another MDM is silent here by design, not by fault. It
	  -- is not offline, and paging someone about it is how a real outage gets lost in
	  -- the noise of devices that were deliberately moved. See internal/db/custody.go.
	  AND d.custody_server = ''
	  -- A device with a live WebSocket is NOT offline, even if its last_seen_at has
	  -- gone stale — this keeps the alert consistent with the dashboard's WS-based
	  -- online indicator instead of paging a still-connected device. $3 is the set of
	  -- currently-connected device ids (empty array = nobody connected → excludes none).
	  AND d.id <> ALL($3::uuid[])
	  AND NOT EXISTS (
	    SELECT 1 FROM alerts a
	    WHERE a.device_id = d.id AND a.type = $2 AND a.status = 'resolved'
	      AND a.fired_at >= d.last_seen_at
	  )`

// offlineNotPeakClause extends offlineHitsQuery for plain "offline": a device already
// covered by an open "offline during peak" alert is left out.
const offlineNotPeakClause = `
	  AND NOT EXISTS (
	    SELECT 1 FROM alerts p
	    WHERE p.device_id = d.id AND p.type = 'offline_peak' AND p.status <> 'resolved'
	  )`

// detectRecentRule returns devices currently violating a recent-tier rule. Window gating
// is applied by the caller (EvaluateRecentAlerts).
func (d *DB) detectRecentRule(ctx context.Context, typ string, p map[string]float64, connected []uuid.UUID) ([]alertHit, string, error) {
	switch typ {
	case "offline":
		mins := int(param(p, "offline_minutes", 5))
		// One outage is one alert: while "offline during peak" is open for a device,
		// plain offline stays quiet instead of doubling up on it.
		rows, err := d.pool.Query(ctx, offlineHitsQuery+offlineNotPeakClause, mins, "offline", connected)
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		now := time.Now().UTC()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var last time.Time
			if err := rows.Scan(&id, &serial, &last); err != nil {
				return nil, "critical", err
			}
			down := int(now.Sub(last).Minutes())
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Offline — last check-in %dm ago", down),
				map[string]any{"offline_minutes": down, "last_seen": last}})
		}
		// A plain outage is a warning, not a page; offline_peak stays critical.
		return hits, "warning", rows.Err()

	case "storage_low":
		freeGB := param(p, "free_gb", 0.5)
		rows, err := d.pool.Query(ctx, `
			SELECT d.id, d.serial_number, (d.latest_extra->>'storage_free_gb')::numeric
			FROM devices d
			WHERE NOT d.hidden AND d.last_seen_at > NOW() - INTERVAL '`+recentReportingCutoff+`'
			  AND (d.latest_extra->>'storage_free_gb')::numeric < $1`, freeGB)
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var free float64
			if err := rows.Scan(&id, &serial, &free); err != nil {
				return nil, "critical", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Only %.0f MB storage free", free*1024),
				map[string]any{"storage_free_gb": free, "limit_gb": freeGB}})
		}
		return hits, "critical", rows.Err()

	case "overheating":
		// WLC-aware dual threshold: a device actively wireless-charging (wlc_status='1')
		// runs hotter by design, so it only alerts at the higher limit; off the pad the
		// lower limit applies. Point-in-time on the latest reading, so the alert lands
		// within ~1 min of the spike and auto-resolves once it cools.
		limit := param(p, "temp_c", 52)        // off-pad limit (the T7 runs 41-48 °C every day)
		limitWLC := param(p, "temp_c_wlc", 65) // on-pad (wireless charging) limit
		rows, err := d.pool.Query(ctx, `
			SELECT d.id, d.serial_number, (d.latest_extra->>'battery_temp_c')::numeric,
			       d.last_seen_at, d.latest_extra->>'timezone',
			       -- COALESCE, not a bare comparison: a device that reports no wlc_status at
		       -- all (a DPC-managed device, or a T7 before the pad is read) yields NULL
		       -- here, and scanning NULL into a bool killed the whole evaluation pass —
		       -- every rule, not just this one.
		       (COALESCE(d.latest_extra->>'wlc_status', '') = '1') AS on_wlc
			FROM devices d
			WHERE NOT d.hidden AND d.last_seen_at > NOW() - INTERVAL '`+recentReportingCutoff+`'
			  AND (
			    (d.latest_extra->>'wlc_status' = '1' AND (d.latest_extra->>'battery_temp_c')::numeric >= $2)
			    OR
			    (COALESCE(d.latest_extra->>'wlc_status','') <> '1' AND (d.latest_extra->>'battery_temp_c')::numeric >= $1)
			  )`, limit, limitWLC)
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var temp float64
			var seen time.Time
			var tz *string
			var onWLC bool
			if err := rows.Scan(&id, &serial, &temp, &seen, &tz, &onWLC); err != nil {
				return nil, "critical", err
			}
			lim := limit
			if onWLC {
				lim = limitWLC
			}
			detail := map[string]any{"temp_c": temp, "limit_c": lim, "on_wlc": onWLC, "event_at": seen}
			if tz != nil && *tz != "" {
				detail["timezone"] = *tz
			}
			ctx := "off pad"
			if onWLC {
				ctx = "on wireless charger"
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Device temperature is %.0f°C %s (limit %.0f°C)", temp, ctx, lim),
				detail})
		}
		return hits, "critical", rows.Err()

	case "temp_elevated":
		lo := param(p, "temp_min", 38)
		hi := param(p, "temp_max", 45)
		// Sustained: every reading in the last ~20 min is in [lo,hi) and spans ≥14 min.
		// Reads device_samples, not checkins: the same readings in fixed columns, so
		// this runs over a narrow table instead of detoasting a jsonb snapshot per row
		// — every minute, for the whole fleet. Samples are written under the same
		// condition as history rows, so the window holds the same readings.
		rows, err := d.pool.Query(ctx, `
			SELECT c.device_id, dv.serial_number, MAX(c.temp) FROM (
				SELECT device_id, temp_c::numeric AS temp, at AS created_at
				FROM device_samples WHERE at > NOW() - INTERVAL '20 minutes'
			) c JOIN devices dv ON dv.id = c.device_id
			WHERE c.temp IS NOT NULL
			GROUP BY c.device_id, dv.serial_number
			HAVING MIN(c.temp) >= $1 AND MAX(c.temp) < $2
			   AND (MAX(c.created_at) - MIN(c.created_at)) >= INTERVAL '14 minutes'`, lo, hi)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var tmax float64
			if err := rows.Scan(&id, &serial, &tmax); err != nil {
				return nil, "warning", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Device held %.0f–%.0f°C for >15 min (peak %.0f°C)", lo, hi, tmax),
				map[string]any{"temp_min": lo, "temp_max": hi, "peak_c": tmax}})
		}
		return hits, "warning", rows.Err()

	case "memory_low":
		availMB := param(p, "avail_mb", 400)
		// Sustained low available RAM (total-used) over the last ~10 min: even the
		// highest reading in the window stays below the floor. Always-on hardware rule.
		// device_samples keeps used and total as they were reported, so the available
		// figure is the same arithmetic on the same numbers, off a narrow table.
		rows, err := d.pool.Query(ctx, `
			SELECT c.device_id, dv.serial_number, MAX(c.avail) FROM (
				SELECT device_id, (ram_total_mb - ram_used_mb)::numeric AS avail, at AS created_at
				FROM device_samples WHERE at > NOW() - INTERVAL '12 minutes'
			) c JOIN devices dv ON dv.id = c.device_id
			WHERE c.avail IS NOT NULL AND NOT dv.hidden
			GROUP BY c.device_id, dv.serial_number
			HAVING MAX(c.avail) < $1 AND (MAX(c.created_at) - MIN(c.created_at)) >= INTERVAL '8 minutes'`, availMB)
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var avail float64
			if err := rows.Scan(&id, &serial, &avail); err != nil {
				return nil, "critical", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Available RAM held under %.0f MB for >8 min (peak %.0f MB)", availMB, avail),
				map[string]any{"avail_mb": avail, "limit_mb": availMB}})
		}
		return hits, "critical", rows.Err()

	case "wifi_weak":
		thr := param(p, "rssi_dbm", -75)
		sustain := param(p, "sustain_min", 10)
		// Sustained weak connected-link RSSI: even the strongest reading in the window
		// stays below (more negative than) the floor, spanning ≥ sustain_min. RSSI is the
		// connected AP's signal (extra.wifi_rssi), not the scan list. Always-on rule.
		rows, err := d.pool.Query(ctx, `
			SELECT c.device_id, dv.serial_number, MAX(c.rssi) FROM (
				SELECT device_id, wifi_rssi::numeric AS rssi, at AS created_at
				FROM device_samples WHERE at > NOW() - INTERVAL '15 minutes'
			) c JOIN devices dv ON dv.id = c.device_id
			WHERE c.rssi IS NOT NULL AND NOT dv.hidden
			GROUP BY c.device_id, dv.serial_number
			HAVING MAX(c.rssi) < $1 AND (MAX(c.created_at) - MIN(c.created_at)) >= ($2 * INTERVAL '1 minute')`, thr, sustain)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var best float64
			if err := rows.Scan(&id, &serial, &best); err != nil {
				return nil, "warning", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Wi-Fi signal held below %.0f dBm for >%.0f min (best %.0f dBm)", thr, sustain, best),
				map[string]any{"rssi_dbm": best, "limit_dbm": thr}})
		}
		return hits, "warning", rows.Err()

	case "storage_warning":
		// Point-in-time warning band: free storage at/under warn_gb but still above the
		// critical floor, so a low disk warns once and only escalates to critical when it
		// gets dire. The floor is the critical storage_low rule's own trigger — the warning
		// hands off to it automatically, so there's no separate floor setting to keep in sync.
		warnGB := param(p, "free_gb", 14)
		floorGB := d.criticalStorageFloor(ctx)
		rows, err := d.pool.Query(ctx, `
			SELECT d.id, d.serial_number, (d.latest_extra->>'storage_free_gb')::numeric
			FROM devices d
			WHERE NOT d.hidden AND d.last_seen_at > NOW() - INTERVAL '`+recentReportingCutoff+`'
			  AND (d.latest_extra->>'storage_free_gb')::numeric <= $1
			  AND (d.latest_extra->>'storage_free_gb')::numeric >  $2`, warnGB, floorGB)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var free float64
			if err := rows.Scan(&id, &serial, &free); err != nil {
				return nil, "warning", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Storage low: %.1f GB free (warn ≤ %.0f GB)", free, warnGB),
				map[string]any{"storage_free_gb": free, "limit_gb": warnGB}})
		}
		return hits, "warning", rows.Err()

	case "charger_flapping":
		// Faulty charger/dock: the charging state oscillates true↔false as the
		// connection drops in and out. Flag devices toggling faster than the
		// per-minute rate over the window. Excludes hidden devices.
		windowMin := int(param(p, "window_min", 5))
		flapsPerMin := int(param(p, "flaps_per_min", 10))
		flapping, err := d.FlappingChargers(ctx, windowMin, flapsPerMin)
		if err != nil {
			return nil, "warning", err
		}
		if len(flapping) == 0 {
			return nil, "warning", nil
		}
		ids := make([]uuid.UUID, 0, len(flapping))
		for id := range flapping {
			ids = append(ids, id)
		}
		rows, err := d.pool.Query(ctx, `
			SELECT id, serial_number FROM devices WHERE id = ANY($1) AND NOT hidden`, ids)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			if err := rows.Scan(&id, &serial); err != nil {
				return nil, "warning", err
			}
			rate := flapping[id]
			hits = append(hits, alertHit{id, serial,
				"Faulty charger — charging keeps dropping in and out; use a genuine charger",
				map[string]any{"rate": rate, "window_min": windowMin}})
		}
		return hits, "warning", rows.Err()

	case "battery_high_night":
		// Point-in-time battery SoC at/above the threshold. Overnight-windowed +
		// deployed-only (gated by the caller), so it flags venue units sitting high on the
		// charger overnight rather than cycling down.
		pct := param(p, "soc_pct", 60)
		rows, err := d.pool.Query(ctx, `
			SELECT d.id, d.serial_number, d.latest_battery_pct
			FROM devices d
			WHERE NOT d.hidden AND d.last_seen_at > NOW() - INTERVAL '`+recentReportingCutoff+`'
			  AND d.latest_battery_pct >= $1`, int(pct))
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var batt int
			if err := rows.Scan(&id, &serial, &batt); err != nil {
				return nil, "warning", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Battery at %d%% overnight (≥ %.0f%%)", batt, pct),
				map[string]any{"battery_pct": batt, "limit_pct": pct}})
		}
		return hits, "warning", rows.Err()

	case "wlc_continuous":
		// Sustained: every reading in the last ~(sustain+5) min is on the wireless
		// charger (wlc_status='1') and the run spans ≥ sustain_min — i.e. the device has
		// been continuously wireless-charging for over an hour (heat/battery stress). A
		// gap or any non-'1'/missing reading breaks continuity (bool_and over NULL fails).
		sustain := param(p, "sustain_min", 60)
		// Read from the shaped tables, which answer the two halves of this separately:
		// device_samples says the device was actually reporting across the window, and
		// device_state_events says the pad state never left '1'. Both are needed — the
		// state alone would fire on a device that went dark an hour ago still carrying
		// its last known state, which is the opposite of continuous charging.
		rows, err := d.pool.Query(ctx, `
			-- The window start is written inline, not as a one-row CTE: a CTE hides it
			-- from the planner, which then walked every row of device_samples (6.3M on
			-- live) to keep the last hour's. The state lookups go per device through
			-- the (device_id, key, at) primary key rather than scanning all history.
			WITH pres AS (
				SELECT device_id, MIN(at) AS mn, MAX(at) AS mx
				FROM device_samples WHERE at > NOW() - (($1 + 5) * INTERVAL '1 minute')
				GROUP BY device_id
			)
			SELECT p.device_id, dv.serial_number,
			       EXTRACT(EPOCH FROM (p.mx - p.mn))/60 AS span_min
			FROM pres p
			JOIN devices dv ON dv.id = p.device_id
			-- The pad state as the window opened. A device whose first ever wlc_status
			-- event falls inside the window has no state here and cannot qualify, which
			-- is the conservative reading: we do not know what it was doing before.
			LEFT JOIN LATERAL (
				SELECT e.to_val FROM device_state_events e
				WHERE e.device_id = p.device_id AND e.key = 'wlc_status'
				  AND e.at <= NOW() - (($1 + 5) * INTERVAL '1 minute')
				ORDER BY e.at DESC LIMIT 1
			) a ON true
			WHERE NOT dv.hidden
			  AND COALESCE(a.to_val, '') = '1'
			  -- Any change inside the window that moved it off the pad breaks continuity.
			  AND NOT EXISTS (
				SELECT 1 FROM device_state_events e
				WHERE e.device_id = p.device_id AND e.key = 'wlc_status'
				  AND e.at > NOW() - (($1 + 5) * INTERVAL '1 minute')
				  AND e.to_val IS DISTINCT FROM '1')
			  AND (p.mx - p.mn) >= ($1 * INTERVAL '1 minute')`, sustain)
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var span float64
			if err := rows.Scan(&id, &serial, &span); err != nil {
				return nil, "critical", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("On wireless charger continuously for %.0f min (≥ %.0f)", span, sustain),
				map[string]any{"span_min": span, "limit_min": sustain}})
		}
		return hits, "critical", rows.Err()

	case "offline_long":
		// Retired: it duplicated the `offline` alert (one device raised both). `offline`
		// is now the single per-device outage alert. Returning no hits means the eval loop
		// auto-resolves any lingering open offline_long alerts on the next pass.
		return nil, "warning", nil

	case "battery_low":
		// Point-in-time low SoC. Peak-windowed + deployed-only (gated by the caller), so
		// it flags a venue unit running low during the busy lunch/dinner rush.
		pct := param(p, "soc_pct", 20)
		rows, err := d.pool.Query(ctx, `
			SELECT d.id, d.serial_number, d.latest_battery_pct
			FROM devices d
			WHERE NOT d.hidden AND d.last_seen_at > NOW() - INTERVAL '`+recentReportingCutoff+`'
			  AND d.latest_battery_pct < $1`, int(pct))
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var batt int
			if err := rows.Scan(&id, &serial, &batt); err != nil {
				return nil, "critical", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Battery at %d%% during peak hours (< %.0f%%)", batt, pct),
				map[string]any{"battery_pct": batt, "limit_pct": pct}})
		}
		return hits, "critical", rows.Err()

	case "offline_peak":
		// Offline during peak hours (peak-windowed + deployed-only via the caller). Shorter
		// fuse than the anytime rules because an outage mid-service is urgent.
		mins := int(param(p, "offline_minutes", 5))
		rows, err := d.pool.Query(ctx, offlineHitsQuery, mins, "offline_peak", connected)
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		now := time.Now().UTC()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var last time.Time
			if err := rows.Scan(&id, &serial, &last); err != nil {
				return nil, "critical", err
			}
			down := int(now.Sub(last).Minutes())
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Offline %dm during peak hours", down),
				map[string]any{"offline_minutes": down, "last_seen": last}})
		}
		return hits, "critical", rows.Err()

	case "wifi_unstable":
		// Point-in-time on the client-reported rolling count of Wi-Fi disconnects in the
		// last hour (extra.wifi_disconnects_1h).
		limit := param(p, "disconnects", 3)
		rows, err := d.pool.Query(ctx, `
			SELECT d.id, d.serial_number, (d.latest_extra->>'wifi_disconnects_1h')::numeric
			FROM devices d
			WHERE NOT d.hidden AND d.last_seen_at > NOW() - INTERVAL '`+recentReportingCutoff+`'
			  AND (d.latest_extra->>'wifi_disconnects_1h')::numeric >= $1`, limit)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var n float64
			if err := rows.Scan(&id, &serial, &n); err != nil {
				return nil, "warning", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("%.0f Wi-Fi disconnects in the last hour (≥ %.0f)", n, limit),
				map[string]any{"disconnects_1h": n, "limit": limit}})
		}
		return hits, "warning", rows.Err()

	case "host_app_away":
		return d.detectHostAppAway(ctx, p, connected)
	case "compliance_failed":
		return d.detectComplianceFailed(ctx, p)
	case "lost_device_seen":
		return d.detectLostDeviceSeen(ctx, connected)
	case "left_venue":
		return d.detectLeftVenue(ctx, p, connected)
	case "lost_device_heard":
		return d.detectLostDeviceHeard(ctx, p, connected)
	case "self_locked":
		return d.detectSelfLocked(ctx)

	case "device_crash":
		// A crash/ANR/tombstone was reported (device_events, from the client's DropBox
		// reader) within the window. Auto-resolves once no crash is seen for that long.
		mins := int(param(p, "window_min", 15))
		rows, err := d.pool.Query(ctx, `
			SELECT d.id, d.serial_number, COUNT(*), MAX(e.occurred_at),
			       (array_agg(e.kind ORDER BY e.occurred_at DESC))[1],
			       (array_agg(e.summary ORDER BY e.occurred_at DESC))[1]
			FROM devices d JOIN device_events e ON e.device_id = d.id
			WHERE NOT d.hidden AND e.kind NOT IN ('reboot', 'kiosk_exit_offline', 'offline_dropped')
			  AND NOT e.late -- a crash that happened while the device was offline is history, not a page
			  AND e.occurred_at > NOW() - ($1 * INTERVAL '1 minute')
			GROUP BY d.id, d.serial_number`, mins)
		if err != nil {
			return nil, "critical", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial, kind, summary string
			var n int
			var at time.Time
			if err := rows.Scan(&id, &serial, &n, &at, &kind, &summary); err != nil {
				return nil, "critical", err
			}
			label := fmt.Sprintf("%d crash(es) in %dm — latest %s", n, mins, kind)
			hits = append(hits, alertHit{id, serial, label,
				map[string]any{"count": n, "kind": kind, "summary": summary, "event_at": at}})
		}
		return hits, "critical", rows.Err()

	case "slow_charge_night":
		// Over the last window_hours the device was continuously charging yet its
		// battery barely rose (≤ max_gain_pct) and ended below below_pct — a stalled
		// / trickle charge that won't be ready by morning. Overnight-windowed.
		maxGain := param(p, "max_gain_pct", 15)
		windowH := param(p, "window_hours", 2)
		// Charging slows down on purpose as a battery fills, so a small gain near the
		// top is normal: 116 of 130 alerts in the 30 Sep review ended at 80% or more.
		belowPct := param(p, "below_pct", 80)
		rows, err := d.pool.Query(ctx, `
			-- Window start inline and state lookups per device, for the same reasons
			-- as wlc_continuous above: this ran 3.8s a minute on live as a CTE.
			WITH s AS (
				SELECT device_id,
				       (array_agg(battery_pct ORDER BY at ASC))[1]  AS first_batt,
				       (array_agg(battery_pct ORDER BY at DESC))[1] AS last_batt,
				       COUNT(*) AS n,
				       (MAX(at) - MIN(at)) AS span
				FROM device_samples
				WHERE at > NOW() - ($1 * INTERVAL '1 hour')
				GROUP BY device_id
			)
			SELECT s.device_id, dv.serial_number, s.first_batt, s.last_batt
			FROM s
			JOIN devices dv ON dv.id = s.device_id
			-- "Continuously charging" is now the charging state as the window opened,
			-- with no event since that left it. A device with no charging event at or
			-- before the window cannot qualify: the old query treated an unconfirmed
			-- reading as not charging, and an unknown state is the same claim.
			LEFT JOIN LATERAL (
				SELECT e.to_val FROM device_state_events e
				WHERE e.device_id = s.device_id AND e.key = 'charging'
				  AND e.at <= NOW() - ($1 * INTERVAL '1 hour')
				ORDER BY e.at DESC LIMIT 1
			) a ON true
			WHERE NOT dv.hidden AND s.n >= 3
			  AND COALESCE(a.to_val, '') = 'true'
			  AND NOT EXISTS (
				SELECT 1 FROM device_state_events e
				WHERE e.device_id = s.device_id AND e.key = 'charging'
				  AND e.at > NOW() - ($1 * INTERVAL '1 hour')
				  AND e.to_val IS DISTINCT FROM 'true')
			  AND s.span >= (($1 - 0.25) * INTERVAL '1 hour')
			  AND (s.last_batt - s.first_batt) <= $2
			  AND s.last_batt < $3`, windowH, maxGain, belowPct)
		if err != nil {
			return nil, "warning", err
		}
		defer rows.Close()
		var hits []alertHit
		for rows.Next() {
			var id uuid.UUID
			var serial string
			var first, last int
			if err := rows.Scan(&id, &serial, &first, &last); err != nil {
				return nil, "warning", err
			}
			hits = append(hits, alertHit{id, serial,
				fmt.Sprintf("Charging for %.0fh but battery only went %d%%→%d%%", windowH, first, last),
				map[string]any{"gain_pct": last - first, "first_pct": first, "last_pct": last}})
		}
		// A warning, not critical: it fires at night, when nobody can act on it.
		return hits, "warning", rows.Err()
	}
	return nil, "warning", nil
}
