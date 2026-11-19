# 0026. Restore by resume

Status: accepted (Plan 4 setup). Probes R1 to R3 ran on 2026-11-14 and amended it (see the amendment below).

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

Amended 2026-11-19 (Plan 4 final records; Rulings in `PROGRESS.md`):

- Probes R1 to R3 (Claude Code 2.1.325, tmux 3.8): a resume keeps the session id, fires SessionStart with source `resume` about a second after the window opens, and keeps the session's own model (the resume payload has no model field); no dialog comes first in a trusted directory (the trust dialog in an untrusted one, `/rate-limit-options` after a usage limit); for 10 to 18 seconds the footer reads `· ← for agents`, which is not an empty prompt; a missing directory is no error for `tmux new-window -c` (it starts in HOME) and the resume still works, so restore's directory check is the only guard; two windows can resume one id with no warning. After a lost server Claude Code deletes the team directory, the resumed lead re-creates it listing only itself and respawns its teammates when told, as new sessions in new panes; teammate panes take the tmux server's global environment, not the lead's.
- Ruling 124: "classifies every session that is not `ended`" above is amended: tmux kill, a killed window and SIGTERM make Claude Code fire SessionEnd with reason `other`, so a session is a candidate when it is not ended or ended with reason `other`. `sessions.end_reason` stores why a session ended (a missing reason is stored as `other`).
- Ruling 130: a session whose pane was closed while the tmux server kept running (it started at or after the listed server's start, and is not on another server) is skipped with "its pane was closed while tmux kept running; reopen it with claude --resume <id> in <dir>" unless `--include-closed`.
- Fixes from the final review and Task 18: a window opened for a session with no task ends the row in the same guarded update, so a second `curia restore` opens no second window; the restore claim is taken again for 10 minutes after the confirmation; the team columns of stale rows are left as they are, so a taskless team's teammates are not offered as sessions of their own; the lead's team notice is delivered by the usual message path and, when the lead's prompt is not yet empty, arrives at its next Stop or through `check_messages`.
