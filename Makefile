SHELL   := /bin/bash
GO      ?= go
PKG     ?= ./...
IMAGE   ?= ghcr.io/l-k-m/dl-tool
VERSION ?= dev
GOLANGCI_LINT_VERSION ?= v2.13.2 # pin at implementation time; make setup fails until it is set
LYCHEE_VERSION        ?= 0.24.2  # pin at implementation time; scripts/doclint.sh fails without lychee

.PHONY: setup gen lint vet typecheck test test-go test-web test-integration e2e \
        build docker-build compose-check doclint ci

setup:
	$(GO) mod download
	$(GO) install github.com/golangci/golangci-lint/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	cd web && npm ci
	cd web && npx playwright install --with-deps chromium
	cargo install --locked lychee --version $(LYCHEE_VERSION)

gen:
	./scripts/gen.sh

lint:
	test -z "$$(gofmt -l cmd internal)"
	golangci-lint run ./...
	cd web && npm run lint
	cd web && npx prettier --check .

vet:
	$(GO) vet ./...

typecheck:
	cd web && npx tsc --noEmit -p tsconfig.json

ifeq ($(PKG),./...)
test: test-go test-web
else
test: test-go
endif

test-go:
	$(GO) test -race -count=1 $(PKG)

test-web:
	cd web && npx vitest run

test-integration:
	$(GO) test -tags=integration -count=1 -timeout=20m ./internal/engine/...

e2e:
	cd web && npx playwright test

build:
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags '-s -w -X main.version=$(VERSION)' \
		-o bin/dl-tool ./cmd/dl-tool

docker-build:
	docker build -t $(IMAGE):$(VERSION) .

compose-check:
	docker compose -f compose.yaml config -q
	docker compose -f compose.yaml -f compose.dev.yaml config -q

doclint:
	./scripts/doclint.sh

ci: lint vet typecheck test compose-check doclint
