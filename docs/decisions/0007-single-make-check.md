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

Amended 2026-11-19 (owner, ticket D-3, answer "D", then D-11): the pre-commit hook no longer runs `make check`. Today it runs `make check-fast`: the static gates (`check-static`) and `go build ./...`, no tests and no machine-wide test lock. This replaced two earlier forms, both recorded here: the original "hook runs exactly `make check`", and D-3's first version of `check-fast`, which also ran `go test -race` (latency tests skipped) of the packages the staged files touch. Why: every commit in every worktree ran the whole gate (about 12 minutes) behind one lock, so eight agents waited about 90 minutes while CI ran the same suite again; and the touched-packages tests still took about 7 minutes for `cmd/curia` alone under load, so D-11 (answer "A") dropped tests from the hook. `make test-staged` runs the touched packages' tests by hand. CI is the full gate: it runs every test and is required on `main` and on every `plan-*` branch (GitHub ruleset "plan branches: CI must pass"; `main`'s branch protection, which also requires the branch to be up to date, D-4). `make check` is unchanged and remains the command for the full gate by hand. The "hook and CI run the same command" rule above no longer holds; decision 0005's line on the hook is amended the same way.

Amended 2026-11-19 (owner, ticket D-3 answer "D", and D-1): CI runs the same gate as parallel jobs, each a Makefile target that `make check` also runs (`check-static`, `test-unit` split into `cmd/curia` and every other package computed with `go list`, `test-latency` alone on its own runner), plus a final job named `check` that needs all of them and fails unless every one succeeded. The required status `check` is that aggregate job. `make check` is unchanged and still the full gate by hand.

2026-11-19: CI shards the `cmd/curia` unit tests (4 shards by test name) and the latency step (3 shards by package, each still `-p 1`), computed by `scripts/shard.sh`; `make check-shards` (in `check-gates` and CI `static`) fails unless the shards partition the set. `make check` still runs everything unsharded.
