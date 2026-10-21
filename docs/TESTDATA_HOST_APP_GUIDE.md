# Testing API: integration update, host app on screen (21 Oct 2026)

Two new read-only routes on the testing team's API tell you **when each tablet's host app
(Nugget, the kiosk app, …) was on screen and when it was not**. Nothing existing changed: the
current `/api/v1/testdata/…` routes, fields and errors are as before, so no change is needed
on your side unless you want the new data.

## What you need

| | |
|---|---|
| Base URL | `https://mdm.dev.aioapp.com` (live; the testing API is not on stage) |
| Auth | Header `X-API-Key: <your QA key>`, the same key you already use |
| Method | `GET` only. CORS is open, so a browser page can call it |
| Scope | Firmware devices (T7, Kiosk), like the other testdata routes |
| Data | Devices on firmware client **1.11.0 or newer**; history is kept from **17 Oct 2026** |

## The two routes

### 1. Every device, state now: `GET /api/v1/testdata/host-app`

One row per device that reports a host app, with what is on screen right now and the share of
the window the app was on screen.

Query (all optional):

| Param | Meaning |
|---|---|
| `range` | `6h`, `24h` (default) or `7d` |
| `start`, `end` | RFC 3339 instead of `range`, at most 7 days apart |
| `group`, `restaurant` | Exact name (any case), as on the other list routes |

```bash
curl -s -H "X-API-Key: $QA_KEY" \
  "https://mdm.dev.aioapp.com/api/v1/testdata/host-app?restaurant=Flights%20Vegas&range=24h"
```

```json
{
  "start": "2026-10-20T13:00:00Z",
  "end": "2026-10-21T13:00:00Z",
  "count": 1,
  "devices": [
    {
      "serial_number": "AT070AABU00601",
      "restaurant_name": "Flights Vegas",
      "package": "aio.app.nugget",
      "last_seen_at": "2026-10-21T12:59:41Z",
      "state": "off_screen",
      "why": "app_not_in_foreground",
      "since": "2026-10-21T12:31:02Z",
      "on_screen_s": 61200,
      "off_screen_s": 9000,
      "unknown_s": 16200,
      "on_screen_pct": 87
    }
  ]
}
```

### 2. One device, the timeline: `GET /api/v1/testdata/devices/{serial}/host-app`

Every stretch of the window as a span, with totals and the raw transitions behind them.

Query (all optional):

| Param | Meaning |
|---|---|
| `range` | `6h`, `24h` (default), `7d` or `30d` |
| `start`, `end` | RFC 3339 instead of `range`, at most 31 days apart |

```bash
curl -s -H "X-API-Key: $QA_KEY" \
  "https://mdm.dev.aioapp.com/api/v1/testdata/devices/AT070AABU00601/host-app?range=6h"
```

```json
{
  "serial_number": "AT070AABU00601",
  "package": "aio.app.nugget",
  "start": "2026-10-21T07:00:00Z",
  "end": "2026-10-21T13:00:00Z",
  "last_seen_at": "2026-10-21T12:59:41Z",
  "now": { "state": "off_screen", "why": "app_not_in_foreground", "since": "2026-10-21T12:31:02Z" },
  "totals": { "on_screen_s": 18362, "off_screen_s": 3238, "unknown_s": 0, "on_screen_pct": 85 },
  "spans": [
    { "from": "2026-10-21T07:00:00Z", "to": "2026-10-21T10:15:00Z", "state": "on_screen",  "why": "app_in_foreground",     "seconds": 11700 },
    { "from": "2026-10-21T10:15:00Z", "to": "2026-10-21T10:40:00Z", "state": "off_screen", "why": "screen_off",            "seconds": 1500 },
    { "from": "2026-10-21T10:40:00Z", "to": "2026-10-21T12:31:02Z", "state": "on_screen",  "why": "app_in_foreground",     "seconds": 6662 },
    { "from": "2026-10-21T12:31:02Z", "to": "2026-10-21T13:00:00Z", "state": "off_screen", "why": "app_not_in_foreground", "seconds": 1738 }
  ],
  "events": [
    { "t": "2026-10-21T10:15:00Z", "key": "screen_on",   "to": false },
    { "t": "2026-10-21T10:40:00Z", "key": "screen_on",   "to": true },
    { "t": "2026-10-21T12:31:02Z", "key": "host_app_fg", "to": false }
  ]
}
```

(Example values are illustrative; the shape is exact.)

## What the states mean

| `state` | `why` | Meaning |
|---|---|---|
| `on_screen` | `app_in_foreground` | The host app is in front and the screen is on |
| `off_screen` | `app_not_in_foreground` | Something else is in front: the launcher, Settings, another app |
| `off_screen` | `screen_off` | The host app is in front but the screen is off |
| `unknown` | `no_report` | No host app report for that time (client older than 1.11.0, or before 17 Oct) |
| `unknown` | `device_offline` | After the device's last check-in, while it is offline |

- Spans cover the whole window with no gaps or overlaps, oldest first, so you can draw them
  straight onto a timeline: `from`/`to` are server time (UTC).
- `on_screen_pct` is the share of the **known** time (`on_screen_s / (on_screen_s +
  off_screen_s)`), so a device that was offline half the day is not counted as "off screen" for
  that half. It is `null` when nothing is known.
- `now.since` is when the current state began, or the window's start if it began earlier.
- `package` is the variant in use (e.g. `aio.app.nugget` or `aio.app.nugget.qa`).
- `events` are the transitions the spans are built from: `host_app_fg` (the app in front or
  not) and `screen_on`. You only need them if you want to build your own view.

## Things to know

- **Freshness.** A change reaches the server with the device's next check-in, so it shows up
  here within a check-in interval (a minute or two for a connected tablet). Polling the list once
  a minute is plenty.
- **Offline.** A device that has been silent for more than three check-in intervals is
  `device_offline` from its last check-in onward. A short drop-out in the middle of a window is
  not detected: the state before it is assumed to have held.
- **Old clients.** A device on a client older than 1.11.0 does not report a host app, so it is
  left out of the list and its timeline is `unknown · no_report`. Most v2.0.997 tablets moved
  to client 1.14.0 on 21 Oct 2026.
- **Which app.** The host app is the one named for the device's role on the dashboard's Lineup
  page (Tableside AI → Nugget, Kiosk → the kiosk app, …). A device whose role has no app reports
  none.

## Errors

JSON `{"error": "…"}`, as on the other routes:

| Status | When |
|---|---|
| 400 | Bad `range`/`start`/`end`, end before start, or a window over 31 days (7 on the list) |
| 401 | Missing or wrong `X-API-Key` |
| 404 | Unknown serial (or one outside firmware devices), group or restaurant |

The full reference for every testing route is `docs/API_REFERENCE.md`, section 3b.
