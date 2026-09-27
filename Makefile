GO ?= go
WATCH ?= watchexec
GH ?= gh
BIN_DIR := ./bin
BINARY := $(BIN_DIR)/dboss
DIST_DIR := ./dist
CMD := ./cmd/dboss
# Every platform `dboss update` can install onto. The release asset is named
# dboss_<os>_<arch>, which is exactly what the updater asks GitHub for.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

# The version is the number of commits in main. HEAD and 0 only cover a checkout without a
# main branch, so a build outside a normal clone still succeeds.
VERSION := v$(shell git rev-list --count main 2>/dev/null || git rev-list --count HEAD 2>/dev/null || echo 0)
LDFLAGS := -X dboss/internal/version.Version=$(VERSION)

.DEFAULT_GOAL := help

.PHONY: help build test fmt vet lint check assets release-ready release demo demo-watch seed kill clean

help: ## List available targets
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_.-]+:.*##/ {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build dboss into ./bin/dboss
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)

test: ## Run the test suite
	$(GO) test ./...

fmt: ## Format Go source files
	$(GO) fmt ./...

vet: ## Run go vet
	$(GO) vet ./...

lint: vet ## Run go vet and staticcheck
	$(GO) tool staticcheck ./...

check: lint test ## Run static checks and tests

assets: ## Build every release asset (dboss_<os>_<arch>) and checksums.txt into ./dist
	@rm -rf $(DIST_DIR)
	@mkdir -p $(DIST_DIR)
	@for target in $(PLATFORMS); do \
		os=$${target%/*}; arch=$${target#*/}; \
		echo "  building $$os/$$arch"; \
		GOOS=$$os GOARCH=$$arch CGO_ENABLED=0 $(GO) build -trimpath \
			-ldflags "-s -w $(LDFLAGS)" -o "$(DIST_DIR)/dboss_$${os}_$${arch}" $(CMD) || exit 1; \
	done
	@cd $(DIST_DIR) && { command -v sha256sum >/dev/null 2>&1 && sha256sum dboss_* || shasum -a 256 dboss_*; } > checksums.txt
	@echo "  wrote $(DIST_DIR)/checksums.txt"

# The version is the commit count, so a release is only cut from a clean main: anything
# uncommitted would ship under the previous commit's number.
release-ready:
	@[ "$$(git rev-parse --abbrev-ref HEAD)" = main ] || { echo "release from main, not $$(git rev-parse --abbrev-ref HEAD)"; exit 1; }
	@[ -z "$$(git status --porcelain)" ] || { echo "commit or stash first: the version is the commit count"; git status --short; exit 1; }
	@command -v $(GH) >/dev/null 2>&1 || { echo "$(GH) not found in PATH"; exit 1; }
	@$(GH) auth status >/dev/null 2>&1 || { echo "$(GH) is not authenticated"; exit 1; }

# One release per version, published by hand: ./bin/dboss and every platform asset are rebuilt
# as $(VERSION), main is pushed, and the release is tagged on that exact commit. Every other
# release (and its tag) is removed, so GitHub always carries exactly the latest build and
# `dboss update` finds it as `latest`. Needs an authenticated gh (repo + admin scopes).
release: release-ready build assets ## From a clean main: build, push main and publish it as the only GitHub release
	@set -e; \
	git push origin main; \
	$(GH) release delete "$(VERSION)" --yes --cleanup-tag 2>/dev/null || true; \
	echo "  publishing $(VERSION)"; \
	$(GH) release create "$(VERSION)" $(DIST_DIR)/* --target "$$(git rev-parse HEAD)" --title "$(VERSION)" --generate-notes; \
	for tag in $$($(GH) release list --limit 100 --json tagName --jq '.[].tagName'); do \
		[ "$$tag" = "$(VERSION)" ] && continue; \
		echo "  removing old release $$tag"; \
		$(GH) release delete "$$tag" --yes --cleanup-tag; \
	done

demo: build ## Run the local demo daemon
	$(BINARY) start $(DEMO_FLAGS) -c ./demo/dboss.yaml

demo-watch: build ## Rebuild and restart the demo on changes
	$(WATCH) --restart --clear \
		--watch ./cmd \
		--watch ./internal \
		--watch ./demo \
		--watch ./go.mod \
		--watch ./go.sum \
		--ignore './demo/.dboss/**' \
		-- $(MAKE) demo DEMO_FLAGS=-y

seed: ## Recreate the demo databases with dummy traffic, logs, audit and blocked data (stop the demo first)
	go run ./internal/demo/seed --dir ./demo/.dboss/log

kill: build ## Kill all demo apps and listeners in the app port range
	$(BINARY) kill -c ./demo/dboss.yaml

clean: ## Remove generated binaries
	rm -rf $(BIN_DIR) $(DIST_DIR)
