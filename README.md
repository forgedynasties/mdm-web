# AIO-MDM

Device-management server for AIO's restaurant fleet — custom AOSP kiosks, tablets,
menu boards, KDS and POS terminals. Devices hold a WebSocket to the server (falling
back to periodic HTTP check-ins), report telemetry, and receive commands, app
installs and OTA updates. Operators work from a web dashboard; build machines and
scripts use the admin REST API.

Go 1.24 · PostgreSQL 17 · HTMX · Docker · AWS (S3, SES)

Telemetry and fleet health · remote commands, shell and live logcat · remote screen ·
APK library and deployments · full and incremental OTA with staged rollouts ·
groups, restaurants, productions and schedules · alerts · accounts with roles.
Release notes in [CHANGELOG.md](CHANGELOG.md).

## The clients

Two kinds of hardware, three tiers of client. How much control we get is decided by
whether we own the firmware, and capability gating in
[`internal/product/caps.go`](internal/product/caps.go) is the source of truth.

**Firmware** — in-house devices (T7, Kiosks) run our own AOSP image, so the client
ships inside it as a platform-signed system app with `android.uid.system`. Kiosk
lock, full firmware OTA, reboot, shell, splash and boot-logo updates, TOTP
kiosk-exit codes. Auto-enrolled on first check-in. The only tier that gets firmware
OTA; since 1.0.0 it also updates itself as an APK.

**DPC** — off-the-shelf devices can't take our image, so this is a normal APK that
becomes Device Owner through Android Enterprise provisioning and manages the device
via `DevicePolicyManager`. More power than Lite, but needs a factory-reset device
with no accounts. **Currently paused.**

**Lite** — a dependency-free AAR embedded in an app we already ship. No Device
Owner, no adb, no root: it reports vitals, crashes, security posture and app
inventory, and accepts a few remote controls (screenshot, WebView reload, clear
cache, update check, restart). HTTP check-in only, no live WebSocket.

All three are checked out here as submodules, so one clone gets the whole MDM:

```bash
git clone --recurse-submodules git@github.com:AIOApp/mdm.git
git submodule update --init            # existing clone
git submodule update --remote          # advance each to its branch tip
```

| Path | Tier | Package | Branch |
|---|---|---|---|
| `aio-mdm-client/` | Firmware | `com.aioapp.mdm` | `main` |
| `aio-mdm-client-dpc/` | DPC — paused | `aio.app.mdmclient.dpc` | `menu-board` |
| `aio-mdm-lite/` | Lite (the AAR) | `AioMdm` | `main` |

Do not rename the paths. None of them are built
into the server image (`.dockerignore` keeps them out of the build context) and
nothing in CI checks them out, so they cannot affect a deploy. Commit and push
**inside** a submodule first: the pointer commit here names a SHA and means nothing
until that SHA is on its remote.

## Running it

```bash
cp .env.example .env    # documents every variable; at minimum set DEVICE_API_KEY,
                        # ADMIN_API_KEY, DASHBOARD_PASSWORD and SESSION_SECRET
docker compose up -d --build
```

Dashboard on `http://localhost:8082`. The server refuses to start without those four.

```bash
go build -o server ./cmd/server && COOKIE_SECURE=false ./server   # local, needs Postgres
go test ./...
```

Staging deploy: `tools/stage-deploy.sh`. Enroll a device over ADB: `tools/enroll-adb.sh`.

## API

Device endpoints take `X-API-Key: $DEVICE_API_KEY`, admin endpoints `$ADMIN_API_KEY`.
Every endpoint, with request and response shapes:
[docs/API_REFERENCE.md](docs/API_REFERENCE.md) · [docs/openapi.yaml](docs/openapi.yaml).

## Layout

```
cmd/server/          Entry point, routing
internal/
  api/               Device + admin REST API, WebSocket, legacy OTA routes
  dashboard/         Web UI handlers + HTMX partials
  db/                Schema, migrations, queries
  middleware/        API key auth, sessions, CSRF, limits
  ws/                Device connection hub
  ota/ otagate/      OTA rollout logic and gating
  apkstore/ apkmeta/ S3 APK library, APK parsing
  remote/ shell/ logstream/   Remote screen, shell, live logcat
  alerts/ notify/ mailer/     Alerting and email
  ai/                AI analysis providers
  geolocate/         WiFi geolocation, reverse geocoding
  product/           Product catalog
  config/ version/ totp/ ratelimit/ safehttp/
templates/           Go HTML templates
static/              CSS, JS, icons, service worker
docker/postgres/     DB init script
docs/                API reference, design and planning docs
tools/               Deploy, enrollment and legacy-OTA cutover scripts
```
