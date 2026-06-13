# Analytics, Alerting & Data-Driven Decisions

> **Status:** Checkpoint / living design doc. This is the working plan for turning MDM
> telemetry into decisions, alerts, and trends. We add to this as we go — it is the
> source of truth for the analytics program, not a one-time writeup.
>
> **Last updated:** 2026-06-13

---

## 0. Context

**Who:** AIO ships tableside ordering + ad devices to restaurants. Devices are used
during service hours and docked/charged overnight (mostly wireless charging pads).

**The job of a device:** take tableside orders and show ads. So the single business
question that drives everything here is:

> **Is each device available and usable during that restaurant's service hours?**

Every other signal (battery, charging, temperature, RAM, firmware) is a *leading
indicator* of that one failure mode. A dead tablet at 7pm Friday = lost orders + an
unhappy restaurant.

### The ML question — answered

**We do not need ML to start, and should not start there.** Device behavior is highly
predictable (daily use + overnight charge cycle). When behavior is this regular, simple
baselines and threshold rules beat ML on accuracy, explainability, and effort.

ML earns its place later for exactly one problem — **predicting battery failure weeks
out** — and only after 6–12 months of per-device history exists. Even then a simple
heuristic (overnight-recovery % trend crossing a line) captures most of the value.
Statistical anomaly detection (z-score vs. a device's own 30-day baseline) is "stats,"
not "ML," and is plenty for Tier 2/3.

**Verdict:** Rules + descriptive time-series first. Revisit ML in ~1 year. See Tier 4.

---

## 1. Data we already collect (raw material)

Devices check in every ~60s (configurable, `checkin_interval_sec`, default 60). Each
checkin is one row in `checkins` with columns + a `extra` JSONB payload. The latest
payload is denormalized onto `devices.latest_extra`.

| Signal | Field | Decision value |
|---|---|---|
| Battery % | `battery_pct` (column) | Charge level, overnight recovery, mid-shift drain |
| Charging state | `extra.charging`, `extra.wlc_status` (0=not,1=charging,2=pad disconnected) | Is it actually docked? |
| Battery temp | `extra.battery_temp_c` | Overheating on the charging pad |
| RAM | `extra.ram_usage_mb {used,total}` | Memory pressure → crashes/reboots |
| Firmware | `build_id` (column) | Version fragmentation across fleet |
| Apps | `installed_apps[] {package,name,version_name}` → `device_packages` | Ordering-app version drift |
| Connectivity | `last_seen_at` (column) | **Down during service = lost orders** |
| Location | `extra.latitude/longitude/location_accuracy`, `extra.wifi_scan` | Device moved / left venue |
| Timezone | `extra.timezone` | Local service-hour windows |

**Grouping:** `groups` / `device_groups` roll all of the above up per restaurant or chain.

### Key references
- Schema: `server/internal/db/db.go` (~2032–2265, single `migrationSQL` string).
- Checkin struct: `server/internal/api/handlers.go:312` (`checkinRequest`).
- `extra` keys populated: `server/internal/geolocate/geolocate.go:219`.
- Config defaults: `server/internal/config/config.go:95` (interval), retention/auto-hide nearby.
- Existing aggregates: `GetSummary()` `db.go:330`, `TableStats()` `db.go:2021`,
  `CountDevices()` `db.go:369`, CSV export `dashboard/handlers.go:1615`,
  fleet SSE `dashboard/handlers.go:1179`.

### What's unused today
We store full per-device history in `checkins` but currently surface only the **latest**
value. The time series is the unmined asset.

---

## 2. Tiered roadmap (build in this order)

### Tier 1 — Descriptive (≈80% already exists) — **rollup pipeline landed**
Extend `GetSummary()`-style snapshots from "right now" to "over time" via per-device and
per-group **daily rollups**. Foundation for everything below.

**Done (branch `analytics-alerting`):**
- `device_daily_stats` table (per device per day): checkin_count, battery min/max/avg,
  temp_max, ram_pct_peak, charging_frac, online_minutes, build_id, first/last_seen.
- `RollupDailyStats(day)` (idempotent upsert) + `BackfillDailyStats()` in `db.go`.
- Wired: rollup runs at the top of `RunHousekeeping` (today + yesterday, before prune);
  one-time backfill at startup in `main.go`.
- Read side: `GetDeviceDailyStats(deviceID, days)` + `GET /devices/{serial}/daily-stats`
  JSON endpoint.
- UI: **Trends** tab on device detail — 30/7/90-day Chart.js line chart (battery min–max
  band + avg, charging-coverage %).
- Per-group (groups treated as generic device buckets): `GetGroupDailyStats(groupID, days)`
  + `GET /groups/{id}/daily-stats` JSON + a **Group trends** chart on the group page
  (battery band/avg, charging coverage; also aggregates device_count + distinct_builds).

**Tier 1 verified (2026-06-09):** brought up the Docker stack, seeded a device + 35 days
of daily stats + a group, and screenshotted the device **Trends** tab and the group
**Group trends** chart — both render (battery band + avg, charging coverage, human-readable
date axis).

### Tier 2 — Alerting (build FIRST — highest ROI, no ML) — **landed + verified**
Rule-based catalog, tuned to the restaurant/overnight-charge use case. See §3.

**Done (branch `analytics-alerting`):**
- `alert_rules` + `alerts` tables; partial unique index enforces one non-resolved alert
  per (type, device) so re-evaluation never duplicates.
- DB layer: `EnsureDefaultRules`, `ListAlertRules`, `CreateAlertIfAbsent`,
  `ResolveOpenAlert`, `ListAlerts`, `CountOpenAlerts`, `SetAlertStatus`.
- Evaluator `EvaluateAlerts` over `device_daily_stats` (rules: **overheating**,
  **no_overnight_charge**, **battery_health_decline**), run in `RunHousekeeping` after
  the rollup; default rules seeded at startup. Creates + auto-resolves alerts.
- Dashboard **Alerts** page (status filter pills, severity/status badges, Ack/Resolve),
  nav entry with an open-count badge.
- Verified 2026-06-09 via the seeded stack (alert row + nav badge render; fixed a
  date/int SQL type-ambiguity bug in the decline query found by running it).

**Also landed (2026-06-09):**
- **offline** rule — fires when `last_seen_at` exceeds a threshold, skipping each
  device's local quiet/overnight window (timezone from `latest_extra`, default 00:00–06:00)
  so overnight charging doesn't trip it. Verified fire + auto-resolve.
- **Webhook notifications** — `EvaluateAlerts` returns serial-tagged notifications;
  housekeeping POSTs each new alert to a Slack/Discord/Mattermost-compatible webhook
  (`AlertWebhookURL` setting + Settings UI). Verified end-to-end delivery.
- Bonus fix: config writes were silently failing in Docker (missing config dir); `Load`
  now creates it, so all settings actually persist.

**Still open for Tier 2:** offline rule uses quiet-hours, not true per-restaurant service
windows (refine once service hours are defined); group/device-scoped rules (evaluator is
fleet-scope only); live dashboard refresh when an alert fires (webhook covers external
notify; in-app live update would need an SSE endpoint).

### Tier 3 — Diagnostic & trends (the "per-group" analytics) — **landed + verified**
Per restaurant/chain: uptime %, offline incidents (count + duration), overnight charge
recovery, battery-health trend, temp distribution, firmware consistency %. Rank venues by
health so ops knows where to send a tech. Week-over-week deltas catch a venue degrading
*before* a support ticket.

**Done (2026-06-09):**
- `GetGroupHealth(activeSecs)` — one SQL (CTEs over `device_daily_stats` + `devices` +
  `alerts`) computing per group: device/offline counts, open critical/warning alerts,
  recent avg daily-peak battery, **week-over-week battery delta**, avg charging coverage,
  max temp, distinct build count.
- A 0–100 **health score** (`computeScore`): penalties for offline ratio, open alerts,
  poor charging, battery decline, overheating, firmware fragmentation → ok/warn/danger.
- **Fleet Health** page at `/fleet-health` (nav "Health"): fleet summary bar + per-group
  scorecard ranked **worst-first**. Verified via seeded stack — a struggling group scored
  0, a healthy one 100, with correct alert/offline/delta columns.

**Still open for Tier 3:** offline *incident* history (count + duration over time) is not
yet tracked (we show current offline count, not historical incidents); temp distribution
is shown as a single max, not a spread.

### Tier 4 — Predictive (ML, eventually & optional)
Predictive battery replacement: "this tablet's battery will fail within ~3 weeks." Needs
6–12 months of history. Start with a heuristic; only reach for a model if the heuristic
proves insufficient. Revisit ~mid-2027.

---

## 3. Alert catalog (Tier 2)

Ordered by value. Each becomes a rule in the evaluator (§5).

| # | Alert | Trigger (draft) | Why it matters |
|---|---|---|---|
| 1 | **Offline during service** | Silent > N min while the venue is open (needs service window) | Lost ordering capability = lost revenue. **#1 alert.** |
| 2 | **Didn't charge overnight** | Battery < ~95% at store-open, or `charging=false` all night | Bad pad / left off dock / dying battery |
| 3 | **Won't survive the shift** | Battery trajectory projects < 20% before close | Pre-empt a mid-service death |
| 4 | **Overheating** | `battery_temp_c` > threshold | Fire risk + battery damage on pads |
| 5 | **Battery health decline** | Overnight-full → shift-end % shrinking week/week | Degradation → schedule replacement |
| 6 | **Firmware / app drift** | Device or group stuck on old `build_id` / ordering-app version | Inconsistent fleet, missed fixes |
| 7 | **Memory pressure** | Sustained high `ram_usage_mb` ratio | Predicts crashes/reboots |
| 8 | **Device left the building** | Location jumped outside venue | Theft / misplacement |

Thresholds (N min, temp °C, %) to be finalized; start conservative, tune from data.

---

## 4. Data-volume reality (drives architecture)

~900 devices × 1440 checkins/day ≈ **1.3M rows/day** in `checkins`. Trends must NOT scan
raw rows.

- **Nightly rollup job** → `device_daily_stats` (per device per day): min/max/avg battery,
  overnight recovery %, online minutes, max temp, peak RAM ratio, build_id, charging
  coverage. Trends + group analytics query this table.
- **Retention:** raw `checkins` ~30–90 days (existing `PruneCheckins()`); daily rollups
  kept ~forever (tiny).
- Plain Go cron + SQL fits the "one binary, one `db.go`" style. TimescaleDB continuous
  aggregates are an option but not required.

---

## 5. Proposed architecture

Fits existing conventions (centralized routes in `cmd/server/main.go`, append-only
`migrationSQL`, `ws.Hub` event fan-out).

**New tables (append to `migrationSQL`, idempotent):**
- `device_daily_stats` — nightly rollups (§4).
- `alert_rules` — rule definitions (type, params, scope: fleet/group/device, enabled).
- `alerts` — fired alert events (rule_id, device_id, status open/ack/resolved, fired_at,
  resolved_at, detail JSONB).
- `service_windows` — per-group open/close hours + timezone (for alerts #1/#3). Or infer.

**Evaluator:**
- Live rules (offline, temp, RAM) evaluate on the checkin path in `internal/api`.
- Daily rules (overnight charge, health decline) run in the nightly job after rollup.
- Fired alerts publish a new `AlertEvent` through `ws.Hub` → dashboard + notifications.

**Notifications:** email / webhook / Slack channel from the alert event.

**Dashboard:** a per-group **Trends** tab (battery recovery, uptime, temp over 30 days)
and an **Alerts** view (open/ack/resolved).

---

## 6. First vertical slice (proposed starting point)

1. `device_daily_stats` table + nightly aggregation in `db.go`.
2. `alert_rules` + `alerts` tables + a minimal evaluator wired into the checkin handler
   and `ws.Hub`, starting with the **two** highest-value rules:
   **offline-during-service** and **didn't-charge-overnight**.
3. Dashboard "Trends" tab per group (battery recovery, uptime, temp, 30-day window).

---

## 7. Open questions (resolve before coding)

- [ ] **What is a `group`** — one restaurant, or a chain/brand? Determines whether
  "per-group trends" means per-venue or per-customer.
- [ ] **Service hours** — do we have per-restaurant open/close hours anywhere, or do we
  infer them from activity patterns? (Alerts #1/#3 need "open" vs "closed".)
- [ ] Alert delivery channel(s): email, Slack, webhook, in-dashboard only?
- [ ] Threshold starting values (offline N min, temp °C, RAM %, overnight target %).
- [ ] Retention windows: raw checkins days vs. daily rollups.

---

## 8. Tier 5 — T7 tableside alert matrix (test-team spec)

The QA/test team supplied a 26-alert matrix for the **T7 tableside device** (battery SoC,
battery health/lifecycle, guest charging pad, thermal, connectivity, app/kiosk, storage,
system health). This tier folds that matrix into the existing rule engine (§3, §5) rather
than building a parallel system. Most of it reuses what Tiers 1–3 already shipped; the new
primitives are **per-restaurant service windows** (§9), a **recent-checkin evaluation tier**
for rate/sustained rules (§10), an **`info` severity**, and a **routing alerts channel** (§11).

**Build order (per the product decision):** server-feasible alerts first (Phase 1 — only
telemetry we already collect), then the alerts that need new AOSP client telemetry (Phase 2).

### 8.1 Mapping — all 26 alerts

Telemetry already collected (client → `checkins.extra`, see §1): `battery_pct`,
`battery_temp_c`, `charging`, `wlc_status` (guest-pad/reverse-charge state via GPIO),
`ram_usage_mb{used,total}`, `storage_free_gb`, `uptime_seconds`, `wifi`/`wifi_scan[].rssi`,
`timezone`, `last_seen_at`. Anything else is a Phase-2 client change.

| # | T7 alert | Sev | Maps to | Phase | Notes |
|---|---|---|---|---|---|
| 1 | SoC low during service (<20% unplugged) | Crit | **new** `soc_low_service` | 1 | `battery_pct`+`charging`; gate to service window; recent-eval |
| 2 | SoC low while pad charging a guest | Crit | **new** `soc_low_guest_charging` | 1 | + `wlc_status`=guest-present; confirm encoding |
| 3 | Not charging overnight (flat 60 min plugged) | Crit | refine `no_overnight_charge` + **new** `overnight_not_charging` | 1 | coarse rule exists; flat-for-60-min needs recent-eval |
| 4 | Charging too slowly overnight (<15%/2h) | Crit | **new** `overnight_slow_charge` | 1 | 2-h SoC-gain rate; recent-eval, overnight window |
| 5 | Abnormal discharge — pad idle (>5%/h) | Warn | **new** `discharge_rate_idle` | 1 | SoC slope + no guest on pad; recent-eval |
| 6 | Abnormal discharge — pad active (>14%/h) | Warn | **new** `discharge_rate_active` | 1 | SoC slope + guest on pad; threshold to calibrate |
| 7 | Battery health degraded (<85%) | Warn | **new** `battery_health_low` | 2 | true health % not collected; `battery_health_decline` is only a proxy |
| 8 | Battery health critical (<80%) | Crit | **new** `battery_health_low` (crit tier) | 2 | needs client health/capacity |
| 9 | High charge-cycle count (>400/>500) | Info | **new** `charge_cycles_high` | 2 | needs client cycle-count; first **info** rule |
| 10 | Pad disconnected during service | Warn | **new** `pad_disconnected` | 1 | `wlc_status` drop; service window |
| 11 | Pad connected, never used all day | Info | **new** `pad_unused` | 1 | needs daily rollup of guest-present count |
| 12 | Pad utilisation count per shift | Info | **metric**, not alert | 1 | new daily-stats column `pad_sessions`; surface on dashboard |
| 13 | Overheating (>45°C) | Crit | **existing** `overheating` | ✓ | uses `battery_temp_c` as device-temp proxy; true thermal = Phase-2 refinement |
| 14 | Temp elevated (38–45°C sustained >15 min) | Warn | **new** `temp_elevated` | 1 | sustained-window; recent-eval |
| 15 | Device offline (>5 min) during service | Crit | refine `offline` | 1 | enable; 5-min threshold; gate to service window not quiet-hours |
| 16 | Frequent Wi-Fi disconnects (>3/h) | Warn | **new** `wifi_disconnects` | 2 | needs client disconnect-event counting |
| 17 | Weak Wi-Fi signal (RSSI <−75 sustained) | Warn | **new** `wifi_weak` | 2 | needs connected-AP RSSI reported explicitly + recent-eval |
| 18 | Ordering app not foreground (>2 min) | Crit | **new** `app_not_foreground` | 2 | needs client foreground-app reporting |
| 19 | Repeated app crashes (>2/4h) | Warn | **new** `app_crashes` | 2 | needs client crash events |
| 20 | Kiosk mode disabled | Warn | **new** `kiosk_exit` | 2 | needs client lock-task exit event |
| 21 | App not responding (ANR) | Warn | **new** `app_anr` | 2 | needs client ANR events |
| 22 | Storage critically low (<500 MB) | Crit | **new** `storage_low` | 1 | `storage_free_gb` snapshot |
| 23 | Storage filling fast (<1.5 GB or >200 MB/24h) | Warn | **new** `storage_filling` | 1 | snapshot + daily delta (new daily-stats column) |
| 24 | Unexpected reboot | Warn | **new** `unexpected_reboot` | 1 (reason → 2) | reboot detectable now via `uptime_seconds` reset; reason code needs client |
| 25 | OS out of compliance (>1 minor behind) | Info | **new** `os_noncompliant` | 2 | only `build_id` today; needs semantic OS version + baseline config |
| 26 | Memory pressure (avail RAM <400 MB sustained) | Crit | refine `memory_pressure` | 1 | exists as %-based; add absolute-MB threshold + enable; recent-eval for "sustained" |

**Phase 1 (server-only, telemetry already in hand):** alerts 1, 2, 3, 4, 5, 6, 10, 11, 12,
13✓, 14, 15, 22, 23, 24 (occurrence), 26. Plus the cross-cutting primitives §9–§11.

**Phase 2 (needs new AOSP client telemetry):** alerts 7, 8, 9, 16, 17, 18, 19, 20, 21, 24
(reason), 25. Tracked separately; the client changes land on the `t7-alert-matrix` client branch
and add fields to `checkins.extra` plus an event-ingestion path (§10.3) for crash/ANR/kiosk/reboot.

## 9. Service windows (per-restaurant active windows)

The matrix gates most alerts to **service hours (07:00–23:00)** or the **overnight window
(23:30–06:00)**. Real venues differ, so we model this per group rather than hardcoding the
matrix's clock. Closes the §7 "service hours" open question.

- **New table `service_windows`** (append-only migration, idempotent): `group_id` FK,
  `open_min`/`close_min` (minutes past local midnight), `timezone` (falls back to the device's
  reported `extra.timezone`), optional per-weekday override later. A group with no row uses a
  fleet default (07:00–23:00). Overnight = the complement of the open window, trimmed to the
  matrix's 23:30–06:00 by default.
- **Rule param `active_window`**: `service` | `overnight` | `always`. The evaluator resolves
  each device's window from its group's `service_windows` row (device-local via `gmtOffset`,
  reusing the existing `inQuiet` helper) and skips devices outside the rule's window — exactly
  how the `offline` rule already skips quiet hours, generalized.
- Migration seeds a fleet-default window so Phase-1 rules work before any group is configured.

**Lab vs deployment (matches the AI-report framing).** The window-gated rules
(`service`/`overnight`) are *operational* — they assume the unit is live in a restaurant
on a service/charge schedule. A bench/lab unit that is idle or unplugged during the default
day window is expected behavior, not an alert, so these rules fire for **deployed units
only** (effective deployed = `COALESCE(device.deployed, any-group.deployed, false)`,
`deployedDeviceSet`). Always-on **hardware** rules (`storage_low`, `temp_elevated`,
`overheating`) still fire fleet-wide so a genuine lab hardware fault surfaces. This keeps
the Alerts feed — and therefore the AI fleet/device report, which ingests open alerts — free
of lab-unit operational noise, consistent with the report's existing "only deployed units
drive watch/at_risk" rule (`internal/ai/ai.go`).

## 10. Evaluation tiers (daily vs recent vs event)

Today `EvaluateAlerts` runs **hourly** in `RunHousekeeping` over `device_daily_stats`. That
fits trend rules but cannot serve point-in-time ("SoC <20% now", "offline >5 min") or
short-window rate/sustained rules. We split evaluation into three tiers:

1. **Daily tier (exists)** — trend rules over `device_daily_stats`, hourly in housekeeping:
   `no_overnight_charge`, `battery_health_decline`, `pad_unused`, `storage_filling` (24-h delta),
   `os_noncompliant`.
2. **Recent tier (new)** — a fast evaluator on a **1-minute ticker** (mirrors the existing
   scheduled-reboot dispatcher in `main.go`), reading `devices.latest_extra` + the last N
   `checkins` rows per device for slope/sustained computation. Serves: `soc_low_*`, `offline`,
   `overnight_*`, `discharge_rate_*`, `temp_elevated`, `pad_disconnected`, `storage_low`,
   `memory_pressure` (sustained), `unexpected_reboot` (uptime reset). Reuses
   `CreateAlertIfAbsent`/`ResolveOpenAlert` so dedupe + auto-resolve are unchanged.
3. **Event tier (Phase 2)** — point-in-time client events (crash, ANR, kiosk-exit, reboot
   reason) POSTed to a new `/api/v1/events` endpoint into an `events` table, evaluated on
   ingest for count-in-window rules (`app_crashes`, `app_anr`, `wifi_disconnects`).

Both daily and recent tiers feed the same `alerts` table and the same channel router (§11),
so the dashboard and notifications don't care which tier fired an alert.

## 11. Severity & the alerts channel (routing + delivery)

**`info` severity** is added alongside `critical`/`warning` (matrix uses all three). It affects
badge styling, the nav open-count (info excluded from the urgent badge), and routing defaults.

Today notification is a single best-effort `SendWebhook` per new alert (`notify.go`). The
matrix needs the alert to reach the right place at the right time, so we build a routing layer
**and** wire a concrete chat destination:

- **New table `alert_channels`**: `name`, `kind` (`webhook` — Slack/Discord/Mattermost-compatible
  to start), `url`, `min_severity` (`info`/`warning`/`critical`), `mode` (`realtime` | `digest`),
  optional `active_window` (route only during/outside service). The existing single
  `AlertWebhookURL` setting is migrated into one default channel so nothing breaks.
- **Routing**: when a tier creates an alert, the router picks every channel whose `min_severity`
  ≤ the alert's severity and whose `active_window` matches. `realtime` channels POST immediately
  (existing `SendWebhook`); `digest` channels accumulate into the existing daily-digest path.
  Recommended default routing: **Critical → realtime on-call channel**, **Warning → realtime ops
  channel**, **Info → daily digest only** (never paged).
- **Flap/spam control**: per-(type,device) dedupe already prevents duplicate opens. Add (a) a
  re-notify cadence so a still-open *critical* re-pings after a configurable interval, and (b) a
  one-line **resolve notification** when an alert auto-resolves, so the channel reflects recovery.
- **Concrete destination (the "literal channel"):** Slack via an Incoming Webhook URL per channel,
  pasted into Settings; messages keep the `{"text": ...}` shape so Discord/Mattermost work too.
  Per-severity emoji/prefix (`🔴 CRITICAL` / `🟠 WARNING` / `🔵 INFO`).

Dashboard: the existing **Alerts** page gains a severity filter (incl. info) and a
**channel/routing** config block in Settings (list channels, min-severity, mode, test-send).

## 12. Changelog

- **2026-06-08** — Initial checkpoint. Data inventory, ML verdict (not needed yet),
  4-tier roadmap, alert catalog, volume/architecture, first slice, open questions.
- **2026-06-08** — Tier 1 rollup pipeline landed on branch `analytics-alerting`:
  `device_daily_stats` + rollup/backfill, housekeeping + startup wiring, daily-stats
  getter + JSON endpoint, and a device-detail **Trends** tab (Chart.js). Per-group
  aggregation and screenshot verification still open.
- **2026-06-08** — Tier 1 per-group landed: `GetGroupDailyStats` + `/groups/{id}/daily-stats`
  + **Group trends** chart on the group page (groups = generic buckets, per answer).
  Only screenshot verification remains for Tier 1. Next up: Tier 2 alerting.
- **2026-06-09** — Tier 2 alerting landed (`analytics-alerting`): alert tables, DB layer,
  evaluator (3 rules) wired into housekeeping, Alerts dashboard page + nav badge. Tier 1
  and Tier 2 both verified by screenshotting a seeded Docker stack. Fixed a date/int SQL
  bug in the decline query.
- **2026-06-09** — Tier 2 completed: **offline** rule (quiet-hours aware) + **webhook
  notifications** (Slack-compatible), both verified end-to-end against the Docker stack;
  fixed config-dir bug so settings persist.
- **2026-06-09** — **Tier 3 landed**: `GetGroupHealth` + 0–100 health score + **Fleet
  Health** page (`/fleet-health`), per-group scorecard ranked worst-first with
  week-over-week battery delta. Verified via seeded stack. Tiers 1–3 now complete; Tier 4
  (predictive ML) remains intentionally deferred (~mid-2027, needs 6–12mo history).
- **2026-06-13** — **Tier 5 planned** (branch `t7-alert-matrix`): folded the QA team's
  26-alert T7 tableside matrix into the rule engine (§8). Mapped each alert to existing/new
  rules + a Phase-1 (server-feasible, telemetry in hand) / Phase-2 (needs AOSP client
  telemetry) split. New primitives designed: per-restaurant **service windows** (§9), a
  **recent-checkin evaluation tier** for rate/sustained rules (§10), an **`info`** severity,
  and a routing **alerts channel** (`alert_channels`, severity/window/digest, §11).
  Implementation next.
- **2026-06-13** — Phase 1 server implementation landed (branch `t7-alert-matrix`):
  service-window schema + resolution (§9); recent-tier evaluator on a 1-min ticker with
  service-window gating + the matrix's point-in-time/rate/sustained rules (offline-5m,
  soc_low_service, soc_low_guest_charging, pad_disconnected, storage_low, temp_elevated,
  discharge_rate_idle/active, overnight_not_charging, overnight_slow_charge); each rule
  type bound to exactly one tier; `alert_channels` CRUD + severity/window realtime routing
  with legacy-webhook migration. Window-gated (operational) rules fire for **deployed units
  only** so lab-bench noise stays out of the Alerts feed and the AI report, preserving the
  lab-vs-deployment distinction.
