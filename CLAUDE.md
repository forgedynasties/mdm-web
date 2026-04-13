# AIO MDM Server

Go MDM (Mobile Device Management) server for AIO Android devices.

## Project Structure

```
cmd/server/         → entrypoint
internal/
  api/              → HTTP + WebSocket handlers
  config/           → runtime config (env-based)
  dashboard/        → admin dashboard templates
  db/               → PostgreSQL queries, schema, migrations
  middleware/        → HTTP middleware (auth, logging)
  shell/            → legacy remote shell support
  ws/               → WebSocket hub (device connections)
migrations/         → SQL migration files
client-app/         → Android MDM client (Java)
static/             → dashboard frontend assets
templates/          → Go HTML templates
```

## Key Concepts

- **Command Queue**: Commands (install_apk, shell, screenshot, reboot, ota) are persisted in DB. Pushed via WebSocket when device online, queued when offline. Flushed on reconnect.
- **Reboot Fence**: When flushing queued commands, processing stops at a reboot command. Commands after reboot remain pending and flush on next reconnect. This prevents post-reboot commands from being marked delivered but never executed.
- **Legacy Checkin**: Older clients poll via `POST /api/v1/checkin` instead of WebSocket. Controlled by `LegacyCheckin()` config flag. Same reboot-fence logic applies.
- **Command Status Lifecycle**: pending → delivered → installed/failed/completed. Reboot commands go directly pending → completed at delivery time (no client ack).
- **Targeting**: Commands target `all`, specific `devices` (by serial→UUID), or `groups`.

## Build & Run

```bash
go build -o mdm ./cmd/server
# or
docker compose up
```

## Testing

```bash
go test ./...
```

## Common Tasks

- Adding new command type: update `db.CreateCommand`, add case in `flushPendingCommands`, `marshalCommand`, and client-side `processWsCommand`
- Modifying checkin response: `internal/api/handlers.go` Checkin handler
- DB schema changes: add migration in `migrations/`, update `internal/db/db.go`
