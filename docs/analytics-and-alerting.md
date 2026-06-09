# Analytics, Alerting & Data-Driven Decisions

> **Status:** Checkpoint / living design doc. This is the working plan for turning MDM
> telemetry into decisions, alerts, and trends. We add to this as we go — it is the
> source of truth for the analytics program, not a one-time writeup.
>
> **Last updated:** 2026-06-08

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

**Still open for Tier 1:** visual screenshot verification of the Trends views against a
seeded stack.

### Tier 2 — Alerting (build FIRST — highest ROI, no ML)
Rule-based catalog, tuned to the restaurant/overnight-charge use case. See §3.

### Tier 3 — Diagnostic & trends (the "per-group" analytics)
Per restaurant/chain: uptime %, offline incidents (count + duration), overnight charge
recovery, battery-health trend, temp distribution, firmware consistency %. Rank venues by
health so ops knows where to send a tech. Week-over-week deltas catch a venue degrading
*before* a support ticket.

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

## 8. Changelog

- **2026-06-08** — Initial checkpoint. Data inventory, ML verdict (not needed yet),
  4-tier roadmap, alert catalog, volume/architecture, first slice, open questions.
- **2026-06-08** — Tier 1 rollup pipeline landed on branch `analytics-alerting`:
  `device_daily_stats` + rollup/backfill, housekeeping + startup wiring, daily-stats
  getter + JSON endpoint, and a device-detail **Trends** tab (Chart.js). Per-group
  aggregation and screenshot verification still open.
- **2026-06-08** — Tier 1 per-group landed: `GetGroupDailyStats` + `/groups/{id}/daily-stats`
  + **Group trends** chart on the group page (groups = generic buckets, per answer).
  Only screenshot verification remains for Tier 1. Next up: Tier 2 alerting.
