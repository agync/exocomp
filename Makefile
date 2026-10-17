.DEFAULT_GOAL := help

.PHONY: build budgets check check-hygiene check-race clean fmt fmt-check help hooks image lint site/build site/serve test test-race tidy-check vet

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

# Install the git hooks the framework drives. Run once per clone after
# `pip install pre-commit`. `core.hooksPath` (the old custom-dispatcher setup)
# is cleared so git uses the installed hooks.
hooks: ## Install the pre-commit git hooks (pre-commit, pre-push, commit-msg)
	git config --unset core.hooksPath 2>/dev/null || true
	pre-commit install
	pre-commit install --hook-type pre-push
	pre-commit install --hook-type commit-msg

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
	# 30m per-binary alarm: the race-instrumented web suite grew past the
	# framework's 10m default on loaded hosts (two lima VMs, the deployed
	# service). The fast -short gate keeps the default budget.
	go test -race -timeout 30m ./...

clean: ## Remove build artifacts
	rm -rf bin/ dist/

##@ Packaging & budgets

IMAGE        := localhost/exocomp:dev
IMAGE_MAX_MB := 800
RSS_MAX_MB   := 80
# A measured figure in the bill of materials is evidence only while its date is
# near; past this it is a claim nobody has checked.
DOC_MAX_DAYS := 180

image: ## Build the container image
	podman build -t $(IMAGE) -f packaging/Containerfile \
		--build-arg VERSION=$(VERSION) --build-arg COMMIT=$(COMMIT) --build-arg DATE=$(DATE) .

# The budgets are the ones docs/sbom/bill-of-materials.md commits to. A number
# in a README is a wish; this target exits non-zero, so it is a constraint. The
# doc's measured figures are checked by date and not by value: they move with the
# host and the day's base images, so equality would cry wolf, while a figure
# nobody has re-measured for six months is worth failing on.
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
	now=$$(date +%s); \
	dates=$$(grep -oE 'measured [0-9]{4}-[0-9]{2}-[0-9]{2}' docs/sbom/bill-of-materials.md | awk '{print $$2}'); \
	if [ -z "$$dates" ]; then echo 'doc figures  no dated measurement  FAIL'; fail=1; fi; \
	for d in $$dates; do \
		age=$$(( (now - $$(date -d "$$d" +%s)) / 86400 )); \
		printf 'doc figure   measured %s  (%3d days old, ceiling %d)' "$$d" "$$age" "$(DOC_MAX_DAYS)"; \
		if [ "$$age" -gt "$(DOC_MAX_DAYS)" ]; then echo '  FAIL'; fail=1; else echo '  ok'; fi; \
	done; \
	exit $$fail

##@ Site

SITE_PORT ?= 8788

site/build: ## Build the landing page and rendered docs into site/dist
	site/build.sh

# wrangler, not a plain file server: it reads the same config the deployment
# does and applies _headers and _redirects. 8788, because wrangler's own default
# 8787 is the obvious ad-hoc `python3 -m http.server` port.
site/serve: site/build ## Build, then serve site/dist for preview (:8788)
	cd site && npx --yes wrangler dev --port $(SITE_PORT)

##@ Utilities

help: ## Show this help
	@awk 'BEGIN {FS = ":.*##"; printf "\nUsage:\n  make \033[36m<target>\033[0m\n"} \
	  /^[a-zA-Z0-9_/-]+:.*?##/ { printf "  \033[36m%-20s\033[0m %s\n", $$1, $$2 } \
	  /^##@/ { printf "\n\033[1m%s\033[0m\n", substr($$0, 5) }' $(MAKEFILE_LIST)
