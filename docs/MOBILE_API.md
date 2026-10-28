# AIO MDM mobile API (`/api/v1/m/*`)

The JSON API behind the AIO MDM Android app (A1 · Tabs, picked 29 Oct 2026). Lean on purpose:
navigating the fleet and the everyday actions; shell, remote control, releases, app library,
rules, users and settings stay on the web.

Every call except `login` takes `Authorization: Bearer <token>`. The token is a normal
dashboard session, so a person sees and may do exactly what they see and may do on the web:
the same roles, access rules and venue walls (customers included). Errors are
`{"error": "<message for a person>"}` with a 4xx/5xx status; 401 means sign in again.

Times are RFC 3339 (UTC). Absent values are `null`, never `""` or `0` standing in for "unknown".

## Sign-in

`POST /api/v1/m/login` `{"username","password"}` →
`{"token","username","display_name","role","role_label","expires_at"}`

Microsoft (PKCE, so a code caught by another app that registered the scheme is useless):
the app makes a `verifier` (43–64 URL-safe characters) and `challenge` =
base64url(SHA-256(verifier)) without padding, then opens
`{server}/auth/microsoft?app=1&challenge=<challenge>` in a Custom Tab. On success the server
sends the browser to `aiomdm://signin?code=<one-time code>` (2 minutes, single use); on failure
to `aiomdm://signin?error=<message>`. The app trades the code:

`POST /api/v1/m/login/microsoft` `{"code","verifier"}` → the same answer as `login`.

`POST /api/v1/m/logout` → `{"ok":true}`

`GET /api/v1/m/me` → `{"username","display_name","role","role_label","web_url"}`

## Home

`GET /api/v1/m/home` →
```
{"name": "Ali",                                   // first name for the greeting
 "health": {"score": 80, "label": "healthy"},     // healthy | watch | at_risk | none
 "todo": [{"tone": "critical", "title": "Huanchaco: 7 offline", "sub": "since 2h · open now",
           "target": {"kind": "venue", "id": "<uuid>"}}],   // kind: device (id = serial) | venue | alert | web (url)
 "todo_total": 9,
 "stats": {"devices": 125, "online": 50, "online_pct": 40, "open_alerts": 9},
 "venues": [VenueRow, ...]}                       // worst first, at most 8
```

## Devices

`GET /api/v1/m/devices?q=&class=&online=&venue=&page=` (online: `online|offline`; class: a
class key; venue: a venue id; page from 1, 50 per page) →
`{"devices":[DeviceRow], "total": 125, "page": 1, "per_page": 50,
  "classes":[{"key":"t7","label":"Tableside AI","count":98}]}`

DeviceRow:
```
{"serial","name","class","class_label","model","venue":{"id","name"}|null,
 "online":true,"last_seen","battery_pct":84|null,"charging":true|null,
 "locked":true,"build","client_version"}
```

`GET /api/v1/m/devices/{serial}` →
```
{"device": DeviceRow + {"temp_c":34.2|null,"temp_src":"battery|cpu"|null,
            "wifi":{"ssid","rssi"}|null,"on_screen":"Nugget"|null,"locked_to":"aio.app.nugget"|null,
            "lost":false,"location":{"lat","lon","accuracy_m"}|null,"ip":"10.32.1.84"|null},
 "needs":[{"tone":"critical|warning|info","title","why"}],
 "alerts":[AlertRow],
 "recent":[{"at","title","sub","tone":"ok|warning|critical|info"}],   // newest first, at most 10
 "actions":{"reboot":true,"lock":true,"unlock":false,"screenshot":true,"locate":true},
 "lock_choices":[{"package","label","on_screen"}],   // only when several builds of the role's app are installed; first = default
 "web_url":"https://…/devices/AT070…"}
```

`POST /api/v1/m/devices/{serial}/actions` `{"action":"reboot|lock|unlock|screenshot","reason"?,"package"?}` →
`{"ok":true,"command_id":"<uuid>"|null,"message":"Rebooting"}`. Locate needs no call: the app
opens the device's `location` in a maps app. `reason` is required for a reboot when the fleet
asks for one (400 "A reason is required for this command."); `package` picks a `lock_choices`
build. Lock and unlock answer once the device has the change, or say it applies when it is back.

`GET /api/v1/m/commands/{id}` → `{"status":"pending|sent|running|completed|failed|expired",
"output":"…","screenshot_url":"/api/v1/m/commands/{id}/screenshot/{serial}"|null}`

`GET /api/v1/m/commands/{id}/screenshot/{serial}` → the PNG (bearer).

## Alerts

`GET /api/v1/m/alerts?state=open|acknowledged|snoozed&severity=&since=` →
`{"alerts":[AlertRow], "counts":{"open":9,"acknowledged":3,"snoozed":1}}`

AlertRow: `{"id","severity":"critical|warning|info","title","summary",
"device":{"serial","name"}|null,"venue":{"id","name"}|null,"fired_at","state"}`

`POST /api/v1/m/alerts/{id}/ack` → `{"ok":true}`
`POST /api/v1/m/alerts/{id}/snooze` `{"hours":1|4|24}` → `{"ok":true}`

The app's background check (every 15 minutes) asks for `state=open&severity=critical&since=<last check>`
and posts a notification per new alert.

## Venues

`GET /api/v1/m/venues` → `{"venues":[VenueRow]}` (worst first)

VenueRow: `{"id","name","initial":"H","icon_url":"/api/v1/m/venues/{id}/icon"|null,
"health":{"score","label"},"open_now":true|null,"devices":9,"online":2,"alerts":1,"hot":0}`

`GET /api/v1/m/venues/{id}` → `{"venue": VenueRow + {"address","timezone"}, "devices":[DeviceRow]}`

`GET /api/v1/m/venues/{id}/icon` → the venue's PNG icon (bearer); 404 when it has none.

## The app

AIO MDM for Android (`com.aioapp.mdmapp`; stage build `.stage`), hosted for download in the
`mdm-android` slot: `{server}/agent/aio-mdm-android.apk`.
