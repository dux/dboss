GO ?= go
WATCH ?= watchexec
GH ?= gh
VSTAMP ?= vstamp
BIN_DIR := ./bin
BINARY := $(BIN_DIR)/dboss
DIST_DIR := ./dist
CMD := ./cmd/dboss
# Every platform `dboss update` can install onto. The release asset is named
# dboss_<os>_<arch>, which is exactly what the updater asks GitHub for.
PLATFORMS := linux/amd64 linux/arm64 darwin/amd64 darwin/arm64

# The version lives in .version, stamped by `vstamp count --release` during `make release`. Both
# are recursively expanded so the release recipe reads .version after the stamp; dev only covers
# a checkout without the file.
VERSION = $(or $(addprefix v,$(shell sed -n 's/^version: *\([^ \#]*\).*/\1/p' .version 2>/dev/null)),dev)
LDFLAGS = -X dboss/internal/version.Version=$(VERSION)

.DEFAULT_GOAL := help

.PHONY: help build test e2e fmt vet lint check assets release-ready release-stamp release demo demo-watch seed kill clean

help: ## List available targets
	@awk 'BEGIN {FS = ":.*##"; printf "Usage: make <target>\n\nTargets:\n"} /^[a-zA-Z0-9_.-]+:.*##/ {printf "  %-12s %s\n", $$1, $$2}' $(MAKEFILE_LIST)

build: ## Build dboss into ./bin/dboss
	@mkdir -p $(BIN_DIR)
	$(GO) build -ldflags "$(LDFLAGS)" -o $(BINARY) $(CMD)

test: ## Run the unit tests
	$(GO) test ./...

e2e: ## Run the end-to-end suite: the real binary on a copy of ./demo (needs bun, lsof, mise and the sinatra gems)
	$(GO) test -tags e2e -count=1 ./e2e/

fmt: ## Format Go source files
	$(GO) fmt ./...

vet: ## Run go vet
	$(GO) vet -tags e2e ./...

lint: vet ## Run go vet and staticcheck
	$(GO) tool staticcheck -tags e2e ./...

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

# The release is the tagged HEAD, so it is only cut from a clean main: anything uncommitted
# would not ship in the tagged commit.
release-ready:
	@[ "$$(git rev-parse --abbrev-ref HEAD)" = main ] || { echo "release from main, not $$(git rev-parse --abbrev-ref HEAD)"; exit 1; }
	@[ -z "$$(git status --porcelain)" ] || { echo "commit or stash first: the release is the tagged HEAD"; git status --short; exit 1; }
	@command -v $(VSTAMP) >/dev/null 2>&1 || { echo "$(VSTAMP) not found in PATH"; exit 1; }
	@command -v $(GH) >/dev/null 2>&1 || { echo "$(GH) not found in PATH"; exit 1; }
	@$(GH) auth status >/dev/null 2>&1 || { echo "$(GH) is not authenticated"; exit 1; }

# Stamps .version from the commit count on main, amends it into HEAD (or adds a "release X.Y.Z"
# commit when HEAD is already pushed), tags vX.Y.Z and pushes main and the tag together. A HEAD
# that is already tagged is only pushed again, so a failed publish can be rerun.
release-stamp: release-ready
	$(VSTAMP) count --release

# One release per version, published by hand: after the stamp, ./bin/dboss and every platform
# asset are rebuilt as $(VERSION) in a sub-make, so they read the new .version, and the release is
# created on the tag vstamp pushed. A rerun replaces that release but keeps its tag. Every other
# release (and its tag) is removed, so GitHub always carries exactly the latest build and
# `dboss update` finds it as `latest`. Needs vstamp and an authenticated gh (repo + admin scopes).
release: release-stamp ## From a clean main: stamp, tag and push the version, then publish it as the only GitHub release
	$(MAKE) build assets
	@set -e; \
	$(GH) release delete "$(VERSION)" --yes 2>/dev/null || true; \
	echo "  publishing $(VERSION)"; \
	$(GH) release create "$(VERSION)" $(DIST_DIR)/* --verify-tag --title "$(VERSION)" --generate-notes; \
	for tag in $$($(GH) release list --limit 100 --json tagName --jq '.[].tagName'); do \
		[ "$$tag" = "$(VERSION)" ] && continue; \
		echo "  removing old release $$tag"; \
		$(GH) release delete "$$tag" --yes --cleanup-tag; \
	done

demo: build ## Run the local demo daemon
	$(BINARY) start $(DEMO_FLAGS) -c ./demo/dboss-server.yaml

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
	$(BINARY) kill -c ./demo/dboss-server.yaml

clean: ## Remove generated binaries
	rm -rf $(BIN_DIR) $(DIST_DIR)
