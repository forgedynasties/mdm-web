# AOSP MDM Server — Documentation

## Overview

A lightweight MDM (Mobile Device Management) backend for ~1000 custom AOSP devices.
Devices poll in every 60 seconds, sending a JSON payload to the API. A web dashboard
built with HTMX + Go templates provides real-time visibility into all devices.

---

## Architecture

```
AOSP Devices (x1000)
      |
      | POST /api/v1/checkin  (X-API-Key header)
      v
+-------------------+
|   Go HTTP Server  |  :8080
|   - REST API      |
|   - HTMX Dashboard|
+-------------------+
      |
      v
+-------------------+
|   PostgreSQL      |  :5432
+-------------------+
```

---

## Project Structure

```
mdm/
├── cmd/
│   └── server/
│       └── main.go
├── internal/
│   ├── api/          # API handlers
│   ├── dashboard/    # HTMX + template handlers
│   ├── db/           # Database layer
│   └── middleware/   # Auth, logging
├── migrations/       # SQL migration files
├── templates/        # Go HTML templates
├── static/           # CSS / JS assets
├── docker-compose.yml
├── Dockerfile
├── .env.example
└── DOCS.md
```

---

## Database Schema

### `devices`
Stores the canonical record for each device (identified by serial number).

| Column        | Type        | Notes                        |
|---------------|-------------|------------------------------|
| id            | UUID        | Primary key                  |
| serial_number | TEXT        | Unique, set on first checkin |
| build_id      | TEXT        | Latest known build           |
| created_at    | TIMESTAMPTZ | First seen                   |
| last_seen_at  | TIMESTAMPTZ | Updated on every checkin     |

### `checkins`
One row per poll. Historical record of every checkin.

| Column            | Type        | Notes                                      |
|-------------------|-------------|--------------------------------------------|
| id                | UUID        | Primary key                                |
| device_id         | UUID        | FK → devices.id                            |
| battery_pct       | SMALLINT    | 0–100                                      |
| build_id          | TEXT        | Build at time of checkin                   |
| extra             | JSONB       | Extensibility — any future fields go here  |
| created_at        | TIMESTAMPTZ | Checkin timestamp                          |

> **Extensibility:** Any new data fields from devices are stored in the `extra` JSONB
> column without requiring a schema migration. When a field becomes common/important
> it can be promoted to a dedicated column via a migration.

---

## Configuration

Copy `.env.example` to `.env` and fill in values:

```env
# Server
PORT=8080

# Postgres
DB_HOST=postgres
DB_PORT=5432
DB_USER=mdm
DB_PASSWORD=changeme
DB_NAME=mdm

# Shared API key — all devices use this
DEVICE_API_KEY=your-secret-key-here

# Dashboard login
DASHBOARD_USER=admin
DASHBOARD_PASSWORD=changeme
```

---

## Running with Docker

### Prerequisites
- Docker
- Docker Compose v2

### Start

```bash
# Clone / enter project directory
cd mdm

# Copy and edit environment
cp .env.example .env
nano .env        # set DEVICE_API_KEY, DASHBOARD_PASSWORD

# Build and start all services
docker compose up -d --build

# View logs
docker compose logs -f server

# Stop
docker compose down

# Stop and wipe database volume
docker compose down -v
```

The server will be available at `http://localhost:8080`.

---

## API Reference

All device API endpoints require the header:

```
X-API-Key: <DEVICE_API_KEY>
```

---

### POST /api/v1/checkin

Devices call this every 60 seconds.

**Request headers:**
```
Content-Type: application/json
X-API-Key: your-secret-key-here
```

**Request body — minimum payload:**
```json
{
  "serial_number": "ABC123XYZ",
  "build_id": "aosp-eng 13 TP1A.220624.014",
  "battery_pct": 87
}
```

**Request body — with extra fields (future extensibility):**
```json
{
  "serial_number": "ABC123XYZ",
  "build_id": "aosp-eng 13 TP1A.220624.014",
  "battery_pct": 87,
  "extra": {
    "ip_address": "192.168.1.42",
    "wifi_ssid": "Corp-WiFi",
    "storage_free_gb": 12.4
  }
}
```

**Response 200 OK:**
```json
{
  "status": "ok"
}
```

**Response 401 Unauthorized:**
```json
{
  "error": "invalid api key"
}
```

---

### curl Examples — Device Checkin

**Basic checkin:**
```bash
curl -X POST http://localhost:8080/api/v1/checkin \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-secret-key-here" \
  -d '{
    "serial_number": "ABC123XYZ",
    "build_id": "aosp-eng 13 TP1A.220624.014",
    "battery_pct": 87
  }'
```

**Checkin with extra fields:**
```bash
curl -X POST http://localhost:8080/api/v1/checkin \
  -H "Content-Type: application/json" \
  -H "X-API-Key: your-secret-key-here" \
  -d '{
    "serial_number": "ABC123XYZ",
    "build_id": "aosp-eng 13 TP1A.220624.014",
    "battery_pct": 54,
    "extra": {
      "ip_address": "10.0.0.55"
    }
  }'
```

**Simulate 3 different devices checking in:**
```bash
for serial in DEVICE001 DEVICE002 DEVICE003; do
  curl -s -X POST http://localhost:8080/api/v1/checkin \
    -H "Content-Type: application/json" \
    -H "X-API-Key: your-secret-key-here" \
    -d "{
      \"serial_number\": \"$serial\",
      \"build_id\": \"aosp-eng 13 TP1A.220624.014\",
      \"battery_pct\": $((RANDOM % 100))
    }" && echo " -> $serial OK"
done
```

**Test with wrong API key (expect 401):**
```bash
curl -X POST http://localhost:8080/api/v1/checkin \
  -H "Content-Type: application/json" \
  -H "X-API-Key: wrong-key" \
  -d '{"serial_number":"TEST","build_id":"x","battery_pct":50}'
```

---

### GET /api/v1/devices

Returns latest state of all known devices. Intended for dashboard and monitoring integrations.

**Request headers:**
```
X-API-Key: your-secret-key-here
```

**curl:**
```bash
curl http://localhost:8080/api/v1/devices \
  -H "X-API-Key: your-secret-key-here"
```

**Response:**
```json
[
  {
    "id": "a1b2c3d4-...",
    "serial_number": "ABC123XYZ",
    "build_id": "aosp-eng 13 TP1A.220624.014",
    "battery_pct": 87,
    "last_seen_at": "2026-03-06T14:32:00Z",
    "created_at": "2026-01-10T08:00:00Z"
  }
]
```

---

### GET /api/v1/devices/{serial}

Returns the latest state and full checkin history for a single device.

**curl:**
```bash
curl http://localhost:8080/api/v1/devices/ABC123XYZ \
  -H "X-API-Key: your-secret-key-here"
```

**Response:**
```json
{
  "device": {
    "id": "a1b2c3d4-...",
    "serial_number": "ABC123XYZ",
    "build_id": "aosp-eng 13 TP1A.220624.014",
    "last_seen_at": "2026-03-06T14:32:00Z",
    "created_at": "2026-01-10T08:00:00Z"
  },
  "checkins": [
    {
      "battery_pct": 87,
      "build_id": "aosp-eng 13 TP1A.220624.014",
      "extra": {},
      "created_at": "2026-03-06T14:32:00Z"
    },
    {
      "battery_pct": 91,
      "build_id": "aosp-eng 13 TP1A.220624.014",
      "extra": {},
      "created_at": "2026-03-06T14:31:00Z"
    }
  ]
}
```

---

## Dashboard

Access the web dashboard at: `http://localhost:8080/`

### Login
- URL: `http://localhost:8080/login`
- Credentials set via `DASHBOARD_USER` and `DASHBOARD_PASSWORD` in `.env`
- Session cookie is valid for 24 hours

### Pages

| Route                     | Description                              |
|---------------------------|------------------------------------------|
| `/`                       | Device list — all devices, latest state  |
| `/devices/{serial}`       | Device detail — history, battery chart   |

### Dashboard Features
- All-devices table: serial number, build ID, battery %, last seen
- Sortable / searchable device list
- Per-device checkin history table
- Battery percentage chart over time
- Auto-refresh every 60 seconds via HTMX polling

---

## Health Check

```bash
curl http://localhost:8080/health
# {"status":"ok","db":"ok"}
```

---

## Adding New Device Fields (Extensibility)

### Short-term — use `extra`
The device simply includes new keys in the `extra` object. No server changes needed:

```json
{
  "serial_number": "ABC123",
  "build_id": "...",
  "battery_pct": 80,
  "extra": {
    "new_field": "value"
  }
}
```

The value is stored as-is in the `checkins.extra` JSONB column and visible in the
device detail API response.

### Long-term — promote to a column
When a field is important enough to query/index directly:

1. Add a migration in `migrations/` to add the column to `checkins`
2. Update the checkin request struct in `internal/api/`
3. Update the DB insert in `internal/db/`
4. Update the dashboard template if needed

---

## Load Estimate

| Metric              | Value                         |
|---------------------|-------------------------------|
| Devices             | 1,000                         |
| Poll interval       | 60 seconds                    |
| Requests/second     | ~17 req/s (steady state)      |
| Rows/day (checkins) | ~1,440,000                    |
| Rows/month          | ~43,200,000                   |

At this scale a single Go server + single Postgres instance (with connection pooling
via pgx) is more than sufficient. Postgres can handle millions of rows easily; add
a `created_at` index on `checkins` and partition by month if the table grows large.
