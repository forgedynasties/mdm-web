# syntax=docker/dockerfile:1
FROM golang:1.24-alpine AS builder

WORKDIR /app

# Cache mounts persist the module cache and the Go build (compiler) cache across
# builds, so `docker compose up --build` only recompiles the packages that actually
# changed instead of the whole dependency tree every time. First build is unchanged;
# subsequent incremental builds drop from ~50s to a few seconds.
COPY go.mod go.sum ./
RUN --mount=type=cache,target=/go/pkg/mod go mod download

COPY . .
RUN --mount=type=cache,target=/go/pkg/mod \
    --mount=type=cache,target=/root/.cache/go-build \
    CGO_ENABLED=0 GOOS=linux go build -ldflags="-s -w" -o server ./cmd/server


FROM alpine:3.21

# wget (busybox) drives the HEALTHCHECK below; ca-certificates/tzdata for TLS + timezones.
RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /app/server .
COPY templates/ templates/
COPY static/    static/

# Run as an unprivileged user, not root. /app/data is where CONFIG_PATH lives and is
# the only path the server writes to at runtime; create it owned by that user so the
# named volume mounted there (see docker-compose.yml) inherits writable ownership on
# first mount. Everything else in /app is read-only to the process.
RUN addgroup -S mdm && adduser -S -G mdm -H mdm \
    && mkdir -p /app/data \
    && chown -R mdm:mdm /app/data
USER mdm

# Container listens on $PORT (compose passes it through; default 8089, matching
# docker-compose.yml and .env). EXPOSE is documentation only, so hard-code the default.
EXPOSE 8089

# Fail the container health check if the server stops answering /health. --start-period
# gives the process time to run migrations and bind before the first probe counts.
HEALTHCHECK --interval=15s --timeout=5s --start-period=30s --retries=3 \
    CMD wget -q -O /dev/null "http://127.0.0.1:${PORT:-8089}/health" || exit 1

CMD ["./server"]
