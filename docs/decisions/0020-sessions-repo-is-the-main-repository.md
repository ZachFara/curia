# 0020. A session's repo is the main repository

Status: accepted (Plan 3 setup). Closes Plan 2 Open Question 8.

## Context

`sessions.repo` has been the top level of the worktree a session runs in. Once `curia spawn` creates a worktree per task, every task would be its own "repo", and repo-scoped precedent, `@repo` addressing and the board's grouping would all miss.

## Decision

- **`sessions.repo` is the main repository and `sessions.worktree` the linked worktree.** The hook's one git process becomes `git -C <cwd> rev-parse --path-format=absolute --show-toplevel --absolute-git-dir --git-common-dir` (three lines; no added process). When the common directory's base name is `.git` and its parent differs from the top level, the session is in a linked worktree: `repo` is the parent of the common directory and `worktree` is the top level. Otherwise `repo` is the top level, as before, and `worktree` is empty.
- `decisions.repo` and `mcp.Caller.Repo` come from the session row, so they follow. `decisions.task_id` is filled from now on from the caller session's `task_id`.
- `curia spawn` does not set `CURIA_SESSION_ID` (reverses a note in Plan 2 and answers its final-review M12): `curia mcp` gives that variable first place and it goes stale at `/clear`; a spawned agent has its own pane, which resolves it.

## Consequences

- Rows written before the upgrade keep their old value until the session's next SessionStart. Decisions raised earlier from a hand-made worktree keep that worktree's path as their repo and stop matching; they can be rescoped with `curia decisions edit`.
- Verified at the desk on git 2.56.0 in a main and a linked worktree. Plan 3 Task 14 owns the change (`TestSessionInLinkedWorktreeStoresMainRepo`).
