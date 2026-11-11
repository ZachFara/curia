# 0009. All Plan 1 dependencies pinned up front

Status: accepted (Phase 1 setup)

## Context

`go.mod` and `go.sum` are shared files. If parallel tasks each added the dependency they need, every wave would end in `go.sum` merge conflicts, and versions would be chosen ad hoc by whichever implementer went first.

## Decision

- Phase 1 adds every dependency Plan 1 names as a direct requirement, latest stable at setup time:

  | Module | Version |
  | --- | --- |
  | github.com/spf13/cobra | v1.10.2 |
  | github.com/charmbracelet/bubbletea | v1.3.10 (v1 line, this exact import path) |
  | github.com/charmbracelet/lipgloss | v1.1.0 |
  | github.com/BurntSushi/toml | v1.6.0 |
  | modernc.org/sqlite | v1.62.0 |

- `go.sum` is complete for all five: it was filled by `go mod tidy` while a temporary file imported all five, before that file was deleted. A throwaway program importing all five (bubbletea and lipgloss rendering, a cobra command, a TOML decode, a modernc SQLite database opened in WAL mode) was built and run with this module's `go.mod` and `go.sum` under `GOFLAGS=-mod=readonly GOPROXY=off`, and both files were byte-identical afterwards.
- No task edits `go.mod` or `go.sum`.
- Never run `go mod tidy` during Plan 1. Only cobra is imported so far, so tidy would strip the other four. For the same reason there is no tidy check in `make check` or CI.
- No test-only helper modules (testify, teatest and so on). Tests use the standard library.

## Consequences

- Any task can import any of the five with no network and no `go.mod` change.
- Until a package imports them, four requirements are unused; that is expected.
- A task that needs a module not on this list reports it; the orchestrator decides and makes the change on `plan-1` between waves, together with the depguard allow entry (decision 0003).
- After Plan 1 merges, the first `go mod tidy` should be a no-op except for `// indirect` markers; if it removes a module, that module was never used and should go.
