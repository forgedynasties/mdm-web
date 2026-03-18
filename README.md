# AIO-MDM

Lightweight MDM (Mobile Device Management) backend for managing a fleet of custom AOSP Android devices. Devices poll the server every 60 seconds, reporting telemetry and picking up queued commands.

## Features

- **Device telemetry** — battery level, build version, installed apps, extensible JSONB fields
- **Remote commands** — install APKs, reboot, capture logcat, kiosk mode, custom payloads
- **Web dashboard** — real-time device list, battery visualization, search/sort, CSV export
- **Device groups** — tag devices for bulk operations
- **Logcat capture** — request and view device logs filtered by level/tag

## Tech Stack

Go 1.23 · PostgreSQL 17 · HTMX · Docker

## Quick Start

```bash
cp .env.example .env
# Edit .env — set DEVICE_API_KEY and DASHBOARD_PASSWORD at minimum

docker compose up -d --build
```

Dashboard: `http://localhost:8080`

## Configuration

| Variable | Description | Default |
|---|---|---|
| `PORT` | Server port | `8080` |
| `DB_HOST` | PostgreSQL host | `postgres` |
| `DB_PORT` | PostgreSQL port | `5432` |
| `DB_USER` | Database user | `mdm` |
| `DB_PASSWORD` | Database password | — |
| `DB_NAME` | Database name | `mdm` |
| `DEVICE_API_KEY` | API key for device checkins | — |
| `DASHBOARD_USER` | Dashboard login username | `admin` |
| `DASHBOARD_PASSWORD` | Dashboard login password | — |
| `SESSION_SECRET` | Cookie signing secret | falls back to `DEVICE_API_KEY` |
| `CONFIG_PATH` | Path to display config | `config/display.json` |

## API

All device endpoints require `X-API-Key` header.

| Method | Endpoint | Description |
|---|---|---|
| `POST` | `/api/v1/checkin` | Device checkin (battery, build, apps, extras) |
| `GET` | `/api/v1/devices` | List all devices |
| `GET` | `/api/v1/devices/{serial}` | Device detail + recent checkins |
| `POST` | `/api/v1/commands` | Queue a command |
| `GET` | `/api/v1/commands` | List commands |
| `POST` | `/api/v1/commands/{id}/ack` | Acknowledge command |
| `POST` | `/api/v1/logcat` | Submit captured logcat |
| `GET` | `/api/v1/groups` | List groups |
| `POST` | `/api/v1/groups` | Create group |
| `GET` | `/health` | Health check |

## Project Structure

```
cmd/server/          # Entry point
internal/
  api/               # Device REST API handlers
  dashboard/         # Web UI handlers + HTMX partials
  db/                # Schema, migrations, queries
  middleware/         # API key auth
  config/            # Dynamic display column config
templates/           # Go HTML templates
static/              # CSS
config/              # display.json (extra dashboard columns)
```

## Development

```bash
# Run locally (requires PostgreSQL)
go build -o server ./cmd/server
./server

# Simulate devices
./simulate.sh          # 5 devices with battery drift
python3 simulate.py    # 50 threaded devices

# Backfill 24h of history
./backfill.sh
```
