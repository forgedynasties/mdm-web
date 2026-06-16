# Alert Matrix

> Implementation reference for the 26-alert T7 matrix. Companion to the
> [status scorecard](./alert-matrix-status.md) and [design rationale](./analytics-and-alerting.md).

Implementation reference for the T7 fleet alerting system. Brief but complete.

## Architecture

- **Rules** live in `alert_rules` (type, enabled, JSONB params, `active_window`). Seeded once by `EnsureDefaultRules` from `defaultAlertRules` in `internal/db/db.go`; editable/disable-able in **Settings → Alerts**.
- **Alerts** (fired instances) live in `alerts` (severity `critical|warning|info`, status `open|acknowledged|resolved`). `CreateAlertIfAbsent` dedups; rules auto-resolve when the condition clears.
- **Two evaluation tiers**:
  - **Daily** — hourly, over `device_daily_stats` rollups (`EvaluateAlerts`).
  - **Recent** — every minute, over `latest_extra` / recent `checkins` (`EvaluateRecentAlerts`, `detectRecentRule`). Stale devices (silent > 15 min) are skipped.
- **Active windows** (`internal/db/db.go`, `ServiceWindow`): `always` | `service` (07:00–23:00) | `overnight` (23:30–06:00), timezone-aware, per-venue overridable. Windowed (`service`/`overnight`) rules are **operational → deployed units only** (`restaurant_id IS NOT NULL`); `always` rules fire fleet-wide.
- **Dashboard catalog**: `alertRuleDefs` in `internal/dashboard/handlers.go` (labels, descriptions, tunable fields, category, windowed/recent flags).

## Matrix (26 alerts)

| # | Alert | Rule type | Tier | Sev | Window | Telemetry field |
|---|---|---|---|---|---|---|
| 1 | SoC low during service | `soc_low_service` | recent | crit | service | `battery_pct`, `charging` |
| 2 | SoC low charging guest | `soc_low_guest_charging` | recent | crit | service | `battery_pct`, `wlc_status` |
| 3 | Not charging overnight | `overnight_not_charging` | recent | crit | overnight | `battery_pct`, `charging` |
| 4 | Charging too slowly overnight | `overnight_slow_charge` | recent | crit | overnight | `battery_pct`, `charging` |
| 5 | Abnormal discharge — pad idle | `discharge_rate_idle` | recent | warn | service | `battery_pct`, `wlc_status` |
| 6 | Abnormal discharge — pad active | `discharge_rate_active` | recent | warn | service | `battery_pct`, `wlc_status` |
| 7 | Battery health degraded (<85%) | `battery_health_low` | recent | warn | always | `battery_health_pct` |
| 8 | Battery health critical (<80%) | `battery_health_critical` | recent | crit | always | `battery_health_pct` |
| 9 | High charge cycle count | `battery_cycles_high` | recent | info | always | `battery_cycle_count` |
| 10 | Pad disconnected | `pad_disconnected` | recent | warn | service | `wlc_status` |
| 11 | Pad never used all day | `pad_unused` | daily | info | always | `wlc_guest_frac` |
| 12 | Pad utilisation / shift | *(metric, no alert)* | daily | — | — | `device_daily_stats.wlc_guest_frac` |
| 13 | Overheating (>45°C) | `overheating` | daily | crit | always | `battery_temp_c` |
| 14 | Temperature elevated | `temp_elevated` | recent | warn | always | `battery_temp_c` |
| 15 | Device offline | `offline` | recent | crit | service | `last_seen_at` (heartbeat) |
| 16 | Frequent Wi-Fi disconnects | `wifi_disconnects` | recent | warn | always | `wifi_disconnects_1h` |
| 17 | Weak Wi-Fi signal | `wifi_weak` | recent | warn | always | `wifi_rssi` |
| 18 | Ordering app not foreground | `app_not_foreground` | recent | crit | service | `foreground_pkg`, `kiosk_*` |
| 19 | Repeated app crashes | `app_crash` | recent | warn | service | `crash_count_4h` |
| 20 | Kiosk mode disabled | `kiosk_disabled` | recent | warn | always | `kiosk_active`, `kiosk_expected` |
| 21 | App not responding (ANR) | `app_anr` | recent | warn | always | `anr_count_4h` |
| 22 | Storage critically low | `storage_low` | recent | crit | always | `storage_free_gb` |
| 23 | Storage filling fast | `storage_filling` | daily | warn | always | `storage_free_gb` |
| 24 | Unexpected reboot | `unexpected_reboot` | recent | warn | service | `uptime_seconds` (+ `boot_reason`) |
| 25 | OS out of compliance | *(deferred)* | — | info | — | — → future **release tracking** |
| 26 | Memory pressure | `memory_low` | recent | crit | always | `ram_usage_mb` |

Defaults: `battery_health_low` warn/crit 85/80%, `battery_cycles_high` 400, `wifi_disconnects` >3/h, `wifi_weak` −75 dBm sustained 10 min, `app_crash` >2/4h, `app_anr` ≥1/4h. `memory_pressure` (ram_pct) is seeded **disabled** (superseded by `memory_low`).

## Client telemetry (`MdmService.buildCheckinPayload`)

Sent in checkin `extra`; no new manifest permissions (relies on system UID + platform signature):

- `wifi_rssi` — connected-link RSSI
- `battery_health_pct`, `battery_cycle_count` — `/sys/class/power_supply/battery/{charge_full,charge_full_design,cycle_count}` (`null` if unreadable)
- `foreground_pkg` — top package (`ActivityManager.getRunningTasks`)
- `kiosk_active` (`getLockTaskModeState`), `kiosk_expected` + `kiosk_package` (saved `KioskManager` config)
- `crash_count_4h`, `anr_count_4h` — `ApplicationExitInfo` for the pinned app
- `wifi_disconnects_1h` — `NetworkCallback.onLost` counted over a trailing hour
- `boot_reason` — `sys.boot.reason` / `ro.boot.bootreason`

Pre-existing: `battery_pct`, `charging`, `battery_temp_c`, `wlc_status`, `storage_free_gb`, `ram_usage_mb`, `uptime_seconds`, `wifi`/`wifi_scan`, `timezone`.

## Notes / caveats

- **`offline` on existing DBs**: seed is enabled now, but `EnsureDefaultRules` only inserts when absent — pre-existing rows need a one-time enable in Settings.
- **`app_not_foreground`** is point-in-time (latest checkin), not a strict 2-min sustain; auto-resolves on the next in-foreground checkin.
- **Battery sysfs paths** are device-specific; adjust `populateBatteryLifecycle` if T7 differs.
- **`boot_reason`** is reported but not yet surfaced in the `unexpected_reboot` alert detail (follow-up to classify user vs system reboots).
- **#25 OS compliance** intentionally deferred to release tracking (release string, not SDK).
