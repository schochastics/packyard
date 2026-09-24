.PHONY: all build test fmt vet lint openapi-lint check e2e clean help

BIN          := packyard-server
CMD_DIR      := ./cmd/packyard-server
VERSION_PKG  := github.com/schochastics/packyard/internal/version
# Leading "v" stripped so local builds report the same "1.2.3" GoReleaser
# injects via {{ .Version }}.
VERSION      := $(patsubst v%,%,$(shell git describe --tags --always --dirty 2>/dev/null || echo dev))
LDFLAGS      := -s -w -X $(VERSION_PKG).Version=$(VERSION)

all: check build ## Run checks and build binary

build: ## Build the packyard-server binary
	go build -trimpath -ldflags "$(LDFLAGS)" -o $(BIN) $(CMD_DIR)

test: ## Run all tests
	go test -race ./...

fmt: ## Format Go sources
	gofmt -s -w .

vet: ## Run go vet
	go vet ./...

lint: ## Run golangci-lint (installs if missing)
	@command -v golangci-lint >/dev/null 2>&1 || { \
		echo "installing golangci-lint..."; \
		go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@latest; \
	}
	golangci-lint run ./...

# v0.29.0 is the newest vacuum that builds with the pinned go 1.25.0
# (v0.30.x needs 1.25.7+, v0.30.5+ needs 1.26); bump with the toolchain.
VACUUM_VERSION ?= v0.29.0

openapi-lint: ## Lint openapi/openapi.yaml with vacuum (installs if missing)
	@command -v vacuum >/dev/null 2>&1 || { \
		echo "installing vacuum..."; \
		go install github.com/daveshanley/vacuum@$(VACUUM_VERSION); \
	}
	vacuum lint --details --errors openapi/openapi.yaml

check: vet lint test openapi-lint ## Run vet, lint, tests, and openapi-lint

E2E_DISTROS ?= jammy rhel9

e2e: ## Run the Docker end-to-end suite: real R clients against a live server
	tests/e2e/run.sh $(E2E_DISTROS)

clean: ## Remove build artefacts
	rm -f $(BIN)
	rm -rf dist

help: ## List make targets
	@awk 'BEGIN {FS = ":.*##"; printf "\nTargets:\n"} /^[a-zA-Z0-9_-]+:.*?##/ { printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
