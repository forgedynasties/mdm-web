# AIO MDM — API Reference

**Audience:** penetration-testing / security review team
**Scope:** the HTTP + WebSocket surfaces of the MDM server (`server/`), generated from the code as of this handoff.
**Companion doc:** [`AUTH_MODEL.md`](./AUTH_MODEL.md) — authentication, roles, session handling, and known trust boundaries.
**Machine-readable:** [`openapi.yaml`](./openapi.yaml) — import into Burp Suite / Postman / ZAP.

> This document describes behaviour as implemented, not as idealised. Where a control is deliberately absent or weak, it is called out under **⚠ Security notes**.

---

## 1. Surfaces at a glance

The server exposes one `http.ServeMux` (`cmd/server/main.go`) carrying four coexisting surfaces:

| Surface | Base path | Auth | Notes |
|---|---|---|---|
| Device API | `/api/v1/checkin`, `/commands/{id}/ack`, `/logcat`, `/ota/status`, `/ws` | `X-API-Key: <DEVICE_API_KEY>` | Used by fleet devices |
| Admin REST API | `/api/v1/devices`, `/groups`, `/productions`, `/commands` | `X-API-Key: <ADMIN_API_KEY>` | Machine-to-machine admin |
| Testing team API | `/api/v1/testdata/…` (GET only) | `X-API-Key: <QA_API_KEY>` | Read-only; see §3b |
| Remote-control WS | `/api/v1/remote/{serial}` | **Single-use token in query string** (no API key) | Screen mirror / input |
| HTMX dashboard | `/`, `/login`, `/devices`, `/settings`, … | Cookie session + RBAC | See `AUTH_MODEL.md` |
| Unauthenticated | `/health`, `/static/…`, `/splash-img/…` | none | See §6 |

All JSON request bodies are expected as `Content-Type: application/json`. Timestamps are RFC 3339 (UTC). IDs are UUIDs.

**Global middleware** (applied to every response): `SecurityHeaders` (HSTS, CSP, `X-Frame-Options: DENY`, etc. — see `AUTH_MODEL.md §5`) and `DecompressRequest` (transparently gunzips request bodies sent with `Content-Encoding: gzip`).

---

## 2. Device API (`X-API-Key: <DEVICE_API_KEY>`)

All endpoints below require the **device** key. Auth is a constant-time compare with per-IP failure rate limiting (30 failures/min → `429`, see `AUTH_MODEL.md §3`).

### POST `/api/v1/checkin`
Device posts its state and receives pending commands + config.

Request:
```json
{
  "serial_number": "string",         // required
  "build_id": "string",              // required
  "battery_pct": 0,                  // optional, nullable, 0–100
  "extra": {},                       // optional, arbitrary JSON (json.RawMessage)
  "installed_apps": [                // optional
    { "package": "string", "name": "string", "version_name": "string", "is_system": false }
  ],
  "ota_progress": {                  // optional
    "command_id": "uuid", "phase": "string", "percent": 0
  }
}
```
Response `200`:
```json
{
  "status": "ok",
  "commands": [ { "id": "uuid", "type": "string", "apk_url": "string", "payload": {} } ],
  "config": {
    "kiosk_enabled": false, "kiosk_package": "string",
    "kiosk_features": 0, "checkin_interval_seconds": 0,
    "guest": {                       // firmware clients only (not DPC / MDM-lite)
      "restaurant_name": "string",
      "wifi_ssid": "string", "wifi_password": "string",
      "wifi_security": "WPA|WEP|nopass|",   // "" when no network is set
      "guest_app_package": "string"         // "" = the client's default ordering app
    }
  }
}
```
Status: `200` ok · `400` missing required fields · `500` internal.

`config.guest` is what a T7 shows its guests: the venue (restaurant name and guest Wi-Fi,
set on the restaurant page). There is no `table_label` any more (removed 14 Oct); the
firmware client reads the missing key as empty, so a tablet that still shows a table clears it.
Every key is always present, empty when unset, so a cleared value clears on the device.
It rides HTTP check-ins only; over the WebSocket it comes as its own `guest` frame (below).

#### Lost-device reporting (18 Oct)

Two more keys are in **every** config answer and every `config` frame, because a frame can be
partial and the clients read a missing key as "unchanged" (`lost_listen`) or "off" (`self_lock`):

| Key | Value |
|---|---|
| `lost_listen` | `{"beacons": ["<16 hex>", …], "scan_s": 8, "every_s": 120}` while a device at the same venue is lost — scan Bluetooth LE for those beacons; `null` otherwise (stop). |
| `self_lock` | firmware clients only: `{"enabled": false, "venue", "venue_icon_v"}`, or `{"enabled": true, "minutes": 10, "message": "It belongs to <venue>. Please hand it to a member of staff[, or call:]", "phone": "<AIO support number>", "venue", "venue_icon_v"}` when the venue has self-lock on. `venue` is the device's venue name (`""` if none) and `venue_icon_v` its icon version (unix seconds, `0` if none); the lost screen (client 1.13.4+) draws them, fetching the icon from `GET /api/v1/venue-icon` when the version changes. "or call:" only appears when a support number is set. |

A `lost_mode` command `{"on": true}` sent to one device gains `"beacon": "<16 hex>"` — a random
token the server makes per lost session (a caller's own is replaced) — and the device's `"venue"`
and `"venue_icon_v"`, as in `self_lock`. The lost device advertises
it as Bluetooth LE service data under UUID `6a1f0001-a10c-4d4d-9c5e-4c0574000001`. When a device is
marked lost or lifted, the other devices at its venue get a `checkin_now` so they pick
`lost_listen` up at once.

Extra keys a client reports for it:

| `extra` key | From | Meaning |
|---|---|---|
| `ble_sightings` | listeners | `[{"beacon", "rssi", "at_ms"}]` — what the last scan heard (omitted when nothing). Stored per neighbour per minute, strongest kept; not kept in history. |
| `ble_scan` | listeners | `{"ok": false, "why"}` when a scan was asked for but could not run. Not kept in history. |
| `lost_beacon` | the lost device | `{"ok": bool, "why"}` — whether it is advertising. The reliable signal; `ble_beacon` can be optimistic. Not kept in history. |
| `lost_mode.self` | firmware 1.13.0+ | `true` when it locked itself (self-lock), with `since_ms`; it lifts itself (`on: false`) when its venue's Wi-Fi is back. |
| `ble_listen`, `ble_beacon` | firmware 1.13.0+ | capability flags (`true`) when the radio can scan / advertise. The Standard client lists the same names in its capabilities. |

The same fields can be set with the admin key (`X-API-Key`), for scripts and test benches;
each change is pushed to the connected firmware devices it affects:

| Method / path | Body | Does |
|---|---|---|
| `POST /api/v1/restaurants` | `{"name", "address"?, "timezone"?, "notes"?}` | creates a restaurant (`201`, the restaurant) |
| `POST /api/v1/restaurants/{id}/devices` | `{"serials": [...]}` | places the devices at the venue |
| `POST /api/v1/restaurants/{id}/guest-wifi` | `{"ssid", "password"?, "security"?, "app_package"?}` | sets the venue's guest Wi-Fi (empty `ssid` clears it; same checks as the dashboard; the password is not echoed back) |

### POST `/api/v1/checkins/backfill`
Readings a firmware client (1.7.0+) kept while it could not reach the server:
`{"serial", "now_ms", "now_elapsed_ms", "boot_id", "dropped", "readings": [{"at_ms", "elapsed_ms",
"boot_id", "battery_pct", "temp_c", "wifi_rssi", "ram_used_mb", "ram_total_mb", "storage_free_gb",
"charging", "wlc_status", "crashes": […], "wifi_scan": […], "lost_mode": {…}}]}`.
Stored as late history (no alerts). Since 1.13.0 (lost-device reporting) a reading may carry
`wifi_scan` — up to 8 `{"bssid", "ssid", "rssi"}`, kept when the APs in view changed — which
becomes the device's trail (placed on the map only for devices that are lost or set off
`left_venue` in the last day), and `lost_mode` — a self-lock (`"self": true`) that happened
while out of reach is recorded from it. Answer `{"accepted", "stored"}`.

### POST `/api/v1/commands/{id}/ack`
Device reports progress/terminal state for a command.

Request:
```json
{
  "serial_number": "string",         // required
  "status": "installing",            // required: downloading|installing|installed|failed|completed
  "output": "string",                // optional
  "progress": 0,                     // optional, nullable, 0–100 (interim)
  "package": "string"                // optional, learned on "installed"
}
```
Response `200`: `{"status":"ok"}`
Status: `200` · `400` invalid status · `403` command does not target this device · `404` device not found · `500`.

### POST `/api/v1/logcat`
Device uploads a requested logcat dump.

Request:
```json
{ "serial_number": "string", "request_id": "uuid", "content": "string" }   // all required
```
Response `200`: `{"status":"ok"}`
Status: `200` · `400` · `404` device not found · `500`.

### POST `/api/v1/ota/status`
Device reports OTA install progress.

Request:
```json
{
  "serial_number": "string",         // required
  "command_id": "uuid",              // required
  "status": "installed",             // required: downloaded|installed|error
  "error_code": "string"             // optional, set when status=error
}
```
Response `200`: `{"status":"ok"}`
Status: `200` · `400` · `403` command not targeting device · `404` · `500`.

### GET `/api/v1/venue-icon?serial=<serial>`
The calling device's own venue icon, a PNG of at most 128 px (`Cache-Control: private,
max-age=86400`), for the lost screen. Same device auth and serial binding as the other device
calls. Fetch it when `venue_icon_v` (lost_mode payload, config `self_lock`) changes; the image
never rides in a WebSocket frame.
Status: `200` · `400` · `401` · `403` · `404` no venue, or the venue has no icon.

### GET `/api/v1/ws` (WebSocket upgrade)
Long-lived device socket for server-push delivery. `X-API-Key` header is checked **before** upgrade.
Query: `serial` (required). Status: `101` upgrade · `400` missing serial · `404` device not found.
Message protocol documented in §5.

---

## 3. Admin REST API (`X-API-Key: <ADMIN_API_KEY>`)

All endpoints below require the **admin** key. Same rate-limit behaviour as the device key. There is **no finer-grained authorization on this surface** — any holder of the admin key can perform every operation here (contrast the dashboard's role model in `AUTH_MODEL.md`).

### Devices
| Method | Path | Purpose |
|---|---|---|
| GET | `/api/v1/devices` | List all devices |
| GET | `/api/v1/devices/{serial}` | Device detail + recent checkins |
| POST | `/api/v1/devices/{serial}/ping` | Live WS liveness/latency probe |
| GET | `/api/v1/devices/{serial}/temp-fast` | Fast-temperature setting + readings kept for 24 h |
| POST | `/api/v1/devices/{serial}/temp-fast` | Switch fast temperature on for N hours, or off |

`GET /devices` → array of device objects:
```json
{ "id":"uuid","serial_number":"string","build_id":"string","battery_pct":0,
  "last_seen_at":"RFC3339","created_at":"RFC3339","poll_interval_ms":0,
  "kiosk_enabled":false,"kiosk_package":"string","latest_extra":{},"hidden":false,
  "discharge_total_pct":0,"discharge_backfilled":false,
  "restaurant_id":"uuid","restaurant_name":"string","deployed_effective":false }
```
`GET /devices/{serial}` → `{ "device": {…}, "checkins": [ {…} ] }` (`404` if unknown).
`POST /devices/{serial}/ping` → `{ "connected":bool, "responsive":bool, "latency_ms":0, "error":"string" }` (`404` if unknown).

**Fast temperature.** `POST /devices/{serial}/temp-fast` `{"hours":24,"interval_sec":10}` (defaults shown;
`hours` 0–72, 0 = off; `interval_sec` 2–3600) → `{ "serial","enabled":bool,"interval_sec","until" }`.
The device gets `temp_fast_sec` in the config its next frame is answered with; client 1.5.0+ on
firmware v2.1.023+ then sends its temperature when it moved 0.2 °C (at most every `interval_sec`,
at least every 60 s). Every reading a switched-on device sends is kept in `device_temp_fast` for
24 h. `GET /devices/{serial}/temp-fast?since=<unix s>` → the same plus
`"readings":[{"at":"RFC3339","temp_c":0.0,"uptime_s":0}]` (default: the last 24 h).

### Groups
| Method | Path | Body | Purpose |
|---|---|---|---|
| GET | `/api/v1/groups` | — | List groups |
| POST | `/api/v1/groups` | `{"name":"string"}` | Create (`201`) |
| GET | `/api/v1/groups/{id}` | — | Group + member devices |
| DELETE | `/api/v1/groups/{id}` | — | Delete group |
| POST | `/api/v1/groups/{id}/devices` | `{"serial_number":"string"}` | Add device |
| DELETE | `/api/v1/groups/{id}/devices/{serial}` | — | Remove device |

Group object: `{ "id":"uuid","name":"string","device_count":0,"created_at":"RFC3339" }`.

### Productions
| Method | Path | Purpose |
|---|---|---|
| GET | `/api/v1/productions` | List production batches |
| POST | `/api/v1/productions` | Create (`201`) |
| GET | `/api/v1/productions/{id}` | Production + devices |
| DELETE | `/api/v1/productions/{id}` | Delete |

`POST /productions` request:
```json
{ "name":"string","product_code":"string","model_code":"string",
  "variant":"0","sku":"AA","batch_month":1,"batch_year":26,
  "start_sequence":1,"end_sequence":100,"notes":"string" }
```
Required: `name, product_code, model_code, batch_month (1–12), batch_year (0–99), start_sequence (≥1), end_sequence (≥start_sequence)`. `variant` defaults `"0"`, `sku` defaults `"AA"`.

### Commands
| Method | Path | Purpose |
|---|---|---|
| POST | `/api/v1/deployments/{id}/cancel` | Cancel an active deployment (frees devices blocked by it) |
| GET | `/api/v1/enrollment-profiles` | List enrollment profiles, tokens included |
| POST | `/api/v1/enrollment-profiles` | Create one and mint its token |
| GET | `/api/v1/commands` | List commands |
| POST | `/api/v1/commands` | Create + fan out to targets |
| GET | `/api/v1/commands/{id}` | Command + per-device delivery status |

`POST /enrollment-profiles` request — only `name` is required; the rest is the intent
every device inheriting this token picks up:
```json
{
  "name": "Menu board · stage",
  "device_class": "dongle",          // t7|kiosk|dongle|kds|pos|mpos|payment|tablet
  "restaurant_id": "uuid",           // site the devices are deployed to
  "group_id": "uuid",
  "expires_days": 30,
  "max_enrolls": 5
}
```
Response `201`: `{ "profile": { "id", "name", "token", "active", … } }`. The token is
minted server-side and never taken from the request. It is what an MDM-lite host app
bakes into `BuildConfig.MDM_ENROLL_TOKEN` at compile time, which is why this exists as
an API rather than a dashboard-only form.

`POST /commands` request:
```json
{
  "type": "install_apk",             // install_apk|shell|screenshot|reboot|ota|update_splash|adb_tcp (default install_apk)
  "apk_url": "string",               // required for install_apk
  "payload": {},                     // type-specific; update_splash: {"url":"...","partition_size":N}
  "target_type": "devices",          // required: all|devices|groups
  "targets": ["serial-or-group-id"]  // required unless target_type=all
}
```
`adb_tcp` switches wireless adb on a firmware device: payload `{"port": 5555, "hours": 24}` (port `0` = off, hours `0` = stays on; defaults 5555 / 24). Exactly one named device per command (`target_type: "devices"`, one target) — anything else is `400`. Needs client 1.4.9+ on firmware v2.1.022+; an older image answers `failed` saying so. adbd still requires an authorized key.

Response `201`: `{ "command": {…}, "skipped": ["serial", …] }`. If every target already has that app installing, returns `200` with `{ "created": false, "skipped": [...], "message": "…" }`.

`GET /commands/{id}` → `{ "command": {…}, "deliveries": [ { "device_id","serial_number","status","progress","updated_at","output","last_seen_at","online" } ] }`.

> ⚠ **Security note — `type: "shell"` and `screenshot`/`ota`/`update_splash`** are all reachable with the single admin key. `shell` yields arbitrary command execution on every targeted device. This is the highest-value target on the admin surface: compromise of `ADMIN_API_KEY` = fleet-wide RCE.

---

## 3b. Testing team API (`X-API-Key: <QA_API_KEY>`, read-only)

A separate key for the testing team's own tools. It reaches the `GET /api/v1/testdata/…`
routes and nothing else, so a leaked key can read data but cannot command a device.
Unset `QA_API_KEY` and the routes do not exist (404). It is set on **live**
(`https://mdm.dev.aioapp.com`), not on stage. CORS is open (`*`), so a static page can call it
from a browser.

**Scope:** firmware devices (T7, Kiosk) only. A serial outside that scope answers 404, like an
unknown one. Lists take `?group=<name>` and/or `?restaurant=<name>` (exact name, any case;
404 when there is no such name).

| Method | Path | What |
|---|---|---|
| GET | `/api/v1/testdata/filters` | `{"groups":[…],"restaurants":[…]}`: the names the filters accept |
| GET | `/api/v1/testdata/devices` | One row per device: latest snapshot, plus `extra` (the whole last report, as sent) |
| GET | `/api/v1/testdata/devices/{serial}/history` | Check-in history: `start`, `end` (RFC 3339, default the last 7 days), `interval_sec` (default 300), `cycles=true` |
| GET | `/api/v1/testdata/battery` | Devices that sent battery rows in the last 7 days, most rows first |
| GET | `/api/v1/testdata/devices/{serial}/battery` | That device's battery rows, oldest first. `?format=csv` for CSV |
| GET | `/api/v1/testdata/devices/{serial}/battery/climbs` | Its charge climbs, newest first. `?limit=` (default 50, max 500) |

`/devices` row: `serial_number, device_class, restaurant_name, groups[], last_seen_at, battery_pct,
battery_temp_c, charging, wlc_state, charging_pad, ram_used_pct, storage_free_pct,
discharge_total_pct, discharge_backfilled, extra`. `discharge_total_pct / 100` = equivalent
full cycles. New fields the client starts sending show up under `extra` without a server change.

`/history` row: `serial_number, battery_pct, build_id, extra, timestamp, sample_at, last_seen_at, empty`.

**Battery data** (the dashboard's `/battery` page; T7 firmware client 1.10.1+). The client logs
every battery broadcast and screen edge, at most one row per 30 s while nothing changes, and
uploads them in batches. Battery rows can therefore arrive minutes to hours after they were taken.

`/battery` row: `serial_number, restaurant_name, rows, first_at, last_at, last_mv, last_pct, climbs`.

`/devices/{serial}/battery`: choose the window with `?range=6h|24h|7d|30d` (default 24h) or
`?start=&end=` (RFC 3339, at most 31 days; 400 otherwise). The answer is
`{serial_number, start, end, count, rows:[…]}`. Each row has these fields; `null` means the
device did not read that value:

| Field | Meaning |
|---|---|
| `t`, `t_ms` | When the row was taken, by the **device's** clock |
| `t_fix_ms` | Server clock minus device clock when the batch arrived; `t_ms + t_fix_ms` ≈ server time |
| `up_s` | Device uptime, seconds (resets on reboot) |
| `ev` | `b` battery broadcast, `s` screen on/off edge |
| `mv`, `pct` | Cell voltage (mV), charge (%) |
| `status` | Android `BatteryManager` status: 1 unknown, 2 charging, 3 discharging, 4 not charging, 5 full |
| `plug` | 0 none, 1 AC, 2 USB, 4 wireless |
| `temp_c` | Battery temperature, °C |
| `screen` | Screen on |
| `ichg_ma`, `iin_ma` | Charge current and charger input current, mA (while charging) |
| `flap` | The charger was toggling on and off (a faulty cable, port or supply) |
| `wlc`, `wlc_on` | Guest charging pad: its state as last read, and its saved on/off switch |

`?format=csv` returns the same columns as the page's Download CSV (`serial, t_ms, up_s, ev, mv,
pct, status, plug, temp_c, screen, ichg_ma, iin_ma, flap, wlc, wlc_on, t_fix_ms`). Unknown values
are empty cells.

`/climbs` item: `{at, received_at, payload}`. `payload` is the client's `battery_cc` object as
sent. One climb is one charge, read as a ladder of voltage rungs: `mv0` is the first rung (mV),
`step` the mV between rungs, and `s[i]` the seconds from the start until the cell reached
`mv0 + i·step`, so a slower climb shows as larger gaps. `scr[i]` is whether the screen was on
at that rung. `end` is how the climb ended (`top` = reached full), `plug` the charger, `ichg_ma` and
`temp_c` the [min, max] over the climb, `iin_ma` the input current (-1 = not read), `gap_s` the
longest gap between samples, `n` the samples behind it, and `at` the time (device ms).

Errors are JSON `{"error":"…"}`: 401 bad or missing key, 404 unknown device/group/restaurant,
400 bad window.

---

## 4. Remote-control WebSocket — GET `/api/v1/remote/{serial}`

> ⚠ **This endpoint has NO API-key middleware.** It is registered in `main.go` as a bare `http.HandlerFunc` (line ~200). Authentication is a **single-use token** passed as a query parameter and redeemed against an in-memory `remote.Manager`.

- Query: `serial` (path), `token` (query string).
- The token is minted by an authenticated **dashboard session** and is single-use.
- On connect the handler redeems the token, extracts the bound device ID, and requires the path `serial` to match the token's device — cross-device hijack is rejected.
- The target device must currently be connected over its device WS (`503` otherwise).
- On success: relays binary capture frames device→browser and JSON input events browser→device; sends the device a `start_capture` control message.

Status: `101` · `400` missing serial · `401` invalid/expired/mismatched token · `404` device not found · `503` device offline.

Pen-test angles worth probing: token entropy and lifetime, single-use enforcement under races, whether `token` leaks via logs/referrer, and whether the query-string token is bound to the requesting IP/session.

---

## 4b. adb tunnel, device leg — GET `/api/v1/adb-tunnel/{session}/{stream}?serial=…` (WebSocket upgrade)

Device auth (`X-API-Key`, bound identity enforced like `/api/v1/ws`). The server asks for
it with an `adb_tunnel_open` frame on the command socket (`{"session","stream","port"}`)
after an admin's `adb connect` reached the session's listener; the agent dials
`127.0.0.1:port` (adbd) and carries it here as binary frames, both ways, until either end
closes. `adb_tunnel_close` on the command socket drops every stream of a session. A dial
failure is reported as one text frame `{"error":"…"}` before closing. Sessions are opened
and ended from the dashboard's Wireless adb page (`/adb`, admin only); the listener is a
port from `ADB_TUNNEL_PORTS` that accepts only the opener's IP. See `internal/adbtunnel`.

Status: `101` · `400` bad serial · `401` · `404` unknown/finished session, or another device's.

---

## 5. WebSocket message protocol

Device connects to `/api/v1/ws?serial=…`. Frames are JSON text frames dispatched by a top-level `"type"` field (`cmd/server/main.go`), except binary frames (type 2) which carry remote-control capture data.

### Device → Server
| `type` | Handler | Payload (beyond `type`) |
|---|---|---|
| `telemetry` | `HandleWsTelemetry` | `serial_number`*, `build_id`*, `battery_pct`, `extra`, `installed_apps[]`, `ota_progress` (delta merged into stored snapshot) |
| `command_ack` | `HandleWsCommandAck` | `command_id`*, `status`* (downloading\|installing\|installed\|failed\|completed), `output`, `progress`, `package` |
| `logcat_result` | `HandleWsLogcat` | `request_id`*, `content`* |
| `ota_status` | `HandleWsOtaStatus` | `command_id`*, `status`* (downloaded\|installed\|error), `error_code` |
| `pong_response` | hub `SignalPong` | `nonce`* (echo of a `ping_request`) |
| `logcat_stream` / `logcat_stream_end` | `logstream.Manager` | streaming logcat frames |
| *(any other / unrecognised type)* | `shell.Manager` | **interactive shell I/O** — note the default branch routes unknown frames to the shell manager |

`*` = required.

### Server → Device
| `type` | Meaning |
|---|---|
| `telemetry_request` | Ask device to send a `telemetry` frame (sent on connect + every `checkin_interval_seconds`) |
| `ping_request` + `nonce` | Liveness probe; device must reply `pong_response` with same nonce (~5 s window) |
| `command` | `{id, command_type, apk_url?, payload?}` — a command to execute |
| `logcat_request` | `{id, level, lines, tag}` |
| `config` | `{kiosk_enabled, kiosk_package, kiosk_features, checkin_interval_seconds, lost_listen, self_lock}` — `lost_listen` and `self_lock` always present (see Lost-device reporting above) |
| `checkin_now` | Check in now — sent to a venue's devices when one there is marked lost or lifted |
| `guest` | `{guest: {restaurant_name, wifi_ssid, wifi_password, wifi_security, guest_app_package}}` — firmware devices only; sent on connect and whenever the venue's name or guest Wi-Fi, or the device's venue changes. Its own type, not a partial `config`, because a `config` frame is applied as a whole (a missing `kiosk_enabled` reads as kiosk off). Older clients log it as an unknown type and ignore it. |
| `start_capture` | `{quality, scale, max_fps}` — begins remote-control capture |

Pen-test angles: the `default` dispatch to the shell manager means any authenticated device socket can drive shell I/O framing — verify a device can only affect its own session, and that the WS upgrade validates `Origin` (Go's default gorilla upgrader behaviour applies).

---

## 6. Unauthenticated endpoints

| Path | Behaviour / rationale |
|---|---|
| `GET /health` | `{"status":"ok"}` / `503 {"status":"error"}`. Body deliberately minimal — does not disclose DB backend or version. |
| `GET /static/…` | Static assets, 7-day immutable cache. Directory autoindex is **disabled** (paths ending `/` → 404) to prevent tree enumeration. |
| `GET /splash-img/…` | Device-downloaded splash images on a persistent volume. Unauthenticated by design; filenames are unguessable UUIDs. Probe for UUID predictability / IDOR. |
| `GET /login`, `POST /login`, `POST /logout`, `GET /wrapped` | Dashboard public routes — see `AUTH_MODEL.md`. |

---

## 7. Prioritised test checklist

1. **`ADMIN_API_KEY` = fleet RCE** via `POST /api/v1/commands {type:"shell"}`. Confirm key storage, rotation, and transport (TLS) hygiene.
2. **`/api/v1/remote/{serial}` token** — the only auth-key-free endpoint. Entropy, single-use, expiry, binding, leakage.
3. **Rate-limit bypass** — `ClientIP()` honours `X-Forwarded-For`; if the server is reachable without a trusted proxy stripping/setting it, per-IP limits (API 30/min, login 8/15min) are spoofable. See `AUTH_MODEL.md §3`.
4. **Key-class confusion** — device vs admin keys return identical `401` bodies; verify no side-channel (timing, headers) distinguishes them.
5. **IDOR on `{serial}` / `{id}`** across devices/groups/productions/commands with a valid admin key (expected: full access — confirm no tenant isolation is assumed).
6. **Device-scoped authorization** — `403` on acking/OTA-reporting a command that doesn't target the caller; try to spoof `serial_number` in the body vs the authenticated socket.
7. **gzip request bomb** via `DecompressRequest` (`Content-Encoding: gzip`) — decompression limits / DoS.


## Enrollment (DPC agent)

`POST /api/v1/enroll` — unauthenticated; the profile token is the credential.
Rate-limited per source IP on failures.

Request:

```json
{ "token": "enr_…", "serial": "R58N30ABC123", "product": "gta9pwifi" }
```

Response `200`:

```json
{
  "device_key":   "dvk_…",          // use as X-API-Key from now on
  "device_id":    "uuid",
  "profile":      "mPOS rollout, Downtown",
  "device_class": "mpos",           // "" when the profile sets none
  "site":         "Bayside Pizza",  // "" when the profile sets none
  "group":        "",               // auto-joined group, if any
  "re_enrolled":  false,            // true when the serial existed (key rotated)
  "onboarded":    true              // profile named a site → skipped the inbox
}
```

`401` invalid, revoked, expired or exhausted token (indistinguishable on purpose).

Check-in (`POST /api/v1/checkin`) keyframes may carry `extra.agent_type` (`"dpc"`),
`extra.capabilities` (array of capability names, see `internal/product/caps.go`) and
`extra.capabilities_degraded`. They are persisted on the device and drive which
actions the dashboard offers.


## Fleet adb key (admin key)

The one adb key pair the AIO firmware trusts, handed to the AIO Enroll Android app after
sign-in so it can `adb connect` to firmware devices without a pairing code. The private
key is sealed at rest with `FLEET_ADB_KEY_SECRET` (server env) and is never returned by
the admin API; only the app endpoint returns it, audited and rate-limited.

| Method | Path | Body / result |
|---|---|---|
| `PUT` | `/api/v1/fleet-adb-key` | `{"private_key_pem","public_key"}` → `{"version","fingerprint"}`. RSA 2048-bit or larger; `public_key` is adb's one-line `adbkey.pub`. `503` when the secret is unset. |
| `GET` | `/api/v1/fleet-adb-key` | `{"version","fingerprint","uploaded_by","uploaded_at"}`, `404` when none. |
| `DELETE` | `/api/v1/fleet-adb-key` | `{"removed":true,"version"}`. |

The fingerprint is the first 12 hex characters of SHA-256 over the decoded `adbkey.pub` blob.

App side (bearer session, operator or above): `GET /api/v1/app/adb-key` →
`{"version","fingerprint","private_key_pem","public_key"}`, `Cache-Control: no-store`,
`404` when none, `429` past 10 fetches an hour per person. Every fetch is written to the
audit log (`fleet_adb_key.fetch`) with the user, the client IP and the `X-AIO-Device`
header. `GET /api/v1/app/me` carries `adb_key_version` (0 when none) so the app knows
when to re-fetch.
