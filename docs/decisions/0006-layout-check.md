# 0006. Layout check

Status: accepted (Phase 1 setup)

## Context

Agents leave throwaway files behind: probe scripts, notes, stray binaries, ad hoc packages. The owner wants a check that rejects files outside an allowed list of directories, and throwaway work kept in `.scratch/`.

## Decision

`scripts/check-layout.sh`, run by `make layout`, checks every file from `git ls-files --cached --others --exclude-standard` (tracked plus untracked-but-not-ignored, so a forgotten file fails before it is ever added):

- Root files allowed: `.gitignore`, `.golangci.yml`, `AGENTS.md`, `CLAUDE.md`, `Makefile`, `PROGRESS.md`, `go.mod`, `go.sum`, `lefthook.yml`.
- Top-level directories allowed: `cmd`, `internal`, `e2e`, `testdata`, `docs`, `scripts`, `.github`.
- Under `internal/`, only the packages in the plan's File structure table: `model`, `config`, `store`, `state`, `transcript`, `hook`, `tmux`, `fleet`, `attention`, `ui`, `install`.
- Under `cmd/`, only `curia/`.
- Go source only under `cmd/`, `internal/` and `e2e/`.

Gitignored paths (`.scratch/`, `.superpowers/`, `.worktrees/`, `.bin/`, the built `curia` binary) are invisible to the check by construction.

## Consequences

- Verified during setup: a stray root file, an unknown top-level directory, an unlisted `internal/bogus/` package and a `.go` file under `docs/` each fail.
- Adding a root file (for example `README.md` or `LICENSE`), directory or package is a shared-file change by the orchestrator, alongside the matching depguard rule (decision 0003).
