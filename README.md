# AIO-MDM

MDM (Mobile Device Management) server for a fleet of custom AOSP Android kiosk devices. Devices hold a WebSocket to the server (falling back to periodic HTTP check-ins), report telemetry, and receive commands, app installs and OTA updates. Operators work from a web dashboard; build machines and scripts use the admin REST API.

## Features

- **Fleet** — device list and detail pages, telemetry history, battery/charging, network, map and WiFi-based geolocation, fleet health and a Daily Report
- **Actions** — reboot, shell, logcat (including live), APK install/uninstall, managed configs, custom payloads; live delivery over WebSocket with per-device progress and history
- **Apps** — APK library backed by S3, versions and deployments
- **Software updates** — releases with full and incremental OTA packages, staged rollouts with live download progress, update policies, and a legacy OTA listener for pre-agent builds
- **Organisation** — groups, restaurants, productions, schedules, geofencing, compliance and alerts
- **Remote access** — remote screen and shell sessions; offline kiosk-exit codes (TOTP)
- **Accounts** — email accounts with roles, self sign-up and password reset (SES), Sign in with Microsoft, and an activity log of who did what
- **AI analysis** — optional report summaries via Anthropic, DeepSeek or OpenAI
- **Dashboard** — HTMX, installable as a PWA, light/dark theme; release notes in [CHANGELOG.md](CHANGELOG.md)

## Tech Stack

Go 1.24 · PostgreSQL 17 · HTMX · Docker · AWS (S3, SES)

## The clients

This repo is the server **and** the umbrella for the device agents, which are checked
out as submodules. One clone gets the whole MDM:

```bash
git clone --recurse-submodules git@github.com:AIOApp/mdm.git
git submodule update --init            # existing clone
git submodule update --remote          # advance each to its branch tip
```

| Path | Agent | Package | Branch |
|---|---|---|---|
| `aio-mdm-client/` | Firmware client — platform-signed system app in our AOSP images | `com.aioapp.mdm` | `main` |
| `aio-mdm-client-dpc/` | DPC agent — Device Owner on stock Android. Paused | `aio.app.mdmclient.dpc` | `menu-board` |
| `aio-mdm-lite/` | MDM-lite AAR — embeds in an app we already ship, no Device Owner | `AioMdm` | `main` |
| `aio-mdm-client-lite/` | Standalone Lite test app, builds against `../aio-mdm-lite` | | `main` |

They live on the `forgedynasties` remote and resolve through the `Host forgedynasties`
entry in `~/.ssh/config`. `aio-mdm-client-lite` builds the library straight out of
`../aio-mdm-lite/mdm-lite`, so those two must stay siblings — do not rename the paths.

None of them are built into the server image; `.dockerignore` keeps them out of the
build context. Nothing in CI checks them out either, so they cannot affect a deploy.
Commit and push **inside** a submodule first — the pointer commit here names a SHA and
means nothing until that SHA is on its remote.


## Quick Start

```bash
cp .env.example .env
# Edit .env — at minimum set DEVICE_API_KEY, ADMIN_API_KEY, DASHBOARD_PASSWORD
# and SESSION_SECRET (>=32 bytes, distinct from both API keys: openssl rand -hex 32)

docker compose up -d --build
```

Dashboard: `http://localhost:8082` (compose default; the bare binary defaults to `8080`).

The server refuses to start without `DEVICE_API_KEY`, `ADMIN_API_KEY`, `DASHBOARD_PASSWORD` and a valid `SESSION_SECRET`.

## Configuration

`.env.example` documents every variable in detail. Summary:

| Variable | Description | Default |
|---|---|---|
| `PORT` | HTTP port | `8080` (`8082` in compose) |
| `DB_HOST` / `DB_PORT` / `DB_USER` / `DB_PASSWORD` / `DB_NAME` | PostgreSQL connection | `localhost` / `5432` / `mdm` / `mdm` / `mdm` |
| `DEVICE_API_KEY` | `X-API-Key` for device endpoints | **required** |
| `ADMIN_API_KEY` | `X-API-Key` for admin REST endpoints | **required** |
| `DASHBOARD_USER` / `DASHBOARD_PASSWORD` | Bootstrap dashboard login | `admin` / **required** |
| `SESSION_SECRET` | Session cookie signing key | **required** |
| `PUBLIC_ORIGIN` | Trusted origin(s) for CSRF checks, comma-separated | request `Host` |
| `PUBLIC_BASE_URL` | Base URL devices use to fetch server-generated assets | derived from request |
| `COOKIE_SECURE` | Set `false` for local HTTP | `true` |
| `CONFIG_PATH` | Persisted settings file | `config/display.json` (`/app/data/config.json` in compose) |
| `AWS_REGION`, `S3_BUCKET`, `S3_PREFIX`, `APK_CACHE_DIR` | APK store on S3 | — |
| `MAIL_FROM_EMAIL` | SES sender for sign-up / reset mail (logged if `AWS_REGION` unset) | `AIO MDM <mdm@dev.aioapp.com>` |
| `MS_CLIENT_ID` / `MS_CLIENT_SECRET` / `MS_TENANT_ID` | Sign in with Microsoft (all three enable it) | — |
| `GOOGLE_GEOLOCATION_API_KEY` | WiFi → lat/lon | — |
| `GOOGLE_GEOCODING_API_KEY` | lat/lon → street address | — |
| `GOOGLE_MAPS_EMBED_API_KEY` | Map on the device page (browser-exposed; restrict by referrer) | — |
| `AI_PROVIDER` / `AI_BASE_URL` / `AI_API_KEY` / `AI_MODEL` / `AI_DIGEST` | AI analysis; overrides Settings | — |
| `AGENT_APK_URL` / `AGENT_APK_DIR` / `AGENT_APK_CHECKSUM` | Agent APK served at enrollment | — |
| `LEGACY_OTA_PORT` | Extra listener for pre-agent OTA clients | off (`8010` in compose) |
| `LEGACY_OTA_UPSTREAM` | Pass-through target for legacy OTA routes | `http://host.docker.internal:8001` |
| `SSRF_ALLOW_CIDRS` | CIDRs exempt from outbound-fetch SSRF guard | — |
| `SPLASH_DIR` | Boot-logo / splash image storage | `data/splash` |

## API

Full reference: [docs/API_REFERENCE.md](docs/API_REFERENCE.md) · [docs/openapi.yaml](docs/openapi.yaml).

Device endpoints take `X-API-Key: $DEVICE_API_KEY`; admin endpoints take `X-API-Key: $ADMIN_API_KEY`.

### Device

| Method | Endpoint | Description |
|---|---|---|
| `POST` | `/api/v1/enroll` | Enroll a device |
| `GET` | `/api/v1/ws` | Device WebSocket (commands pushed live) |
| `POST` | `/api/v1/checkin` | HTTP check-in (telemetry, apps, extras) |
| `POST` | `/api/v1/commands/{id}/ack` | Acknowledge a command result |
| `POST` | `/api/v1/logcat` | Submit captured logcat |
| `POST` | `/api/v1/ota/status` | Report OTA state |
| `POST` | `/api/v1/ota/progress` | Report OTA download progress |

### Admin

| Method | Endpoint | Description |
|---|---|---|
| `GET` | `/api/v1/devices` | List devices |
| `GET` | `/api/v1/devices/{serial}` | Device detail + recent check-ins |
| `POST` | `/api/v1/devices/{serial}/ping` | Ping a connected device |
| `GET` `POST` | `/api/v1/commands` | List / queue commands |
| `GET` | `/api/v1/commands/{id}` | Command status |
| `GET` `POST` | `/api/v1/groups` | List / create groups |
| `GET` `DELETE` | `/api/v1/groups/{id}` | Read / delete a group |
| `POST` | `/api/v1/groups/{id}/devices` | Add a device to a group |
| `DELETE` | `/api/v1/groups/{id}/devices/{serial}` | Remove a device from a group |
| `GET` `POST` | `/api/v1/productions` | List / create productions |
| `GET` `DELETE` | `/api/v1/productions/{id}` | Read / delete a production |
| `GET` `POST` | `/api/v1/releases` | List / get-or-create a release |
| `GET` | `/api/v1/releases/{id}` | Release with its packages |
| `POST` | `/api/v1/releases/{id}/packages` | Attach a full or incremental OTA package |
| `POST` | `/api/v1/releases/{id}/publish` | Publish a release |
| `GET` | `/health` | Health check (no auth) |

### Publishing a build

A build pipeline can put a signed OTA package on the fleet without the dashboard. MDM stores the URL devices download from; it does not host the package.

```bash
H=(-H "X-API-Key: $ADMIN_API_KEY" -H "Content-Type: application/json")

# 1. Get or create the release (200 if it exists, 201 if created; body has "created")
curl -s "${H[@]}" -X POST $MDM/api/v1/releases \
  -d '{"version":"<build-id>","product":"t7","name":"September","changelog":"...","is_dev":true}'

# 2. Attach packages — one active full image, any number of incrementals
curl -s "${H[@]}" -X POST $MDM/api/v1/releases/<id>/packages \
  -d '{"type":"full","update_url":"https://.../full.zip"}'
curl -s "${H[@]}" -X POST $MDM/api/v1/releases/<id>/packages \
  -d '{"type":"incremental","source_build_id":"<previous-build-id>","update_url":"https://.../inc.zip"}'

# 3. Publish — makes it deployable
curl -s "${H[@]}" -X POST $MDM/api/v1/releases/<id>/publish
```

- Re-running is safe: an existing release's `name`, `changelog` and `is_dev` are left alone unless the request sets `"update_meta": true`.
- Attaching a package that already exists (even a yanked one) returns `409` naming the artifact.
- Every call writes an audit row with actor `api`.

## Project Structure

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
scripts/             Diagnostic scripts
tools/               Deploy, enrollment and legacy-OTA cutover scripts; reel/ (demo video tooling)

aio-mdm-client/      Submodule: firmware client (see "The clients")
aio-mdm-client-dpc/  Submodule: DPC agent
aio-mdm-lite/        Submodule: MDM-lite AAR
aio-mdm-client-lite/ Submodule: Lite test app
```

## Development

```bash
# Run locally (requires PostgreSQL)
go build -o server ./cmd/server
COOKIE_SECURE=false ./server

go test ./...
```

Staging deploy: `tools/stage-deploy.sh`. Enroll a device over ADB: `tools/enroll-adb.sh`.
