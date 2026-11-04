SHELL := bash
.SHELLFLAGS := -eu -o pipefail -c

APP_NAME    := gedour
MODULE      := github.com/gedour/gedour
# Entry-point package; passed to docker build too, so image and local binary share it.
PKG         := ./cmd/gedour
VERSION     ?= v0.0.0
COMMIT_HASH := $(shell git rev-parse --short HEAD 2>/dev/null || echo "0000000")
BUILD_TS    := $(shell date -Iseconds)
BUILDER     := $(shell go version 2>/dev/null || echo "unknown")
PROJECT_URL ?= https://$(MODULE)
# Always `main.`: the linker names the entry package main, an import path is silently ignored.
LDFLAGS     := -s -w \
  -X 'main.Version=$(VERSION)' \
  -X 'main.CommitHash=$(COMMIT_HASH)' \
  -X 'main.BuildTimestamp=$(BUILD_TS)' \
  -X 'main.Builder=$(BUILDER)' \
  -X 'main.ProjectURL=$(PROJECT_URL)'
COVERAGE_MIN ?= 80

.PHONY: build web go-build test coverage go-coverage web-coverage lint e2e docker-build dev clean

build: web go-build

web/node_modules: web/package.json web/pnpm-lock.yaml
	pnpm --dir web install --frozen-lockfile
	touch $@

web: web/node_modules
	pnpm --dir web build

go-build:
	CGO_ENABLED=0 go build -ldflags="$(LDFLAGS)" -o bin/$(APP_NAME) $(PKG)

test: web/node_modules
	go test -race ./...
	pnpm --dir web test

# 001-AC29: CI fails below the 80 % Go and web coverage floors.
coverage: go-coverage web-coverage

# cmd/ is thin wiring (flags, logger, signals); its logic lives in pkg/ and is measured there.
go-coverage:
	go test -race -coverpkg=./pkg/... -coverprofile=coverage.out ./...
	total=$$(go tool cover -func=coverage.out | awk '/^total:/ { sub("%", "", $$3); print $$3 }'); \
	echo "Go coverage: $${total}% (min $(COVERAGE_MIN)%)"; \
	awk -v t="$${total}" -v m="$(COVERAGE_MIN)" 'BEGIN { exit !(t >= m) }' || { echo "Go coverage below $(COVERAGE_MIN)%"; exit 1; }

web-coverage: web/node_modules
	pnpm --dir web test:coverage

lint: web/node_modules
	golangci-lint run
	pnpm --dir web lint
	pnpm --dir web typecheck
	pnpm --dir web format:check

e2e: build
	scripts/e2e.sh

docker-build:
	docker build \
	  --build-arg VERSION="$(VERSION)" \
	  --build-arg COMMIT_HASH="$(COMMIT_HASH)" \
	  --build-arg BUILD_TIMESTAMP="$(BUILD_TS)" \
	  --build-arg PROJECT_URL="$(PROJECT_URL)" \
	  --build-arg PKG="$(PKG)" \
	  -t $(APP_NAME):local .

dev:
	docker compose up --build

clean:
	rm -rf bin/ coverage.out web/coverage web/playwright-report web/test-results
	find web/dist -mindepth 1 ! -name .gitkeep -delete
