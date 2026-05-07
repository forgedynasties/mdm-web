.PHONY: build dev docker-build up down

VERSION ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
BRANCH  ?= $(shell git rev-parse --abbrev-ref HEAD 2>/dev/null || echo unknown)

build:
	go build -ldflags="-s -w -X main.Version=$(VERSION) -X main.Branch=$(BRANCH)" -o server ./cmd/server

dev:
	go run ./cmd/server

docker-build:
	VERSION=$(VERSION) BRANCH=$(BRANCH) docker compose build

up:
	VERSION=$(VERSION) BRANCH=$(BRANCH) docker compose up -d

down:
	docker compose down
