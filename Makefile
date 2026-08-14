# SPDX-License-Identifier: MIT
# go-unifi2mqtt — developer Makefile
#
# Tabs are required by GNU make. The whitespace rules below pin sane
# shell behaviour so a failing recipe step actually aborts the target
# instead of silently moving on.

SHELL := /usr/bin/env bash
.SHELLFLAGS := -euo pipefail -c
.DEFAULT_GOAL := help

GO            ?= go
GOFUMPT       ?= gofumpt
GOIMPORTS     ?= goimports
GOLANGCI_LINT ?= golangci-lint
GOVULNCHECK   ?= govulncheck
GOLICENSES    ?= go-licenses
DOCKER        ?= docker

export CGO_ENABLED := 0

BIN_DIR  := bin
MODULE   := github.com/SukramJ/go-unifi2mqtt
PKG_VER  := $(MODULE)/internal/version

VERSION    ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT     ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
BUILD_DATE ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)

LDFLAGS := -s -w \
	-X $(PKG_VER).Version=$(VERSION) \
	-X $(PKG_VER).Commit=$(COMMIT) \
	-X $(PKG_VER).BuildDate=$(BUILD_DATE)

GO_BUILD_FLAGS := -trimpath -ldflags="$(LDFLAGS)"

DOCKER_IMAGE ?= go-unifi2mqtt
DOCKER_TAG   ?= $(VERSION)

DIST_DIR         := dist
RELEASE_TARGETS  ?= linux/amd64 linux/arm64 darwin/arm64
# Pulled from internal/version/version.go's default — the contract is
# that bumping the source default and adding a changelog.md entry
# happen in the same commit, so this is the canonical "what release am
# I packaging" answer. Override via `make release RELEASE_VERSION=...`
# for ad-hoc dry runs.
RELEASE_VERSION  ?= $(shell awk -F'"' '/^[[:space:]]*Version = /{print $$2; exit}' internal/version/version.go)
RELEASE_PAYLOAD  := config-template.yaml README.md LICENSE changelog.md

.PHONY: help
help: ## show this help
	@awk 'BEGIN {FS = ":.*## "} /^[a-zA-Z_-]+:.*## / {printf "  \033[36m%-14s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

.PHONY: setup
setup: hooks ## install developer tooling (gofumpt, goimports, golangci-lint, govulncheck, go-licenses) + git hooks
	$(GO) install mvdan.cc/gofumpt@latest
	$(GO) install golang.org/x/tools/cmd/goimports@latest
	$(GO) install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest
	$(GO) install golang.org/x/vuln/cmd/govulncheck@latest
	$(GO) install github.com/google/go-licenses@latest

.PHONY: hooks
hooks: ## point git at the tracked hooks in .githooks/ (blocks direct commits on main)
	@git config core.hooksPath .githooks
	@echo "git core.hooksPath -> .githooks (direct commits on main/master are now blocked)"

.PHONY: build
build: ## build the unifi2mqtt daemon into bin/
	@mkdir -p $(BIN_DIR)
	$(GO) build $(GO_BUILD_FLAGS) -o $(BIN_DIR)/unifi2mqtt ./cmd/unifi2mqtt

.PHONY: install
install: ## go install the daemon to $(go env GOPATH)/bin
	$(GO) install $(GO_BUILD_FLAGS) ./cmd/unifi2mqtt

.PHONY: test
test: ## run the full test suite with race detector
	CGO_ENABLED=1 $(GO) test -race -count=1 -timeout=60s ./...

.PHONY: test-cover
test-cover: ## run tests + coverage report
	CGO_ENABLED=1 $(GO) test -race -count=1 -covermode=atomic -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -20

.PHONY: vet
vet: ## run go vet
	$(GO) vet ./...

.PHONY: fmt
fmt: ## format with gofumpt + goimports (writes in place)
	$(GOFUMPT) -w .
	$(GOIMPORTS) -w -local $(MODULE) .

.PHONY: fmt-check
fmt-check: ## fail when sources are not gofumpt-clean
	@diff=$$($(GOFUMPT) -l .); \
	if [ -n "$$diff" ]; then \
	  echo "gofumpt would rewrite:"; echo "$$diff"; exit 1; \
	fi

.PHONY: lint
lint: ## run golangci-lint
	$(GOLANGCI_LINT) run ./...

.PHONY: vuln
vuln: ## scan dependencies + reachable code for known vulnerabilities (govulncheck)
	$(GOVULNCHECK) ./...

.PHONY: licenses
licenses: ## fail on copyleft dependency licenses (GPL/AGPL/LGPL forbidden; MPL = reciprocal)
	$(GOLICENSES) check ./... --disallowed_types=forbidden,restricted,reciprocal

.PHONY: tidy
tidy: ## sync go.mod / go.sum
	$(GO) mod tidy

.PHONY: check
check: vet fmt-check lint test ## the pre-commit / pre-push gate

.PHONY: run
run: build ## run the daemon against ./config.yaml
	$(BIN_DIR)/unifi2mqtt --config ./config.yaml

.PHONY: clean
clean: ## remove build artefacts
	rm -rf $(BIN_DIR) $(DIST_DIR) coverage.out

.PHONY: release
release: ## stage cross-compiled release archives + notes into dist/ (no upload)
	@rm -rf $(DIST_DIR)
	@mkdir -p $(DIST_DIR)
	@echo "release version: $(RELEASE_VERSION)"
	@version="$(RELEASE_VERSION)"; \
	commit="$$(git rev-parse --short HEAD 2>/dev/null || echo none)"; \
	build_date="$$(date -u +%Y-%m-%dT%H:%M:%SZ)"; \
	ldflags="-s -w \
	  -X $(PKG_VER).Version=$$version \
	  -X $(PKG_VER).Commit=$$commit \
	  -X $(PKG_VER).BuildDate=$$build_date"; \
	for tgt in $(RELEASE_TARGETS); do \
	  goos=$${tgt%/*}; goarch=$${tgt#*/}; \
	  stage="$(DIST_DIR)/go-unifi2mqtt-$$version-$$goos-$$goarch"; \
	  mkdir -p "$$stage"; \
	  echo "==> $$goos/$$goarch -> $$stage"; \
	  GOOS=$$goos GOARCH=$$goarch $(GO) build -trimpath -ldflags="$$ldflags" \
	    -o "$$stage/unifi2mqtt" ./cmd/unifi2mqtt; \
	  cp $(RELEASE_PAYLOAD) "$$stage/"; \
	  ( cd $(DIST_DIR) && tar -czf "$$(basename $$stage).tar.gz" "$$(basename $$stage)" ); \
	  rm -rf "$$stage"; \
	done
	@cd $(DIST_DIR) && shasum -a 256 *.tar.gz > SHA256SUMS
	@$(MAKE) --no-print-directory release-notes
	@echo ""
	@ls -lh $(DIST_DIR)

.PHONY: release-notes
release-notes: ## extract the changelog.md section for $(RELEASE_VERSION) into dist/RELEASE_NOTES.md
	@mkdir -p $(DIST_DIR)
	@script/extract-release-notes.sh $(RELEASE_VERSION) > $(DIST_DIR)/RELEASE_NOTES.md
	@echo "--- $(DIST_DIR)/RELEASE_NOTES.md (first 20 lines) ---"
	@head -20 $(DIST_DIR)/RELEASE_NOTES.md

.PHONY: docker
docker: ## build a tagged container image
	$(DOCKER) build \
	  --build-arg VERSION=$(VERSION) \
	  --build-arg COMMIT=$(COMMIT) \
	  --build-arg BUILD_DATE=$(BUILD_DATE) \
	  -t $(DOCKER_IMAGE):$(DOCKER_TAG) \
	  -t $(DOCKER_IMAGE):latest .

.PHONY: version
version: ## print the resolved build metadata
	@echo "VERSION    = $(VERSION)"
	@echo "COMMIT     = $(COMMIT)"
	@echo "BUILD_DATE = $(BUILD_DATE)"
