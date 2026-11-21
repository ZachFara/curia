# 0034. Modes, the frozen allow root, and away

Status: accepted (2026-11-20, Rulings 199 to 202; owner decision D-6 = C; code in `internal/hook/mode.go`, `cmd/curia/mode.go`, `away.go`, `internal/store/mode.go`, `away.go`).

## Context

F20 lets the user decide how much of Claude Code's prompting Curia answers. Security review of the merged modes found 13 problems, among them an allow root that followed the agent's current directory and case-insensitive containment for allows.

## Decision

- Four modes (`normal`, `edits`, `away`, `bypass`), set only by the user (`curia mode`, `curia away`). `bypass` means "allow what the user's settings do not forbid" (D-6 = C), never "no prompts"; `AskUserQuestion` and `ExitPlanMode` get no opinion. It needs the word `bypass` typed at the terminal and always expires (`mode.bypass_for`, default 2h).
- The allow root is frozen by `store.SetModes` into `sessions.mode_root` (the worktree, else the repository) when the mode is set (migration 0023). It is never taken from a hook event, never the home directory, a parent of it or `/`. The call's cwd and every path must lie inside it, with exact-case comparison. No root, no allow.
- `edits` allows edit tools inside the root, except under auto-executed names (`.git`, `.claude`, `.curia`, `.husky`, `.mcp.json`, lefthook files and others in `autoExecNames`, and a gitdir a `.git` file points to). Build files that a later command runs stay editable: a documented residual risk.
- `away` adds reads inside the root (Glob and Grep need an explicit path and a safe pattern) and Bash commands whose first words equal an `mode.away_allow` entry, with plain arguments only (no option, no shell metacharacter, paths inside the root). This is stricter than the spec's globs on purpose. Everything else is denied with a reason, recorded in `permission_events`, and listed by `curia back`.
- While away a blocking ticket is stored as an assumption with no expiry (`decisions.coerced_from`); Curia answers nothing for the user. The monitor ends the period at its deadline; the hook stops applying away at that time regardless.
- `--native` only prints the relaunch command for idle sessions; Curia neither relaunches nor answers Claude Code's acceptance dialog.

## Consequences

- A session on `mode.default = edits` that was never `set` has no root and gets no allow; `curia away` does not freeze a root either (a follow-up was in flight when this was written): away is safe but allows no in-worktree edit until a root is frozen.
- Away's Bash allow is a string heuristic, not a sandbox. Curia restores its own mode rows only, not Claude Code's own permission mode.
