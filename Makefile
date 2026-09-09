.DEFAULT_GOAL := help

.PHONY: build budgets check check-hygiene check-race clean fmt fmt-check help image lint test test-race tidy-check vet

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

# check is the fast per-commit gate; the full -race suite is check-race,
# which runs at pre-push and as the worktree-merge integration gate.
check: fmt-check vet lint tidy-check test ## Run fmt-check, vet, lint, and the fast -short tests

check-race: fmt-check vet lint tidy-check test-race ## Run fmt-check, vet, lint, and the full -race suite

check-hygiene: ## Run all pre-commit framework hooks over every tracked file
	pre-commit run --all-files

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

test: ## Run the fast test suite (-short, skips heavy integration tests)
	go test -short ./...

test-race: ## Run the full test suite with the race detector
	go test -race ./...

clean: ## Remove build artifacts
	rm -rf bin/ dist/

##@ Packaging & budgets

IMAGE        := localhost/exocomp:dev
IMAGE_MAX_MB := 800
RSS_MAX_MB   := 80

image: ## Build the container image
	podman build -t $(IMAGE) -f packaging/Containerfile .

# The budgets are the ones docs/sbom/bill-of-materials.md commits to. A number
# in a README is a wish; this target exits non-zero, so it is a constraint.
budgets: build ## Print image size and idle RSS against their ceilings, failing above
	@fail=0; \
	if size=$$(podman image inspect --format '{{.Size}}' $(IMAGE) 2>/dev/null); then \
		mb=$$(( size / 1048576 )); \
		printf 'image size   %4d MB  (ceiling %d MB)' "$$mb" "$(IMAGE_MAX_MB)"; \
		if [ "$$mb" -gt "$(IMAGE_MAX_MB)" ]; then echo '  FAIL'; fail=1; else echo '  ok'; fi; \
	else \
		echo "image size    MISSING  ($(IMAGE) is not built; run 'make image')"; fail=1; \
	fi; \
	rss=$$(packaging/idle-rss.sh ./bin/exocomp) || fail=1; \
	if [ -n "$$rss" ]; then \
		mb=$$(( rss / 1024 )); \
		printf 'idle RSS     %4d MB  (ceiling %d MB)' "$$mb" "$(RSS_MAX_MB)"; \
		if [ "$$mb" -gt "$(RSS_MAX_MB)" ]; then echo '  FAIL'; fail=1; else echo '  ok'; fi; \
	fi; \
	exit $$fail

##@ Utilities

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
	  /^[a-zA-Z0-9_/-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } \
	  /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
