# 0022. Setup script approval

Status: accepted (Plan 3 final records; Rulings 108 and 109). Amends plan decision 11 and spec F6 step 2.

## Context

Spec F6 runs the repository's `.curia/setup.sh` when a task is spawned. The script lives in the repository, and a task's worktree is created from a branch an agent may have written to, so running it unasked executes whatever an agent put there, as the user, with the user's environment. Task 19's security finding made this the one place where Curia itself would run agent-influenced code.

## Decision

- **A setup script runs only if the user approved that exact content for that repository.** `curia setup approve <task id>` (user-only, so the guard of decision 0021 applies) stores the SHA-256 of the script's bytes against the repository in the `meta` table. Spawn hashes the file, compares, and runs it; an unapproved or changed script is skipped with a note ("Setup: … skipped") and the spawn goes on. Hash, then execute, without a window in which the file can be swapped: the script runs from a private copy of the bytes that were hashed, with a ten-variable environment.
- **Approval shows the whole script and refuses what the display would hide or alter** (Ruling 109): control, escape or bidi characters, invalid UTF-8, an over-long line, too many lines, or a file over the size limit. The user types the first 8 hex digits of the hash on `/dev/tty`, not stdin, so a piped answer approves nothing, in addition to the user guard.
- **What it binds:** the script's bytes, one approval per repository. Two branches with different scripts replace each other's approval, and a changed script needs a new approval; the script runs from a private copy, so a path relative to `$0` breaks.

## Consequences

- Cost: one extra command per repository, and again after every change to the script.
- Verified by the final review at the desk: unapproved content was skipped, approval refused with `CLAUDECODE` set and without a terminal, a wrong hash prefix approved nothing, approved content ran from a private copy, and one appended line made the next spawn skip it again.
- It does not bind what the script runs. An approved `npm install` runs whatever `package.json` says when a worktree is reused by `resume` or a second spawn. The README says so.
- As with every guard in decision 0021, an agent that can write the `meta` row, or fake a terminal from outside Curia's reach, can approve its own script; the approval stops accidents and one-command evasions, not a determined agent running as the same OS user.
