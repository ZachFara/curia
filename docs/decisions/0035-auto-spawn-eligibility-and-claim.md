# 0035. Auto-spawn: what makes a task eligible, and the claim

Status: accepted (2026-11-20, Rulings 211 and 212; owner decisions D-7 = A and D-8 = B; code in `internal/task/autospawn.go`, `cmd/curia/autospawn.go`, `internal/store/autospawn.go`).

## Context

D-8 made auto-spawn a real feature: the monitor starts ready tasks by itself. D-7 added that auto-spawn may start agent-filed tasks, but always in a new session, never by assigning one to a running agent.

## Decision

- Off by default (`tasks.auto_spawn.enabled`). It lives in the user file; a repository file may only turn it off or lower the caps. `curia auto-spawn on|off` are user-only.
- One pure function, `task.AutoEligible`, holds the whole rule and names the first rule that fails (14 checks). A task qualifies only if it is `ready`, not paused, its stored `ready_hash` equals the hash of its current title and body, and it has no open blocking ticket. `ready_hash` is written only by `curia task ready`, the board's `r` key, and by `file_task` when the user's own `tasks.agent_filed.level` is `ready`. A web move, an edit of `--status`, a launch or an agent's spawn leave none.
- Then the gate: no session in bypass, live agents under `max_agents` (3), starts in 24 h under `max_per_day` (5), `interval` (5 min) since the last start or failed attempt, no forecast warning or full usage window, then per task: budget not spent, setup script approved.
- The claim is one UPDATE (`AutoClaim`): it needs the stored hash and the title and body that were read, no start within the interval, fewer than the day's cap (failed spawn attempts count), sets `auto_spawned_at` and clears the hash. A launch therefore consumes the task's readiness, so a task whose session ended without review is not restarted by itself. A failed spawn gives the claim back (`AutoRelease`) and logs `spawn_failed`.
- It never types into a pane; `spawnTask` only creates a window.

## Consequences

- The Docker memory rule reads the newest Docker VM sample while fresh and uses `resources.docker_memory_refuse_pct` (0 = off); the usage "full" check uses `schedule.limited_pct`. (The first build had a stub that never fired; fixed in PR #209.)
- Its log is its own table `auto_spawn_runs` (migration 0020), not `schedule_runs` as the spec says.
- With `tasks.agent_filed.level = ready` an agent can file a task the monitor then starts without the user reading it. The default level is `backlog`.
