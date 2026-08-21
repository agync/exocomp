.DEFAULT_GOAL := help

.PHONY: build check fmt fmt-check vet lint tidy-check test clean help

MODULE    := exocomp.dev/exocomp
VERSION   := $(shell git describe --tags --always --dirty 2>/dev/null || echo dev)
COMMIT    := $(shell git rev-parse --short HEAD 2>/dev/null || echo dev)
DATE      := $(shell date -u +%Y-%m-%dT%H:%M:%SZ)
LDFLAGS   := -X $(MODULE)/internal/version.Version=$(VERSION) \
             -X $(MODULE)/internal/version.Commit=$(COMMIT) \
             -X $(MODULE)/internal/version.Date=$(DATE)

##@ Build & Quality

build: ## Build the static exocomp binary into bin/
	CGO_ENABLED=0 go build -ldflags "$(LDFLAGS)" -o bin/exocomp ./cmd/exocomp

check: fmt-check vet lint tidy-check test ## Run fmt-check, vet, lint, and test

fmt: ## Format source files
	gofmt -w -s .

fmt-check: ## Fail if source files are not gofmt-formatted
	@unformatted="$$(gofmt -l -s .)"; \
	if [ -n "$$unformatted" ]; then \
		echo "not gofmt-formatted:"; echo "$$unformatted"; exit 1; \
	fi

vet: ## Run go vet
	go vet ./...

lint: ## Run golangci-lint if installed
	@if command -v golangci-lint >/dev/null 2>&1; then \
		golangci-lint run ./...; \
	else \
		echo "WARNING: golangci-lint not installed — lint NOT run" >&2; \
	fi

tidy-check: ## Fail if go.mod/go.sum are not tidy
	go mod tidy -diff

test: ## Run the test suite with the race detector
	go test -race ./...

clean: ## Remove build artifacts
	rm -rf bin/ dist/

##@ Utilities

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
	  /^[a-zA-Z0-9_/-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } \
	  /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
