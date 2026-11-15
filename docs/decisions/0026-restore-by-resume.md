# 0026. Restore by resume

Status: accepted (Plan 4 setup). Probes R1 to R3 have not run; their results can amend this record and Task 18.

## Context

Spec F12: after a reboot or a tmux crash the agents should come back with their context. Claude Code 2.1.325's `--help` lists `-r/--resume [value]` and `--fork-session` ("create a new session ID instead of reusing the original"), which implies a resume keeps its id. `updated_at` is reset when a row is marked stale (Plan 1 live-check defect 12), so it cannot say what was alive.

## Decision

- **`curia restore [--dry-run] [--yes] [--since <duration>]` is user-only, needs a running tmux server, shows its plan and asks before acting** (`--yes` for scripts, exit 1 without a terminal and without `--yes`). One restore runs at a time (`store.ClaimRestore`).
- **What is offered is decided by the transcript's modification time, within 24 hours by default** (`restore.max_age`, `--since`), and by `restorePlan`, a pure function that classifies every session that is not `ended`: live, other server, outside tmux, teammate, not Claude Code, bad id, paused, no directory, no transcript, too old, superseded, restore. At most 32 are restored a run.
- **Restore opens one new window per session with Plan 3's `NewWindow` and types nothing itself.** A session linked to a live task runs `curia launch <task> --resume <session id>` (plus the task's model and team) in the task's worktree with `CURIA_TASK_ID`; any other session runs `claude --resume <session id>` by absolute path in its cwd. No permission flag is ever added, so a session started with special flags comes back without them. Pane layouts are not restored (tmux-resurrect does that).
- **No stored text reaches tmux unvalidated:** the session id must be a UUID, the tmux session and window names are rebuilt from `[A-Za-z0-9_-]`, and `NewWindow` validates again.
- **For up to 30 seconds `restore` waits for each launched session to be live in its new pane;** a new id in that pane ends the old row and counts as restored.
- **A restored team lead is told by a message to respawn its team, and its teammates' rows are ended.** The message reaches it by typing, the Stop hook or `check_messages`.

## Consequences

- Probe R1 decides whether the same id comes back, whether a dialog comes first and whether the idle prompt is recognized; R2 whether a resumed session keeps its model (else `--model` is added for an unlinked session); R3 what a resumed lead sees of its former teammates. Each has its fallback in plan section 14.
- A session idle for more than a day before the crash needs `--since`.
- Plan section 4.10 and decisions 22 to 26 of section 13 carry the detail.
