# Tether — developer tasks. `make help` lists them.

BINARY   := tether
PKG      := github.com/adeelahmad/tether
CMD      := ./cmd/tether
DIST     := dist

VERSION  ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT   ?= $(shell git rev-parse --short HEAD 2>/dev/null || echo none)
DATE     ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS  := -s -w \
	-X $(PKG)/internal/version.Version=$(VERSION) \
	-X $(PKG)/internal/version.Commit=$(COMMIT) \
	-X $(PKG)/internal/version.Date=$(DATE)

PLATFORMS := darwin/arm64 darwin/amd64 linux/arm64 linux/amd64 windows/amd64

GO       ?= go
GOLANGCI ?= golangci-lint

.DEFAULT_GOAL := build
.PHONY: help build run test test-integration lint fmt vet tidy cover clean cross vuln check

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"} /^[a-zA-Z_-]+:.*##/ {printf "  \033[36m%-18s\033[0m %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build ./bin/tether for this platform
	CGO_ENABLED=0 $(GO) build -trimpath -ldflags "$(LDFLAGS)" -o bin/$(BINARY) $(CMD)

run: build ## Build and run `tether serve` (pass ARGS="--launch")
	./bin/$(BINARY) serve $(ARGS)

test: ## Run unit tests with the race detector
	$(GO) test -race -count=1 ./...

test-integration: ## Run end-to-end tests against a real headless Chrome
	$(GO) test -race -count=1 -tags integration ./test/integration/...

lint: ## Run golangci-lint
	$(GOLANGCI) run ./...

fmt: ## Format code (gofmt + goimports via golangci-lint)
	$(GOLANGCI) fmt ./...

vet: ## Run go vet (including integration-tagged code)
	$(GO) vet ./...
	$(GO) vet -tags integration ./...

tidy: ## Tidy and verify go.mod
	$(GO) mod tidy
	$(GO) mod verify

cover: ## Unit + integration coverage across all packages (needs Chrome)
	$(GO) test -race -count=1 -tags integration -covermode=atomic -coverpkg=./... -coverprofile=coverage.out ./...
	$(GO) tool cover -func=coverage.out | tail -1

vuln: ## Scan for known vulnerabilities
	$(GO) run golang.org/x/vuln/cmd/govulncheck@latest ./...

check: fmt vet lint test ## Everything CI checks, locally

cross: ## Static release builds for all platforms into ./dist
	@mkdir -p $(DIST)
	@for p in $(PLATFORMS); do \
		os=$${p%/*}; arch=$${p#*/}; ext=""; [ $$os = windows ] && ext=".exe"; \
		echo "  $$os/$$arch"; \
		CGO_ENABLED=0 GOOS=$$os GOARCH=$$arch $(GO) build -trimpath -ldflags "$(LDFLAGS)" \
			-o $(DIST)/$(BINARY)_$${os}_$${arch}$$ext $(CMD) || exit 1; \
	done

clean: ## Remove build output
	rm -rf bin $(DIST) coverage.out
