# 0016. Tasks and derived status

Status: accepted (Plan 3 setup)

## Context

Spec F5 wants a board that never goes stale, and spec section 5 sketches a `tasks` table with a stored status. A stored `running` or `needs_decision` goes wrong whenever an agent exits, a pane is killed or the machine reboots, and repairing it needs a daemon Curia does not have. The exact DDL is in plan section 4.1 and lands as `internal/store/migrations/0004_tasks.sql` (Plan 3 Task 8).

## Decision

- **`tasks.status` stores only what the user sets: `backlog`, `ready`, `dropped`.** The `CHECK` rejects every other value. What is shown is `task.Derive`, on every read: stored `dropped` shows `dropped`; a live linked session (not `ended`, not `stale`) together with a `needs_decision` session or an open blocking ticket shows `needs_decision`; any live linked session shows `running`; otherwise the stored status. `review` and `done` exist as `model.TaskStatus` values and are never produced in Plan 3; Plan 4 adds two rows above `needs_decision` from its `reviews` table.
- **A session is linked to its task by `CURIA_TASK_ID`** in the agent's environment, read by `curia hook` on a session's first event and copied into `sessions.task_id` (a valid id matching `^T-[1-9][0-9]{0,8}$` only). The link survives `/clear` because the process keeps its environment, and it costs the hook one `getenv`. A session that already has a task keeps it. Probe P3 confirms that hooks run with the agent's environment; the fallback is matching on the pane `spawn` stored.
- **Differences from spec section 5:** `n` and a `UNIQUE` id, as `decisions` has; `branch`, `worktree`, `team`, `agent_model`, `pane`, `resume_session`, `launched_at`, `paused_at` and `cleaned_at` are added for F6; `sessions` gains three team columns (`team_id`, `team_role`, `team_member`) written only by `store.SetTeams`.
- **Ids** are `T-<n>` (and `M-<n>` for messages), decimal, from 1, allocated inside the creating transaction as `D-<n>` is and never reused.
- `store.upsert` gains `task_id` and `worktree` and still names its columns, so the team columns and Plan 2's cache columns survive every session write, from this binary and from an older one.

## Consequences

- Verified while writing the plan with the system `sqlite3` 3.51.0 after applying 0001 to 0003: the script runs as one multi-statement script in one transaction, the `CHECK` rejects `status = 'running'`, the id expression yields `T-1`, and `decisions_fts` passes `integrity-check` afterwards. Task 8 proves it again on the pinned `modernc.org/sqlite`.
- A task whose agent has gone shows its stored status again (`curia spawn` sets a `backlog` task to `ready`), so it reads `ready`, not `done`. If Plan 4 wants a stored `done`, it needs a migration to widen the `CHECK`.
- `0004_tasks.sql` is the plan's only migration; a later one is `0005` or higher, created by the orchestrator.
