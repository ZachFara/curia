# 0017. Curia runs git and tmux itself for spawn

Status: accepted (Plan 3 setup). The probes behind it (P1 to P4) are recorded as Rulings from 97 in `PROGRESS.md`.

## Context

Spec F6 says "native worktrees first": wrap `claude -w <name> --tmux`. The spec's own steps also fix the worktree path, the branch name and the base branch, and Claude Code 2.1.323's `-w/--worktree [name]` has a flag for none of them. Its `--tmux` help says it creates a tmux session and uses iTerm2 native panes when it can, so on this Mac it may open no tmux window at all, and it cannot be pointed at a private test server. `pause`, `resume` and `clean` need Curia's own git code whatever spawn does.

## Decision

1. **Curia runs `git worktree` and `tmux new-window` itself** (plan decision 1). Path `<dir of repo>/.curia-worktrees/<base of repo>-<id>`, branch `curia/<id>-<slug>`, from the task's base branch. If the owner chooses the native wrap (plan question 1), only `spawnTask` steps 2 and 4 change.
2. **The agent is started by a hidden `curia launch <id>` inside the new window**, which reads the brief from the store and replaces itself (`syscall.Exec`) with `claude` (plan decision 2). A brief file inside the worktree would pollute `git status`, and one outside it would cost the agent a permission prompt for its first read.
3. **No untrusted text is ever an argument to tmux.** tmux reads an argument equal to `;`, or ending in `;`, as a command separator and runs what follows as a tmux command (seen on tmux 3.8: `unknown command: $(id) ...`). `tmux.Client.NewWindow` takes only values Curia built from validated ids and paths (the binary path, fixed words, a task id, a validated model name, a session uuid) and refuses anything else. The brief reaches `claude` through `execve`, byte for byte.
4. **The brief is the agent's first prompt, one argument, at most 48 KiB of body** (plan decision 3), which stays in the transcript across `--resume`. A session is linked by `CURIA_TASK_ID` (decision 0016).
5. **Setup, pause, clean:** `.curia/setup.sh` runs only as a regular, executable file inside the new worktree, directly, for at most 10 minutes, its output in a 0600 log capped at 1 MiB (plan decision 13). `pause` kills the task's panes, then commits, then removes the worktree, and never passes `--force` to git. `clean` requires the branch to be an ancestor of its base, or `--force` (plan decisions 14 to 16).
6. **A second spawn is held off by a claim on `launched_at` for 11 minutes** (the setup limit plus one), taken in one write transaction (plan decision 12).

## Consequences

- Verified at the desk on tmux 3.8 and git 2.56.0: tmux's handling of `;`, direct execution of several arguments after the flags of `new-window`, `-e NAME=value` (a variable for that pane only), `-c`, `-P -F`, exact-match targets and `capture-pane -S`; `git worktree remove` refuses a worktree with modified or untracked files and silently deletes one holding only ignored files; `git branch -d` refuses a branch a worktree has checked out; `merge-base --is-ancestor`.
- Probes P1 (what the native path creates), P2 (whether the trust dialog appears at the sibling worktree path), P3 (environment reaching hooks, a 40 KiB prompt) and P4 (`--resume` after the worktree is re-created) can amend this record; where one does, the amendment is dated here and its Ruling is in `PROGRESS.md`.
- Curia's worktrees are unknown to Claude Code's own worktree commands. A window closes when the agent exits.
