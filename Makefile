.PHONY: all build test fmt vet lint openapi-lint check clean help

BIN          := packyard-server
CMD_DIR      := ./cmd/packyard-server
VERSION_PKG  := github.com/schochastics/packyard/internal/version
VERSION      := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
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
		go install github.com/golangci/golangci-lint/cmd/golangci-lint@latest; \
	}
	golangci-lint run ./...

openapi-lint: ## Lint openapi/openapi.yaml with vacuum (installs if missing)
	@command -v vacuum >/dev/null 2>&1 || { \
		echo "installing vacuum..."; \
		go install github.com/daveshanley/vacuum@latest; \
	}
	vacuum lint --details --errors openapi/openapi.yaml

check: vet lint test openapi-lint ## Run vet, lint, tests, and openapi-lint

clean: ## Remove build artefacts
	rm -f $(BIN)
	rm -rf dist

help: ## List make targets
	@awk 'BEGIN {FS = ":.*##"; printf "\nTargets:\n"} /^[a-zA-Z_-]+:.*?##/ { printf "  \033[36m%-12s\033[0m %s\n", $$1, $$2 }' $(MAKEFILE_LIST)
