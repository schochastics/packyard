.PHONY: all build test fmt vet lint check clean help

BIN          := pakman-server
CMD_DIR      := ./cmd/pakman-server
VERSION_PKG  := github.com/schochastics/pakman/internal/version
VERSION      := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
LDFLAGS      := -s -w -X $(VERSION_PKG).Version=$(VERSION)

all: check build ## Run checks and build binary

build: ## Build the pakman-server binary
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
		go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest; \
	}
	golangci-lint run ./...

check: vet lint test ## Run vet, lint, and tests

clean: ## Remove build artefacts
	rm -f $(BIN)
	rm -rf dist

help: ## List make targets
	@awk 'BEGIN {FS = ":.*##"; printf "\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
