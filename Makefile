# Outpost. `make help` lists targets.
#
# The binary must stay cgo-free so it keeps the static-linking property that
# modernc.org/sqlite was chosen for — CGO_ENABLED=0 is set here rather than
# left to the caller's environment.
export CGO_ENABLED := 0

BIN     := outpost
PKG     := ./cmd/outpost
PREFIX  ?= $(HOME)/.local

# Build stamp. VERSION defaults to the nearest tag, or "dev" outside a tagged
# checkout; commit and date come from git. A plain `go build` without these
# still identifies itself from the toolchain's own VCS stamp, so this only has
# to add the version name.
VERSION ?= $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT  ?= $(shell git rev-parse HEAD 2>/dev/null)
DATE    ?= $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
V       := github.com/sroberts/outpost/internal/version
LDFLAGS := -X $(V).Version=$(VERSION) -X $(V).Commit=$(COMMIT) -X $(V).Date=$(DATE)

# gofmt is checked rather than applied, and one file is exempt: it predates
# the current formatting and is deliberately left as originally written.
FMT_EXEMPT := internal/manifest/manifest_test.go

.DEFAULT_GOAL := help
.PHONY: help build install test test-race lint fmt fmt-check vet tidy clean check version

help: ## List targets
	@grep -hE '^[a-z-]+:.*?## ' $(MAKEFILE_LIST) | sort | \
	  awk 'BEGIN{FS=":.*?## "};{printf "  \033[36m%-12s\033[0m %s\n",$$1,$$2}'

build: ## Build the binary into ./$(BIN)
	go build -ldflags "$(LDFLAGS)" -o $(BIN) $(PKG)

install: ## Install the binary into $(PREFIX)/bin
	@mkdir -p $(PREFIX)/bin
	go build -ldflags "$(LDFLAGS)" -o $(PREFIX)/bin/$(BIN) $(PKG)
	@echo "installed $(PREFIX)/bin/$(BIN)"

test: ## Run the test suite
	go test ./...

test-race: ## Run the test suite under the race detector
	go test -race ./...

vet: ## go vet
	go vet ./...

lint: vet ## go vet plus golangci-lint
	golangci-lint run ./...

fmt: ## Format everything except the exempt file
	@gofmt -l -w $$(git ls-files '*.go' | grep -vF '$(FMT_EXEMPT)')

fmt-check: ## Fail if anything except the exempt file is unformatted
	@out=$$(gofmt -l $$(git ls-files '*.go' | grep -vF '$(FMT_EXEMPT)')); \
	if [ -n "$$out" ]; then echo "unformatted:"; echo "$$out"; exit 1; fi; \
	echo "gofmt: clean (exempt: $(FMT_EXEMPT))"

tidy: ## Verify go.mod/go.sum are tidy
	go mod tidy -diff

check: fmt-check vet lint test test-race ## Everything CI runs

version: ## Show the stamp this build would carry
	@echo "version=$(VERSION) commit=$(COMMIT) date=$(DATE)"

clean: ## Remove build output
	rm -f $(BIN)
