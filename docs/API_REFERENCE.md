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
    "kiosk_features": 0, "checkin_interval_seconds": 0
  }
}
```
Status: `200` ok · `400` missing required fields · `500` internal.

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
  "type": "install_apk",             // install_apk|shell|screenshot|reboot|ota|update_splash (default install_apk)
  "apk_url": "string",               // required for install_apk
  "payload": {},                     // type-specific; update_splash: {"url":"...","partition_size":N}
  "target_type": "devices",          // required: all|devices|groups
  "targets": ["serial-or-group-id"]  // required unless target_type=all
}
```
Response `201`: `{ "command": {…}, "skipped": ["serial", …] }`. If every target already has that app installing, returns `200` with `{ "created": false, "skipped": [...], "message": "…" }`.

`GET /commands/{id}` → `{ "command": {…}, "deliveries": [ { "device_id","serial_number","status","progress","updated_at","output","last_seen_at","online" } ] }`.

> ⚠ **Security note — `type: "shell"` and `screenshot`/`ota`/`update_splash`** are all reachable with the single admin key. `shell` yields arbitrary command execution on every targeted device. This is the highest-value target on the admin surface: compromise of `ADMIN_API_KEY` = fleet-wide RCE.

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
| `config` | `{kiosk_enabled, kiosk_package, kiosk_features, checkin_interval_seconds}` |
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
