# Client-Side WebSocket Migration Guide

## Overview

Commands and logcat requests are no longer delivered in the checkin response.
The server now pushes them over a persistent WebSocket connection.

Devices must establish a WebSocket connection on startup and maintain it.
The checkin endpoint remains but is now telemetry-only.

---

## What Changed on the Server

| Before | After |
|--------|-------|
| `POST /api/v1/checkin` returns `commands[]` and `logcat_requests[]` | `POST /api/v1/checkin` returns only `config` |
| `poll_interval_ms` in checkin response | Removed — no command polling |
| Device polls every N seconds to get commands | Server pushes commands instantly over WS |

---

## New Endpoint

```
GET /api/v1/ws?serial=<serial_number>
Header: X-API-Key: <api_key>
Protocol: WebSocket (ws:// or wss://)
```

The `serial` query parameter must match the device's serial number as registered
via checkin. Authentication uses the same `X-API-Key` header as all other endpoints.

---

## Connection Lifecycle

1. Device opens WebSocket connection to `ws://<host>/api/v1/ws?serial=<SERIAL>`
2. Server immediately flushes any commands or logcat requests queued while the device was offline
3. Connection stays open indefinitely — server sends pings every 45 seconds; device must respond with pong
4. Device receives push messages as they are created
5. On disconnect, reconnect with exponential backoff (see below)

---

## Message Format — Server → Device

All messages are JSON text frames.

### Command

```json
{
  "type": "command",
  "id": "550e8400-e29b-41d4-a716-446655440000",
  "command_type": "install_apk",
  "apk_url": "https://example.com/app.apk",
  "payload": {}
}
```

`command_type` values:

| Value | Description |
|-------|-------------|
| `install_apk` | Download and install APK from `apk_url` |
| `shell` | Run shell command; `payload.cmd` contains the command string |
| `screenshot` | Capture and upload a screenshot |
| `reboot` | Reboot the device immediately |
| `ota` | Apply OTA update; see payload fields below |

OTA payload fields:

```json
{
  "package_id": 3,
  "build_id": "aosp-eng 14 UP1A.231005.007",
  "update_url": "https://example.com/ota.zip",
  "payload_offset": 1234,
  "payload_size": 56789012,
  "payload_headers": ["Content-Type: application/zip"]
}
```

### Logcat Request

```json
{
  "type": "logcat_request",
  "id": "550e8400-e29b-41d4-a716-446655440001",
  "level": "W",
  "lines": 500,
  "tag": ""
}
```

`level` is one of `V`, `D`, `I`, `W`, `E`.
`tag` is an optional logcat tag filter; empty string means no filter.

---

## Device Must Still Do

These REST endpoints are unchanged and still required:

| Endpoint | Purpose |
|----------|---------|
| `POST /api/v1/checkin` | Send telemetry (battery, build, installed apps, extras) |
| `POST /api/v1/commands/{id}/ack` | Report command result (`installed`, `failed`, `completed`) |
| `POST /api/v1/logcat` | Submit captured logcat content |
| `POST /api/v1/ota/status` | Report OTA download/install/error status |

### Updated Checkin Response

The checkin response no longer contains `commands` or `logcat_requests`.
It now returns only:

```json
{
  "status": "ok",
  "config": {
    "kiosk_enabled": false,
    "kiosk_package": "",
    "kiosk_features": 1
  }
}
```

Process the `config` block on every checkin as before.

---

## Reconnection Strategy

The device must reconnect automatically when the WebSocket drops.

Recommended backoff:

```
attempt 1:  retry after 1s
attempt 2:  retry after 2s
attempt 3:  retry after 4s
attempt 4:  retry after 8s
attempt 5+: retry after 30s (cap)
```

Reset the backoff counter after a connection stays alive for at least 60 seconds.

Any commands that were created while the device was disconnected will be
delivered automatically as soon as the new connection is established — no
special handling needed on reconnect.

---

## Ping / Pong

The server sends a WebSocket `ping` frame every 45 seconds.
The device must respond with a `pong` frame.

Most WebSocket client libraries handle this automatically. If using a raw
implementation, register a pong handler and reset the read deadline on receipt.

If the device does not respond to 2 consecutive pings (within ~60 seconds),
the server closes the connection.

---

## Command Acknowledgement

After executing a command, POST the result to `POST /api/v1/commands/{id}/ack`:

```json
{
  "serial_number": "DEVICE-001",
  "status": "installed",
  "output": ""
}
```

`status` values: `installed` (success), `failed`, `completed` (for commands
with no binary success/fail, e.g. screenshot).

`reboot` commands do not need an explicit ack — the server marks them
completed at delivery time.

---

## Logcat Submission

After capturing logcat output, POST to `POST /api/v1/logcat`:

```json
{
  "serial_number": "DEVICE-001",
  "request_id": "550e8400-e29b-41d4-a716-446655440001",
  "content": "<raw logcat output>"
}
```

---

## Implementation Checklist

- [ ] On app start, open WebSocket to `ws://<host>/api/v1/ws?serial=<SERIAL>` with `X-API-Key` header
- [ ] Implement reconnect loop with exponential backoff
- [ ] Parse incoming JSON frames and dispatch on `type` field
- [ ] Execute commands received via `type: command`
- [ ] Capture and submit logcat when `type: logcat_request` is received
- [ ] POST ack to `/api/v1/commands/{id}/ack` after each command completes
- [ ] Continue sending `POST /api/v1/checkin` for telemetry at existing interval
- [ ] Remove any code that reads `commands[]` or `logcat_requests[]` from checkin response
- [ ] Remove any code that uses `poll_interval_ms` from checkin response
