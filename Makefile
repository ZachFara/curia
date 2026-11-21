# Curia build and check entry points.
#
# `make check` is the full gate and CI runs exactly it (docs/decisions/0007).
# The lefthook pre-commit hook runs the fast `make check-fast` instead.
#
# Tools are pinned here and installed into .bin/ (gitignored) with
# `go install <pkg>@<version>`, so local runs and CI use identical binaries.
# Tools are deliberately not tracked in go.mod (docs/decisions/0002).
#
# Requires `go` (1.26) on PATH.

SHELL := /bin/bash
.SHELLFLAGS := -eu -o pipefail -c
.DEFAULT_GOAL := check

BIN := $(CURDIR)/.bin

GOLANGCI_LINT_VERSION := v2.15.0
GOFUMPT_VERSION       := v0.12.0
STATICCHECK_VERSION   := v0.8.2
DEADCODE_VERSION      := v0.52.0

# GoReleaser is not a `make check` tool: it is run with `go run` by
# release-snapshot, and by .github/workflows/release.yml at the same version.
GORELEASER_VERSION    := v2.20.2

GOLANGCI_LINT := $(BIN)/golangci-lint
GOFUMPT       := $(BIN)/gofumpt
STATICCHECK   := $(BIN)/staticcheck
DEADCODE      := $(BIN)/deadcode

# Linters that `lint` leaves to the dedicated `complexity` and `boundaries`
# steps, so each concern is reported under its own heading.
COMPLEXITY_LINTERS := gocyclo,gocognit,funlen,nestif
BOUNDARY_LINTERS   := depguard

# Every tracked or untracked-but-not-ignored Go file (skips .scratch/ etc.).
GO_FILES = $$(git ls-files --cached --others --exclude-standard -- '*.go')

.PHONY: shard-matrices test-unit-store test-unit-rest check check-fast check-gates check-static latency-names test-unit test-latency test-unit-cmd check-shards test-staged tools fmt fmt-check lint complexity boundaries suppressions vet staticcheck deadcode layout install-script test build release-snapshot clean

## check: run every gate, in order, under the lock. CI runs this; run it by hand for the full gate.
check:
	@scripts/with-test-lock.sh $(MAKE) --no-print-directory check-gates

## check-fast: the pre-commit gate, no lock: static checks and build only. No tests: run `make test-staged` or a narrow `go test -run` yourself; CI runs every test and is required to merge.
check-fast: check-static
	@echo "==> go build ./..."
	@go build ./...
	@echo "==> make check-fast: all gates passed"

## test-staged: race-test the packages the staged files touch (what check-fast used to run; run by hand before committing)
test-staged:
	@scripts/staged-tests.sh

## check-gates: the gates themselves; `check` runs them under the worktree lock
check-gates: check-static check-shards test
	@echo "==> make check: all gates passed"

## check-static: every gate that is not a test (CI job `static`; `check-gates` and `check-fast` run it too)
check-static: tools fmt-check lint complexity boundaries suppressions vet staticcheck deadcode layout install-script

## tools: install the pinned tool versions into .bin/
tools: $(BIN)/.golangci-lint-$(GOLANGCI_LINT_VERSION) \
       $(BIN)/.gofumpt-$(GOFUMPT_VERSION) \
       $(BIN)/.staticcheck-$(STATICCHECK_VERSION) \
       $(BIN)/.deadcode-$(DEADCODE_VERSION)

$(BIN)/.golangci-lint-$(GOLANGCI_LINT_VERSION):
	GOBIN=$(BIN) go install github.com/golangci/golangci-lint/v2/cmd/golangci-lint@$(GOLANGCI_LINT_VERSION)
	@touch $@

$(BIN)/.gofumpt-$(GOFUMPT_VERSION):
	GOBIN=$(BIN) go install mvdan.cc/gofumpt@$(GOFUMPT_VERSION)
	@touch $@

$(BIN)/.staticcheck-$(STATICCHECK_VERSION):
	GOBIN=$(BIN) go install honnef.co/go/tools/cmd/staticcheck@$(STATICCHECK_VERSION)
	@touch $@

$(BIN)/.deadcode-$(DEADCODE_VERSION):
	GOBIN=$(BIN) go install golang.org/x/tools/cmd/deadcode@$(DEADCODE_VERSION)
	@touch $@

## fmt-check: fail if any Go file is not gofumpt-formatted (never rewrites)
fmt-check: tools
	@echo "==> gofumpt (fail on diff)"
	@files="$(GO_FILES)"; \
	if [ -z "$$files" ]; then exit 0; fi; \
	out="$$($(GOFUMPT) -l $$files)"; \
	if [ -n "$$out" ]; then \
		echo "gofumpt: these files need formatting (run 'make fmt'):"; \
		echo "$$out"; \
		$(GOFUMPT) -d $$out; \
		exit 1; \
	fi

## fmt: rewrite Go files with gofumpt (a developer convenience, not a gate)
fmt: tools
	$(GOFUMPT) -w $(GO_FILES)

## lint: golangci-lint with the strict config in .golangci.yml
lint: tools
	@echo "==> golangci-lint (strict config, .golangci.yml)"
	$(GOLANGCI_LINT) run --allow-parallel-runners --disable $(COMPLEXITY_LINTERS),$(BOUNDARY_LINTERS) ./...

## complexity: complexity limits (thresholds in .golangci.yml, docs/decisions/0002)
complexity: tools
	@echo "==> complexity limits ($(COMPLEXITY_LINTERS))"
	$(GOLANGCI_LINT) run --allow-parallel-runners --enable-only $(COMPLEXITY_LINTERS) ./...

## boundaries: depguard package-boundary rules (docs/decisions/0003)
boundaries: tools
	@echo "==> package boundaries (depguard)"
	$(GOLANGCI_LINT) run --allow-parallel-runners --enable-only $(BOUNDARY_LINTERS) ./...

## suppressions: no lint suppressions of any kind may exist in the tree
suppressions:
	@echo "==> no lint suppressions (//nolint, //lint:ignore, #nosec)"
	@files="$(GO_FILES)"; \
	if [ -z "$$files" ]; then exit 0; fi; \
	if grep -nE '//[[:space:]]*nolint|//[[:space:]]*lint:(file-)?ignore|#nosec' $$files; then \
		echo "lint suppressions are not allowed (AGENTS.md, ENGINEERING)"; \
		exit 1; \
	fi

## vet: standalone go vet
vet:
	@echo "==> go vet"
	go vet ./...

## staticcheck: standalone staticcheck
staticcheck: tools
	@echo "==> staticcheck"
	$(STATICCHECK) ./...

## deadcode: unreachable functions, with test binaries as roots (docs/decisions/0004)
deadcode: tools
	@echo "==> deadcode -test"
	@out="$$($(DEADCODE) -test ./...)"; \
	if [ -n "$$out" ]; then \
		echo "deadcode: unreachable functions:"; \
		echo "$$out"; \
		exit 1; \
	fi

## layout: reject files outside the allowed directories (docs/decisions/0006)
layout:
	@echo "==> layout check"
	scripts/check-layout.sh

## install-script: offline tests of scripts/install.sh (no network, no real home) and of the staged-package mapping
install-script:
	@echo "==> install.sh tests"
	@sh scripts/install_test.sh
	@sh scripts/staged_tests_test.sh
	@sh scripts/ci_retry_once_test.sh >/dev/null
	@sh scripts/ci_docs_only_test.sh

## test: all tests under the race detector, in two steps
# Several packages hold latency gates (the hook's 20 ms budget, the end-to-end
# 50 ms budget). With packages running in parallel those gates measured the
# other packages' load, on CI most of all, so they must run alone. Everything
# else does not care, and running it one package at a time made the suite
# four times slower than it needs to be.
# Step 1: every test that is not a latency gate, packages in parallel.
# Step 2: the latency gates only, one package at a time, nothing beside them.
# Together the two steps run every test exactly once; no test and no
# threshold is changed. A latency gate is a test whose name contains
# "Latency", plus TestReplayFiveSessions: name new ones accordingly
# (scripts/check-latency-names.sh fails the gate otherwise).
LATENCY_TESTS := Latency|TestReplayFiveSessions
# CI-only shard counts (scripts/shard.sh): the ONE place they are set; ci.yml
# builds its matrices from `make shard-matrices`. Latency shards are by gate.
LATENCY_SHARDS := 6
CMD_SHARDS     := 6
# CI job `unit-store` runs this package alone; `unit-rest` runs every other
# package but cmd/curia (scripts/shard.sh list rest).
STORE_PKG := ./internal/store
export LATENCY_TESTS LATENCY_SHARDS CMD_SHARDS STORE_PKG
# `make test-unit` runs every package unless UNIT_PKGS says otherwise.
UNIT_PKGS ?= ./...

## shard-matrices: the CI matrices as GitHub outputs (read by the `plan` job in ci.yml)
shard-matrices:
	@echo "latency=[$$(seq 0 $$(($(LATENCY_SHARDS) - 1)) | paste -sd, -)]"; echo "cmd=[$$(seq 0 $$(($(CMD_SHARDS) - 1)) | paste -sd, -)]"

latency-names:
	@scripts/check-latency-names.sh

## test-unit: step 1 (CI jobs `unit-store` and `unit-rest` pass UNIT_PKGS)
test-unit: latency-names
	@echo "==> go test -race (all but the latency gates, packages in parallel): $(UNIT_PKGS)"
	@scripts/with-test-lock.sh go test -race -skip '$(LATENCY_TESTS)' $(UNIT_PKGS)

## test-unit-store, test-unit-rest: step 1 for CI jobs `unit-store` and `unit-rest`
test-unit-store:
	@$(MAKE) --no-print-directory test-unit UNIT_PKGS=$(STORE_PKG)
test-unit-rest:
	@$(MAKE) --no-print-directory test-unit UNIT_PKGS="$$(scripts/shard.sh list rest | tr '\n' ' ')"

## test-unit-cmd: step 1 for ./cmd/curia only, shard SHARD of CMD_SHARDS by test name (CI job `unit-cmd`)
test-unit-cmd: latency-names
	@run="$$(scripts/shard.sh list cmd | scripts/shard.sh pick $(SHARD) $(CMD_SHARDS) | paste -sd '|' -)"; \
	echo "==> go test -race ./cmd/curia, shard $(SHARD) of $(CMD_SHARDS)"; \
	scripts/with-test-lock.sh go test -race -skip '$(LATENCY_TESTS)' -run "^($$run)$$" ./cmd/curia

## test-latency: step 2; with SHARD set (CI job `latency`) only shard SHARD of LATENCY_SHARDS
## of the latency gates (by gate, not by package), each package still run alone with -p 1.
## Three passes, each running every gate with ONE batch (so a burst of runner
## noise spoils one batch of a gate, not all three); then internal/latency/verdict
## applies the median-of-three rule to the results file (docs/decisions/0012).
## A pass fails on a non-latency assertion; the p95 is judged by the verdict.
## One run only here; CI wraps it in scripts/ci-retry-once.sh (rerun once, D-15).
test-latency: latency-names
	@dir="$$(mktemp -d)"; rc=0; \
	if [ -n "$(SHARD)" ]; then scripts/shard.sh list latency | scripts/shard.sh pick $(SHARD) $(LATENCY_SHARDS) | scripts/shard.sh group >"$$dir/runs"; \
	else echo './... $(LATENCY_TESTS)' >"$$dir/runs"; fi; \
	for pass in 1 2 3; do \
		while read -r pkg re; do \
			echo "==> latency pass $$pass of 3: go test -race -p 1 -run '$$re' $$pkg (the latency gates alone, one batch each)"; \
			CURIA_LATENCY_SPACED=1 scripts/with-test-lock.sh go test -v -race -count=1 -p 1 -timeout 30m -run "$$re" $$pkg >>"$$dir/$$pass" 2>&1 || { rc=1; cat "$$dir/$$pass"; }; \
		done <"$$dir/runs"; \
		grep -E '^(--- (FAIL|SKIP)|ok |FAIL|panic)' "$$dir/$$pass" || true; \
	done; \
	echo "==> latency verdict (median of the three passes per gate)"; \
	cat "$$dir/1" "$$dir/2" "$$dir/3" | go run ./internal/latency/verdict || rc=1; \
	exit $$rc

## check-shards: fail unless the CI shards partition the latency gates and cmd/curia tests, and unit-store + unit-rest + cmd/curia are all packages (part of check-gates and CI job `static`)
check-shards:
	@echo "==> shard partition check"
	@scripts/check-shards.sh

test: test-unit test-latency

## build: build the curia binary into ./curia
build:
	go build -o curia ./cmd/curia

## release-snapshot: build every release archive locally into dist/ (gitignored); publishes nothing
release-snapshot:
	go run github.com/goreleaser/goreleaser/v2@$(GORELEASER_VERSION) release --snapshot --clean

## clean: remove the built binary and installed tools
clean:
	rm -rf curia dist $(BIN)
