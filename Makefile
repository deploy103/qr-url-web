# QR Code generator + URL shortener

BINARY      := qrurl
MODULE      := github.com/deploy103/qr-url-web
DIST        := dist
VERSION     ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS     := -s -w -X main.version=$(VERSION) -X $(MODULE)/internal/server.BuildVersion=$(VERSION)
GO          ?= go

.PHONY: all
all: check build

## build: compile the production binary into dist/
.PHONY: build
build:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o $(DIST)/$(BINARY) ./cmd/qrurl
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "-s -w" -o $(DIST)/qrdecode ./cmd/qrdecode
	@echo "built $(DIST)/$(BINARY) ($(VERSION))"

## build-linux: cross compile for an amd64 Ubuntu server
.PHONY: build-linux
build-linux:
	@mkdir -p $(DIST)
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
		-o $(DIST)/$(BINARY)-linux-amd64 ./cmd/qrurl
	@echo "built $(DIST)/$(BINARY)-linux-amd64 ($(VERSION))"

## fmt: format every Go file
.PHONY: fmt
fmt:
	$(GO) fmt ./...

## vet: run the standard analysers
.PHONY: vet
vet:
	$(GO) vet ./...

## lint: run staticcheck if it is installed
.PHONY: lint
lint:
	@command -v staticcheck >/dev/null 2>&1 \
		&& staticcheck ./... \
		|| echo "staticcheck not installed: go install honnef.co/go/tools/cmd/staticcheck@latest"

## test: unit and integration tests
.PHONY: test
test:
	$(GO) test ./...

## test-race: the full suite under the race detector
.PHONY: test-race
test-race:
	$(GO) test -race -count=1 ./...

## cover: write a coverage profile to dist/coverage.out
.PHONY: cover
cover:
	@mkdir -p $(DIST)
	$(GO) test -coverprofile=$(DIST)/coverage.out -covermode=atomic ./...
	$(GO) tool cover -func=$(DIST)/coverage.out | tail -1

## check: format check, vet, lint and tests
.PHONY: check
check: vet lint test
	@test -z "$$(gofmt -l . | grep -v '^$(DIST)/')" || { echo "gofmt needed:"; gofmt -l .; exit 1; }

## run: start the server locally on 127.0.0.1:8080 with a local data directory
.PHONY: run
run:
	QRURL_DATA_DIR=./data \
	QRURL_BASE_URL=http://127.0.0.1:8080 \
	QRURL_SECURE_COOKIE=false \
	QRURL_HSTS=false \
	QRURL_TRUST_PROXY=false \
	QRURL_LOG_FORMAT=text \
	$(GO) run ./cmd/qrurl serve

## setpassword: change the administrator password of the local database
.PHONY: setpassword
setpassword:
	QRURL_DATA_DIR=./data $(GO) run ./cmd/qrurl setpassword

## docker-build: build the runtime image
.PHONY: docker-build
docker-build:
	docker build --build-arg VERSION=$(VERSION) -t qr-url:$(VERSION) -t qr-url:latest .

## docker-test: run vet and the whole suite inside the image
.PHONY: docker-test
docker-test:
	docker build --target test --build-arg VERSION=$(VERSION) -t qr-url:test .

## docker-dev: start the development stack on 127.0.0.1:8013
.PHONY: docker-dev
docker-dev:
	docker compose -f compose.dev.yaml up -d --build
	@echo "http://127.0.0.1:8013/  ·  http://127.0.0.1:8013/adminpage"

## docker-dev-down: stop the development stack
.PHONY: docker-dev-down
docker-dev-down:
	docker compose -f compose.dev.yaml down

## clean: remove build artefacts
.PHONY: clean
clean:
	rm -rf $(DIST)

## help: list the targets
.PHONY: help
help:
	@grep -E '^## ' $(MAKEFILE_LIST) | sed 's/## /  /'
