# 0007. One `make check` for the hook and CI

Status: accepted (Phase 1 setup)

## Context

When the pre-commit hook and CI run different commands, they drift: something passes locally and fails in CI, or CI quietly checks less. The owner requires one check command wired into both.

## Decision

- `make check` is the only gate. In order: install pinned tools, gofumpt (fail on diff), golangci-lint, complexity limits, package boundaries, the suppression ban, `go vet`, staticcheck, `deadcode -test`, the layout check, `go test -race ./...`. Each step prints a `==>` heading.
- The lefthook `pre-commit` hook runs exactly `make check`.
- CI (`.github/workflows/ci.yml`, GitHub Actions, `ubuntu-latest`) checks out, sets up Go from `go.mod` (its `toolchain` line), restores the `.bin/` cache, installs tmux with apt so the real-tmux test runs, and then runs exactly `make check` as its only check.
- The CI job and its status check are both named `check`. Branch protection on `main` requires that name, so it must not be renamed.
- Tool versions come from the Makefile in both places (decision 0002).

## Consequences

- Changing what is checked means editing the Makefile only; hook and CI follow automatically.
- `go test -race` needs cgo on Linux; `ubuntu-latest` ships gcc. The race detector is a test-time tool and does not conflict with the pure-Go rule for the binary (decision 0001).
- There is deliberately no `go mod tidy` check (decision 0009).
