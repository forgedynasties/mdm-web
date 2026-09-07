#!/bin/sh
# Run the Go toolchain in Docker (host has no Go). Usage: tools/go.sh test ./internal/dashboard/
docker run --rm -v "$(pwd)":/app -v mdm-gomod:/go/pkg/mod -v mdm-gocache:/root/.cache/go-build -w /app -e CGO_ENABLED=0 -e GOFLAGS=-buildvcs=false golang:1.24 go "$@"
