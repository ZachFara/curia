# 0033. One decision point for PreToolUse: denials first, allows narrow

Status: accepted (2026-11-20, Rulings 195, 196, 198, 199 and 219; code in `internal/hook/decide.go` and `mode.go`).

## Context

Plan 6 needs the PreToolUse hook to deny (protect Curia's own files, stop a task over its budget) and to allow (modes, F20). The live probe on Claude Code 2.1.329 showed: a hook `allow` never overrides a deny rule, an ask rule or plan mode; a hook `deny` or exit code 2 blocks and the reason reaches the model; a hook that exits 1 or times out FAILS OPEN to the normal prompt. Curia's own hook already exits 0 and is bounded under every store failure.

## Decision

- `preToolUse` in `internal/hook/decide.go` is the only place a PreToolUse answer is made. Order: (1) `protectedFile` (an edit tool aimed at Curia's config, data or state directory, or its database or log), (2) the budget hard stop (only six of Curia's seven MCP tools still pass, not `file_task`: a stopped agent must not file work auto-spawn could start past the stop), (3) `modeLine`. The first two are denials and nothing after them can undo one. Modes only add `allow` lines (and away's `deny`) after both.
- Path checks fail closed: one component-by-component walk (`walk`) that follows symlinks, refuses `..` after a missing directory, a loop, more than 40 hops or a path over 4096 bytes. Deny checks fold case; allow checks keep it (`insideExact`).
- An allow needs a root the user froze (see decision 0034). With no root, no path is allowed.
- When the store cannot be read the budget check says nothing and the hook logs it as undecided; file protection needs no store and still holds (`PreToolUseNoStore`).
- Because a hook can fail open, `curia install` also writes permission deny rules (`Edit(~/.config/curia/**)`, `~/.local/share/curia`, `~/.local/state/curia`, plus the XDG and relocated equivalents), which Claude Code enforces itself.

## Consequences

- Bash is not parsed: a shell command that writes a protected file is not caught by either layer.
- The deny rules name the home defaults, the `XDG_*_HOME` directories in use and a relocated database or log; the absolute `Edit(//path)` form was taken from Claude Code's documentation and never run against it.
- A check-then-use race exists (a symlink made after the check is not seen).
- The budget stop can land one tool call late. A later contributor who adds an allow before the denials, or adds a path check that is not fail closed, undoes the safety property that bypass relies on.
