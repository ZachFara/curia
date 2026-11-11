# 0001. Go 1.26 and pure-Go dependencies

Status: accepted (Phase 1 setup)

## Context

The spec (section 8) fixes Go: hooks run on every tool call, so a compiled binary with near-zero startup matters more than development speed, and Bubble Tea is the strongest TUI library for the popup. The plan's global constraints require Go 1.26 and pure-Go dependencies only (no cgo), so `GOOS=linux` and `GOOS=darwin` builds work from either machine.

## Decision

- One module, `github.com/ZachFara/curia`.
- `go.mod` declares `go 1.26.0` and `toolchain go1.26.11`. The `go` line cannot be the bare `go 1.26`: `modernc.org/sqlite`, `modernc.org/libc` and `golang.org/x/sys` declare `go 1.26.0`, and Go orders `1.26` before `1.26.0`, so the go command rewrites it. The `toolchain` line pins the exact patch release: `actions/setup-go` reads it, so CI and local builds use the same Go.
- Dependencies are pure Go: `modernc.org/sqlite` (not `mattn/go-sqlite3`), `spf13/cobra`, `charmbracelet/bubbletea` (v1 line, import path `github.com/charmbracelet/bubbletea`), `charmbracelet/lipgloss`, `BurntSushi/toml`. Versions are in decision 0009.
- On the dev Mac, Go 1.26 is a keg-only Homebrew formula at `/opt/homebrew/opt/go@1.26/bin`, not on PATH by default. Repository files (Makefile, hooks, CI) call plain `go` and never hardcode that path.

## Consequences

- A `CGO_ENABLED=0 GOOS=linux` build of every dependency was verified during setup.
- Raising the toolchain is a one-line `go.mod` change made by the orchestrator, and it also invalidates the CI tool cache (decision 0002).
- Anyone running `make` on the Mac must put Go 1.26 on PATH first (AGENTS.md).
