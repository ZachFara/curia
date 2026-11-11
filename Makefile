# Curia build and check entry points.
#
# `make check` is the single gate. The lefthook pre-commit hook and CI both
# run exactly `make check`, so they can never diverge (docs/decisions/0007).
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

.PHONY: check tools fmt fmt-check lint complexity boundaries suppressions vet staticcheck deadcode layout test build clean

## check: run every gate, in order. This is what the hook and CI run.
check: tools fmt-check lint complexity boundaries suppressions vet staticcheck deadcode layout test
	@echo "==> make check: all gates passed"

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
	$(GOLANGCI_LINT) run --disable $(COMPLEXITY_LINTERS),$(BOUNDARY_LINTERS) ./...

## complexity: complexity limits (thresholds in .golangci.yml, docs/decisions/0002)
complexity: tools
	@echo "==> complexity limits ($(COMPLEXITY_LINTERS))"
	$(GOLANGCI_LINT) run --enable-only $(COMPLEXITY_LINTERS) ./...

## boundaries: depguard package-boundary rules (docs/decisions/0003)
boundaries: tools
	@echo "==> package boundaries (depguard)"
	$(GOLANGCI_LINT) run --enable-only $(BOUNDARY_LINTERS) ./...

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

## test: all tests under the race detector
test:
	@echo "==> go test -race ./..."
	go test -race ./...

## build: build the curia binary into ./curia
build:
	go build -o curia ./cmd/curia

## clean: remove the built binary and installed tools
clean:
	rm -rf curia $(BIN)
