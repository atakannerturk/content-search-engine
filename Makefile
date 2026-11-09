GO ?= go
PKG := ./...
BIN := bin/api

.PHONY: all build run test test-race test-integration vet lint fmt tidy up down logs sync-once migrate docker-build check-name

all: vet test build

build:
	$(GO) build -o $(BIN) ./cmd/api

run:
	$(GO) run ./cmd/api

test:
	$(GO) test -count=1 $(PKG)

test-race:
	$(GO) test -race -count=1 $(PKG)

## Requires a reachable PostgreSQL, e.g. `make up` first.
test-integration:
	TEST_DATABASE_URL=postgres://app:app@localhost:5432/content_test?sslmode=disable $(GO) test -count=1 -run Integration ./internal/storage/...

vet:
	$(GO) vet $(PKG)

lint:
	golangci-lint run

fmt:
	gofmt -s -w .

tidy:
	$(GO) mod tidy

up:
	docker compose up -d postgres redis

down:
	docker compose down -v

logs:
	docker compose logs -f api

sync-once:
	$(GO) run ./cmd/api -sync-once

migrate:
	$(GO) run ./cmd/api -migrate-only

docker-build:
	docker build -t content-search-engine:local .

## The assignment asks not to mention the issuing company anywhere in the repository.
## The pattern is assembled from fragments so this file does not match itself.
check-name:
	@pattern="enuy""gun|win""gie"; \
	if git grep -niE "$$pattern" -- . ':!.github/workflows/ci.yml' ':!Makefile'; then echo "forbidden name found"; exit 1; fi
