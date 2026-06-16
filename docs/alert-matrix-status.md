# T7 Alert Matrix — Status & Plan (Done / Left)

> **Scope:** the QA/test team's 26-alert matrix for the **T7 tableside device**.
> This is the focused done/left/plan sheet. The full design rationale lives in
> [`analytics-and-alerting.md`](./analytics-and-alerting.md) §8–§11; this doc is the
> implementation scorecard derived from an audit of the actual server/client code.
>
> **Last audited:** 2026-06-15 (against `server/internal/db/db.go`, `cmd/server/main.go`,
> `internal/api/handlers.go`, `client/src/com/aioapp/mdm/MdmService.java`).
>
> **Update 2026-06-16:** Phase 2 implemented — new client telemetry + rules added.
> 25 of 26 alerts now wired (only #25 OS compliance deferred to release tracking).
> Live rule/field mapping moved to [`alert-matrix.md`](./alert-matrix.md); the
> sections below are the original pre-Phase-2 audit, kept for history.

---

## 1. Bottom line

- **Phase 1 (server-only, telemetry already collected): complete and wired.**
  18 default rules seeded (`db.go` `defaultAlertRules`, ~L2529–2548), evaluated on two tiers.
- **Phase 2 (needs new AOSP client telemetry): not started.** Every Phase-2 rule type is
  confirmed **absent** from the Go code, and the `/api/v1/events` ingestion endpoint does
  not exist yet.
- **15 of 26** matrix alerts fire today (16 counting reboot-occurrence without its reason
  code). **11** remain, all blocked on client-side telemetry we don't yet collect.

### How evaluation runs (recap)
- **Recent tier** — 1-minute ticker → `RunRecentAlerts` (`main.go:230–235`). Point-in-time +
  rate/sustained rules over `devices.latest_extra` + recent `checkins`.
- **Daily tier** — 1-hour ticker → `RunHousekeeping`. Trend rules over `device_daily_stats`.
- Cross-cutting primitives in place: per-restaurant **service windows** (`service_windows`),
  **`info` severity**, **`alert_channels`** routing (Slack/Discord/Teams), and deployed-only
  gating for operational/windowed rules so lab-bench units don't generate noise.

---

## 2. Done — 15 alerts shipping (Phase 1) ✅

| # | T7 alert | Rule type | Tier | Notes |
|---|---|---|---|---|
| 1 | SoC low during service (<20% unplugged) | `soc_low_service` | recent | service window |
| 2 | SoC low while pad charging a guest | `soc_low_guest_charging` | recent | uses `wlc_status` |
| 3 | Not charging overnight (flat 60 min) | `overnight_not_charging` (+ daily `no_overnight_charge`) | recent + daily | |
| 4 | Charging too slowly overnight (<15%/2h) | `overnight_slow_charge` | recent | |
| 5 | Abnormal discharge — pad idle (>5%/h) | `discharge_rate_idle` | recent | |
| 6 | Abnormal discharge — pad active (>14%/h) | `discharge_rate_active` | recent | **threshold still to calibrate** |
| 10 | Pad disconnected during service | `pad_disconnected` | recent | `wlc_status` drop |
| 11 | Pad connected, never used all day | `pad_unused` | daily | |
| 13 | Overheating (>45°C) | `overheating` | daily | **`battery_temp_c` proxy**, not true device thermal |
| 14 | Temp elevated (38–45°C sustained) | `temp_elevated` | recent | |
| 15 | Device offline (>5 min) during service | `offline` | recent | **disabled by default** (`app_flag` `offline_rule_disabled_v1`); ops re-enables |
| 22 | Storage critically low (<500 MB) | `storage_low` | recent | |
| 23 | Storage filling fast (<1.5 GB / >200 MB/24h) | `storage_filling` | daily | snapshot + 24h delta |
| 24 | Unexpected reboot (occurrence) | `unexpected_reboot` | recent | via `uptime_seconds` reset; **reason code = Phase 2** |
| 26 | Memory pressure (avail RAM <400 MB) | `memory_low` (+ daily `memory_pressure`) | recent + daily | |

**Telemetry these rely on (already sent by client `MdmService.java`):** `battery_pct`,
`battery_temp_c`, `charging`, `wlc_status`, `storage_free_gb`, `uptime_seconds`,
`ram_usage_mb{used,total}`, `timezone`, `wifi_scan[]`, `rssi`.

---

## 3. Left — 11 alerts (Phase 2, blocked on client telemetry) ❌

All confirmed **not implemented** in the server (rule types absent) and **not collectable**
with today's checkin payload.

| # | T7 alert | Proposed rule type | Blocked on (new client telemetry) |
|---|---|---|---|
| 7 | Battery health degraded (<85%) | `battery_health_low` (warn) | true battery health % |
| 8 | Battery health critical (<80%) | `battery_health_low` (crit) | battery health / design-vs-current capacity |
| 9 | High charge-cycle count (>400/>500) | `charge_cycles_high` (info) | battery cycle count *(first `info` rule)* |
| 12 | Pad utilisation count per shift | **metric, not a rule** | `pad_sessions` daily-stats column — **never built** |
| 16 | Frequent Wi-Fi disconnects (>3/h) | `wifi_disconnects` | client disconnect-event counting → events path |
| 17 | Weak Wi-Fi signal (RSSI <−75 sustained) | `wifi_weak` | connected-AP RSSI reported explicitly |
| 18 | Ordering app not foreground (>2 min) | `app_not_foreground` | foreground-app reporting |
| 19 | Repeated app crashes (>2/4h) | `app_crashes` | crash events → events path |
| 20 | Kiosk mode disabled | `kiosk_exit` | lock-task exit event |
| 21 | App not responding (ANR) | `app_anr` | ANR events → events path |
| 25 | OS out of compliance (>1 minor behind) | `os_noncompliant` | semantic OS version + baseline config |

> #24 is split: the **occurrence** ships today; the **reason code** (thermal shutdown vs
> kernel panic vs watchdog vs user) needs a client field and is tracked in Phase 2.

---

## 4. Known gaps & discrepancies (independent of Phase 2)

1. **#12 pad-utilisation metric was never built.** The design doc implies a `pad_sessions`
   daily-stats column; it does not exist (`grep` for `pad_session|guest_count` in `db.go` is
   empty). Either build the column or correct the doc.
2. **#13 overheating uses `battery_temp_c` as a device-temp proxy.** True SoC/thermal-zone
   temperature is a Phase-2 client refinement.
3. **#6 active-discharge threshold (`14%/h`) is uncalibrated** — needs real pad-active
   discharge-test data, per the matrix's own note.
4. **#15 offline rule is disabled by default** (one-time `app_flag`). Intentional, but worth
   stating: the matrix wants a 5-min service-window offline alert, so re-enabling +
   confirming the threshold is the expected ops step.

---

## 5. Plan — getting Phase 2 done

Phase 2 is gated almost entirely by **client telemetry**, so the critical path is the AOSP
client (`t7-alert-matrix` client branch), then server rules, then dashboard.

### Workstream A — Client telemetry (extends `checkins.extra`)
Add periodic fields to the checkin payload in `MdmService.java`:
- `battery_health_pct` (and/or `battery_capacity_uah` design vs current) → alerts 7, 8
- `charge_cycle_count` → alert 9
- `device_temp_c` (true thermal zone, not battery) → refine alert 13
- `wifi_rssi` for the **connected** AP, reported explicitly → alert 17
- `os_version` (semantic, comparable) alongside `build_id` → alert 25

Server side: extend the recent/daily evaluators to read these keys (same `extraNum`/`extraStr`
helper pattern already used for `storage_free_gb`, `uptime_seconds`).

### Workstream B — Event-ingestion path (`/api/v1/events`)
Per `analytics-and-alerting.md` §10.3:
- New `events` table + `POST /api/v1/events` (device-auth) for point-in-time client events:
  app crash, ANR, kiosk/lock-task exit, reboot reason, Wi-Fi disconnect.
- **Event tier** evaluator: count-in-window rules `app_crashes` (>2/4h), `app_anr`,
  `wifi_disconnects` (>3/h); plus `kiosk_exit` (immediate) and reboot-reason enrichment of #24.
- Reuse `CreateAlertIfAbsent` / `ResolveOpenAlert` so dedupe + auto-resolve are unchanged.

### Workstream C — New server rules (once A/B land)
`battery_health_low` (warn+crit tiers), `charge_cycles_high` (info), `wifi_weak`,
`wifi_disconnects`, `app_not_foreground`, `app_crashes`, `app_anr`, `kiosk_exit`,
`os_noncompliant`. Add to `defaultAlertRules`, bind each to the correct tier
(`recentRuleTypes` / daily / event), and pick the active-window per the matrix.

### Workstream D — Metric & cleanups
- Build the `pad_sessions` daily-stats column + surface it on the dashboard (#12).
- Wire `device_temp_c` into `overheating`/`temp_elevated` once present (#13).
- Calibrate `discharge_rate_active` from pad-active discharge tests (#6).
- Decide on the `offline` rule default + confirm 5-min threshold (#15).

### Suggested sequencing
1. **A + B scaffolding together** (client fields + `/events` table/endpoint) — unblocks everything.
2. **C: snapshot-based rules first** (battery health, cycles, wifi_weak, os_noncompliant) — they
   only need Workstream-A fields.
3. **C: event-based rules** (crashes, ANR, kiosk-exit, wifi-disconnects, reboot reason) — need B.
4. **D cleanups** in parallel; they don't block the client work.

---

## 6. Quick reference — where things live
- Default rules + rule→tier binding: `server/internal/db/db.go` (`defaultAlertRules`, `recentRuleTypes`).
- Recent evaluator: `RunRecentAlerts` wired in `cmd/server/main.go` (1-min ticker).
- Daily evaluator: `EvaluateAlerts` in `RunHousekeeping` (1-hour ticker).
- Checkin payload (client): `client/src/com/aioapp/mdm/MdmService.java` (~L950–1115).
- Checkin ingest (server): `internal/api/handlers.go` `checkinRequest` (~L312).
- Service windows: `service_windows` table + resolution helpers in `db.go` (§ "Service windows").
- Channels/routing: `alert_channels` + `internal/notify/notify.go`.
</content>
</invoke>
