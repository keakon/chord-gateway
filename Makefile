# Common developer shortcuts.
#
# This Makefile is intentionally minimal: it mirrors the commands used by CI and
# .githooks without introducing new tooling assumptions. Anything here must stay
# runnable from a plain checkout with Go installed.
#
#   make preflight   what pre-push runs, before pushing
#   make ci          everything CI runs
#   make test-fast   tests over a memory-backed TMPDIR when one is available

SHELL := /bin/bash

GO ?= go
PKGS ?= ./...
LOCAL ?= github.com/keakon/chord-gateway

GOIMPORTS ?= goimports
STATICCHECK ?= staticcheck
GOPLS ?= gopls

MIN_COVERAGE ?= 70.0
COVERAGE_FILE ?= coverage.out
# Override to point at an existing checkout: CHORD_REPO=/path/to/chord make contract
CHORD_REPO ?= ../chord

.PHONY: help ci preflight fmt fmt-check deps-check test test-fast test-cover race vet staticcheck gopls-check quality docs-check docs-sync contract clean

help:
	@echo "preflight   what pre-push runs: goimports, quality gate, gopls"
	@echo "ci          the full gate CI runs"
	@echo "test-fast   tests with TMPDIR on a memory-backed filesystem when possible"
	@echo "fmt         apply goimports + gofmt"
	@echo "quality     scripts/check-go-quality.sh (coverage + race + vet + staticcheck)"
	@echo "docs-sync   regenerate the website from docs/"

# Same Go checks CI runs. Docs checks stay in .githooks/pre-push and
# .github/workflows/docs.yml because they depend on the changed paths.
ci: fmt-check deps-check quality gopls-check contract docs-check

# The gates .githooks/pre-push runs, plus the cheaper module-tidiness check;
# docs checks stay in the hook because they depend on the changed paths.
# check-go-quality.sh already covers vet, staticcheck, the short and race test
# runs and the coverage floor, so they are not repeated here.
preflight: fmt-check deps-check quality gopls-check

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
# scripts/test_tmpdir.sh.
test-fast:
	@tmp="$$( ./scripts/test_tmpdir.sh )"; \
	 if [[ -n "$$tmp" ]]; then echo "test-fast: TMPDIR=$$tmp"; \
	 else echo "test-fast: no memory-backed temp dir; using the default temp dir"; fi; \
	 TMPDIR="$$tmp" TMP="$$tmp" TEMP="$$tmp" $(GO) test -count=1 $(PKGS)

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
gopls-check:
	CG_GO=$(GO) CG_GOPLS=$(GOPLS) ./scripts/check_gopls.sh

docs-check:
	python3 scripts/check-docs.py

docs-sync:
	cd website && npm run sync

# Cross-repo headless protocol contract tests. Needs a local Chord checkout;
# skipped with a notice when CHORD_REPO does not exist, since Chord is a
# companion checkout rather than a build dependency.
contract:
	CHORD_GATEWAY_CONTRACT_CHORD_REPO=$(CHORD_REPO) ./scripts/check-chord-contract.sh

clean:
	rm -f $(COVERAGE_FILE)
	./scripts/test_tmpdir.sh --destroy
