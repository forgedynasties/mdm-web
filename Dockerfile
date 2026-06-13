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

RUN apk add --no-cache ca-certificates tzdata

WORKDIR /app

COPY --from=builder /app/server .
COPY templates/ templates/
COPY static/    static/

EXPOSE 8080

CMD ["./server"]
