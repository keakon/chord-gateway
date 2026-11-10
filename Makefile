# Common developer shortcuts.
#
# This Makefile is intentionally minimal: it mirrors the commands used by CI and
# .githooks. Go targets run from a plain checkout with Go installed; documentation
# checks also need Python 3, Node >=22.12.0, and npm ci --prefix website.
#
#   make preflight   the pre-push checks: formatting, module tidiness, vet, staticcheck, short tests
#   make ci          everything CI runs
#   make test-fast   tests over a memory-backed TMPDIR when one is available

SHELL := /bin/bash

GO ?= go
PKGS ?= ./...
LOCAL ?= github.com/keakon/chord-gateway

GOIMPORTS ?= goimports
STATICCHECK ?= staticcheck
GOPLS_VERSION ?= v0.24.0
# gopls runs on the host even when GOOS/GOARCH select another analysis target,
# and the versioned directory keeps an older pin from being reused.
GOPLS_DIR := $(shell $(GO) env GOCACHE)/chord-gateway-gopls/$(GOPLS_VERSION)-$(shell $(GO) env GOHOSTOS)-$(shell $(GO) env GOHOSTARCH)
GOPLS ?= $(GOPLS_DIR)/gopls

MIN_COVERAGE ?= 70.0
COVERAGE_FILE ?= coverage.out
# Override to point at an existing checkout: CHORD_REPO=/path/to/chord make contract
CHORD_REPO ?= ../chord

.PHONY: help ci preflight build fmt fmt-check deps-check test test-fast test-cover race vet staticcheck gopls-install gopls-check quality docs-check docs-sync contract clean

help:
	@echo "preflight   pre-push checks: formatting, module tidiness, vet, staticcheck, short tests"
	@echo "ci          the full gate CI runs"
	@echo "build       build ./chord-gateway with release-style flags"
	@echo "build-matrix cross-build every supported GOOS/GOARCH target"
	@echo "test-fast   tests with TMPDIR on a memory-backed filesystem when possible"
	@echo "fmt         apply goimports + gofmt"
	@echo "quality     scripts/check-go-quality.sh (coverage + race + vet + staticcheck)"
	@echo "docs-check  check documentation (requires npm ci --prefix website)"
	@echo "docs-sync   regenerate the website from docs/"

# Same Go checks CI runs. Docs checks stay in .githooks/pre-push and
# .github/workflows/docs.yml because they depend on the changed paths.
ci: fmt-check deps-check quality gopls-check build-matrix contract docs-check resource-scripts-check

# Release-style build: pure Go (no cgo), path-trimmed, and stripped.
build:
	@build_env=(CGO_ENABLED=0); \
	if [[ "$$($(GO) env GOOS)" == "darwin" && "$$($(GO) env GOARCH)" == "arm64" ]]; then \
		build_env+=(GOARM64=v8.1); \
	fi; \
	env "$${build_env[@]}" $(GO) build -trimpath -ldflags "-s -w" -o chord-gateway .

# What `git push` runs: the checks cheap enough to keep a push interactive. The
# coverage floor, `go test -race` and gopls diagnostics are heavier than that
# split allows, so CI owns them; `make quality` and `make gopls-check` reproduce
# them locally. The hook runs docs checks only when docs changed. -short keeps
# the cross-repo contract tests out: they would build ../chord's working tree,
# so an unrelated Chord edit could block a gateway push. `make contract` runs
# them.
preflight: GOTESTFLAGS := -short
preflight: fmt-check deps-check vet staticcheck test-fast resource-scripts-check

fmt:
	$(GOIMPORTS) -w -local $(LOCAL) .
	$(GO) fmt ./...

fmt-check:
	@out="$$( $(GOIMPORTS) -l -local $(LOCAL) . )" || exit $$?; \
	if [[ -n "$$out" ]]; then \
		echo "goimports formatting needed:"; \
		echo "$$out"; \
		echo "Run: make fmt"; \
		exit 1; \
	fi

# A tidy-less go.mod is easy to land by accident and cheap to detect, so the
# check is a hard diff rather than a warning.
deps-check:
	$(GO) mod tidy -diff

test:
	$(GO) test -count=1 $(PKGS)

# Opt-in speed-up: run the tests with a memory-backed temp dir when one is
# available, otherwise fall back to the default temp dir. See
# scripts/test_tmpdir.sh. A private RAM disk created for this run alone is
# ejected when the run ends; shared mounts are never touched. Cache only the
# unambiguous short-only invocation: Go's test cache does not track changes to
# the external Chord dependency used by full contract tests.
test-fast:
	@tmp="$$( ./scripts/test_tmpdir.sh )"; \
	 trap './scripts/test_tmpdir.sh --release "$$tmp"' EXIT; \
	 if [[ -n "$$tmp" ]]; then echo "test-fast: TMPDIR=$$tmp"; \
	 else echo "test-fast: no memory-backed temp dir; using the default temp dir"; fi; \
	 test_flags=( $(GOTESTFLAGS) ); count_flags=(-count=1); \
	 if [[ $${#test_flags[@]} -eq 1 && ( "$${test_flags[0]}" == -short || "$${test_flags[0]}" == -short=true ) ]]; then \
		count_flags=(); \
	 fi; \
	 TMPDIR="$$tmp" TMP="$$tmp" TEMP="$$tmp" $(GO) test "$${count_flags[@]}" "$${test_flags[@]}" $(PKGS)

test-cover:
	COVERAGE_FILE=$(COVERAGE_FILE) MIN_COVERAGE=$(MIN_COVERAGE) ./scripts/check-go-quality.sh

# check-go-quality.sh already runs vet and staticcheck; listed separately for
# people who want to iterate on a single failure without waiting for coverage.
quality:
	COVERAGE_FILE=$(COVERAGE_FILE) MIN_COVERAGE=$(MIN_COVERAGE) ./scripts/check-go-quality.sh

race:
	$(GO) test -race -short -timeout=5m $(PKGS)

vet:
	$(GO) vet ./...

staticcheck:
	$(STATICCHECK) -checks 'all,-ST1000' ./...

# Diagnoses only the files Go selects for the host platform. Do not replace this
# with `git ls-files '*.go' | xargs gopls check`: that feeds build-tagged twins
# (config_persistence_windows.go alongside config_persistence_unix.go) to a
# single-platform gopls and reports diagnostics against files that never compile
# here, once per target platform.
# Keep cache paths in shell quotes so Make does not split paths with spaces.
gopls-install:
	@if [[ ! -x '$(GOPLS_DIR)/gopls' ]]; then \
		mkdir -p '$(GOPLS_DIR)' && \
		GOOS=$(shell $(GO) env GOHOSTOS) GOARCH=$(shell $(GO) env GOHOSTARCH) GOBIN='$(GOPLS_DIR)' $(GO) install golang.org/x/tools/gopls@$(GOPLS_VERSION); \
	fi

ifeq ($(GOPLS),$(GOPLS_DIR)/gopls)
GOPLS_DEP := gopls-install
endif

gopls-check: $(GOPLS_DEP)
	CG_GO=$(GO) CG_GOPLS='$(GOPLS)' ./scripts/check_gopls.sh

# Complements gopls-check: that gate only sees the host platform's files, so the
# windows and unix twins are type-checked here. The target list lives in the
# script because .github/workflows/ci.yml calls the same one.
build-matrix:
	./scripts/check_build_matrix.sh

docs-check:
	python3 -m unittest discover -s scripts -p 'test_check_docs.py'
	python3 scripts/check-docs.py

resource-scripts-check:
	bash -n scripts/test_tmpdir.sh scripts/test_resource_scripts.sh scripts/check_build_matrix.sh
	bash scripts/test_resource_scripts.sh

docs-sync:
	cd website && npm run sync

# Cross-repo headless protocol contract tests. Needs a local Chord checkout;
# skipped with a notice when CHORD_REPO does not exist, since Chord is a
# companion checkout rather than a build dependency.
contract:
	CHORD_GATEWAY_CONTRACT_CHORD_REPO=$(CHORD_REPO) ./scripts/check-chord-contract.sh

clean:
	rm -f $(COVERAGE_FILE) chord-gateway
	./scripts/test_tmpdir.sh --destroy
