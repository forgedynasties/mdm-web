# Alert Matrix

> Implementation reference for the lean T7 alert matrix. Companion to the
> [status scorecard](./alert-matrix-status.md) and [design rationale](./analytics-and-alerting.md).

Implementation reference for the T7 fleet alerting system. Brief but complete.

> **Scope note:** battery-health (degraded/critical/cycle-count), Wi-Fi disconnect
> counting, and the app/kiosk behavioural rules (`app_not_foreground`, `kiosk_disabled`,
> `app_crash`, `app_anr`) were removed to keep the set lean — the battery-health sysfs
> proxy wasn't trusted and the app/kiosk signals weren't actionable yet. The kiosk
> lock-task feature itself is untouched; the client just no longer reports its state.

## Architecture

- **Rules** live in `alert_rules` (type, enabled, JSONB params, `active_window`). Seeded once by `EnsureDefaultRules` from `defaultAlertRules` in `internal/db/db.go`; editable/disable-able in **Settings → Alerts**.
- **Alerts** (fired instances) live in `alerts` (severity `critical|warning|info`, status `open|acknowledged|resolved`). `CreateAlertIfAbsent` dedups; rules auto-resolve when the condition clears.
- **Two evaluation tiers**:
  - **Daily** — hourly, over `device_daily_stats` rollups (`EvaluateAlerts`).
  - **Recent** — every minute, over `latest_extra` / recent `checkins` (`EvaluateRecentAlerts`, `detectRecentRule`). Stale devices (silent > 15 min) are skipped.
- **Active windows** (`internal/db/db.go`, `ServiceWindow`): `always` | `service` (07:00–23:00) | `overnight` (23:30–06:00), timezone-aware, per-venue overridable. Windowed (`service`/`overnight`) rules are **operational → deployed units only** (`restaurant_id IS NOT NULL`); `always` rules fire fleet-wide.
- **Dashboard catalog**: `alertRuleDefs` in `internal/dashboard/handlers.go` (labels, descriptions, tunable fields, category, windowed/recent flags).

## Matrix

| # | Alert | Rule type | Tier | Sev | Window | Telemetry field |
|---|---|---|---|---|---|---|
| 1 | SoC low during service | `soc_low_service` | recent | crit | service | `battery_pct`, `charging` |
| 2 | SoC low charging guest | `soc_low_guest_charging` | recent | crit | service | `battery_pct`, `wlc_status` |
| 3 | Not charging overnight | `overnight_not_charging` | recent | crit | overnight | `battery_pct`, `charging` |
| 4 | Charging too slowly overnight | `overnight_slow_charge` | recent | crit | overnight | `battery_pct`, `charging` |
| 5 | Abnormal discharge — pad idle | `discharge_rate_idle` | recent | warn | service | `battery_pct`, `wlc_status` |
| 6 | Abnormal discharge — pad active | `discharge_rate_active` | recent | warn | service | `battery_pct`, `wlc_status` |
| 7 | Did not charge overnight | `no_overnight_charge` | daily | warn | always | `device_daily_stats.battery_max`, `charging_frac` |
| 8 | Pad disconnected | `pad_disconnected` | recent | warn | service | `wlc_status` |
| 9 | Pad never used all day | `pad_unused` | daily | info | always | `wlc_guest_frac` |
| 10 | Pad utilisation / shift | *(metric, no alert)* | daily | — | — | `device_daily_stats.wlc_guest_frac` |
| 11 | Overheating (>45°C) | `overheating` | daily+recent | crit | always | `battery_temp_c` |
| 12 | Temperature elevated | `temp_elevated` | recent | warn | always | `battery_temp_c` |
| 13 | Device offline | `offline` | recent | crit | always¹ | `last_seen_at` (heartbeat) |
| 14 | Weak Wi-Fi signal | `wifi_weak` | recent | warn | always | `wifi_rssi` |
| 15 | Storage critically low | `storage_low` | recent | crit | always | `storage_free_gb` |
| 16 | Storage filling fast | `storage_filling` | daily | warn | always | `storage_free_gb` |
| 17 | Unexpected reboot | `unexpected_reboot` | recent | warn | service | `uptime_seconds` (+ `boot_reason`) |
| 18 | Memory pressure | `memory_low` | recent | crit | always | `ram_usage_mb` |
| 19 | OS out of compliance | *(deferred)* | — | info | — | — → future **release tracking** |

Defaults: `wifi_weak` −75 dBm sustained 10 min, `storage_low` <0.5 GB, `memory_low` <400 MB avail, `unexpected_reboot` 30-min look-back. `memory_pressure` (daily `ram_pct`, seeded **disabled**) supplies the Daily Report's RAM cutoff; `memory_low` is the live alert.

## Client telemetry (`MdmService.buildCheckinPayload`)

Sent in checkin `extra`; no new manifest permissions (relies on system UID + platform signature):

- `wifi_rssi` — connected-link RSSI
- `boot_reason` — `sys.boot.reason` / `ro.boot.bootreason`

Core fields: `battery_pct`, `charging`, `battery_temp_c`, `wlc_status`, `storage_free_gb`, `ram_usage_mb`, `uptime_seconds`, `wifi`/`wifi_scan`, `ip_address`, `timezone`.

## Notes / caveats

- ¹ **`offline` is fleet-wide** (deployed *and* bench/lab units), not gated to service hours or deployment. It self-suppresses overnight via its own `quiet_start`/`quiet_end` window (default 00:00–06:00 local), so it doesn't use the `service` active window. `migrationSQL` flips legacy `service`-windowed rows to `always` on startup.
- **`boot_reason`** is reported but not yet surfaced in the `unexpected_reboot` alert detail (follow-up to classify user vs system reboots).
- **#19 OS compliance** intentionally deferred to release tracking (release string, not SDK).
- **Removed rules** (`battery_health_low`/`_critical`, `battery_cycles_high`, `battery_health_decline`, `wifi_disconnects`, `app_not_foreground`, `kiosk_disabled`, `app_crash`, `app_anr`) are purged from `alert_rules` on existing DBs by an idempotent `DELETE` in `migrationSQL`.
