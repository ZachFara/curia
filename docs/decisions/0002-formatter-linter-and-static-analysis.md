# 0002. Formatter, linter and static analysis

Status: accepted (Phase 1 setup)

## Context

Plan 1 is built by parallel implementers. They need a gate strict enough that review time goes to design rather than style, and the gate must be identical on every machine. The owner's rules forbid loosening any check later: no new suppressions, exclusions or raised thresholds.

## Decision

Tools, pinned in the `Makefile` and installed into the gitignored `.bin/` with `GOBIN=$(CURDIR)/.bin go install <pkg>@<version>`. They are not tracked in `go.mod`.

| Tool | Version | Run by |
| --- | --- | --- |
| gofumpt | v0.12.0 | `make fmt-check`: `gofumpt -l`, fails on any diff and prints it; never rewrites |
| golangci-lint | v2.15.0 | `make lint`, `make complexity`, `make boundaries` |
| go vet | toolchain | `make vet` (standalone, in addition to golangci-lint's govet) |
| staticcheck | v0.8.2 (2026.2.2) | `make staticcheck` (standalone, default checks) |
| deadcode | golang.org/x/tools v0.52.0 | `make deadcode` (decision 0004) |

golangci-lint (`.golangci.yml`, `default: none`, explicit list):

- Correctness: govet (all analyzers except `fieldalignment`, because struct field order follows the plan's documented types), staticcheck, errcheck (type assertions checked), ineffassign, errorlint, bodyclose, nilerr, rowserrcheck, sqlclosecheck, gosec.
- Unused code: unused, unparam.
- Complexity: gocyclo, gocognit, funlen, nestif.
- Boundaries: depguard (decision 0003).
- Style: revive (default rules, including doc comments on exported names), misspell (US), unconvert, gocritic (default checks).
- Suppression hygiene: nolintlint with `require-explanation`, `require-specific` and `allow-unused: false`.

Complexity thresholds. These are fixed and will never be raised.

| Linter | Limit | Applies to tests |
| --- | --- | --- |
| gocyclo | cyclomatic complexity at most 15 | yes |
| gocognit | cognitive complexity at most 20 | yes |
| nestif | if-nesting complexity at most 4 | yes |
| funlen | 60 lines and 40 statements per function, comments not counted | no |

Two exclusions, both set now and never extended:

- funlen does not apply to `_test.go`: table-driven tests are long by nature. The complexity limits still apply to tests, so long tests must stay flat.
- gosec G204 (subprocess launched with variable arguments) is off. The tmux adapter, the git lookup in the hook and the e2e test run fixed program names with argv arguments by design, never through a shell; G204 fires on every such call (verified during setup). All other gosec rules stay on: use `filepath.Clean` on paths you open (G304) and `0o600` for files you write (G306).

Suppressions: `make suppressions` fails if any Go file contains `//nolint`, `//lint:ignore`, `//lint:file-ignore` or `#nosec`. The tree has zero suppressions, so nolintlint is a second line of defence, not an escape hatch.

## Consequences

- Each concern prints under its own heading in `make check`: lint, complexity limits, package boundaries, suppressions.
- golangci-lint, staticcheck and deadcode take about a minute to build on first use; `.bin/` is cached in CI, keyed on the Makefile and `go.mod`.
- golangci-lint upstream does not guarantee `go install` builds; if v2.15.0 ever fails to build with our toolchain, pin a different version in the Makefile rather than switching install methods silently.
- A check that seems wrong is recorded under Open Questions in PROGRESS.md, never loosened.
