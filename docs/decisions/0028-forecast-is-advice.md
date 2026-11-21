# 0028. A forecast is advice

Status: accepted (Plan 5 setup). The plan numbers this record 0026.

## Context

Spec F16 wants a warning when the usage limit will land before the reset, attribution of who burns it, a suggestion to pause the lowest-priority task, and a cheaper model for new agents. The inputs are numbers any local process can write (decision 0027), and spec section 7 says only the owner decides, approves, merges or pauses.

## Decision

- **Burn** is measured over the trailing 20 minutes from at least two samples at least 3 minutes apart, merged across sessions and cut at a drop (a drop means the window reset). **No warning without a reset time**: the warning is "the limit lands before `resets_at`".
- **The badge** carries the spec's sentence (`5h 62% -> limit ~48m (resets 1h50m)`) and only the sooner of two warnings; no tmux option is changed for it. The API-mode projection (`next 2h ≈ $14 at the current rate`) is shown by `curia usage` and the dashboard, not the badge.
- **Attribution is in dollars per hour**, not token deltas: the contract carries no token counter and Curia keeps no price table.
- **Accepting the pause suggestion is `P` then `y`**, one key more than the spec, because a pause commits work and closes a pane. It considers only tasks whose derived status is `running`, takes the lowest priority literally (P3 before P2 before P1; ties by burn, then by the higher id), and never acts without the owner.
- **The cheaper model** is a configured name (`usage.cheaper_model`, default `sonnet`, empty turns it off) used by `curia spawn` under a warning for a Claude Code task with no model chosen; `--model` always wins. The cost-lens advisor is not used (it has no command to call).
- **Priority** is an integer 1 to 3 on the task, set by `task add` and `task edit`; the board does not show it.
- No MCP tool is added; the surface stays at Plan 4's six.

## Consequences

- A forged series can produce a wrong warning, suggestion or default model; the owner sees and overrides each.
- A single long turn produces no sample and so no warning until it ends.
- The pause suggestion can name the only running task.
- Owner questions 2, 3 and 11 of the plan record the defaults taken.

Amended 2026-11-19 (Plan 5 records; Rulings in `PROGRESS.md`):

- Ruling 169 (PR #140, merged): the cheaper model applies to the one spawn it was chosen for and is not stored on the task; a stored model is used only with the agent it was stored for, and a task whose agent changes loses its model.
- Ruling 163: where a dollar figure is unknown, cells and the stakes line show tokens, never an invented dollar figure.
- Migration `0007_usage_forecast.sql` (PR #135): the badge's forecast reads `usage_samples` through a covering index (read p95 under `-race` 16 to 19 ms down to about 7 ms). The advice itself is unchanged.
- Owner decisions D-1 and D-12 (Rulings 172 and 183): a latency gate passes on the median of three spaced batches, scaled by a reference workload. It does not change this record's rules. Final review C-2 (PR #173): the pause suggestion prefers a task whose agent is idle in the lowest-priority tier; otherwise it names the task and offers no `P`, because `curia pause` refuses a task whose session is working or waiting unless forced.
