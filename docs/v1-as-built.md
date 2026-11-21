# Curia V1 as built

`docs/DESIGN.md` is the owner's spec and is not edited by this document. This lists where V1 differs from it or says more than it does, one line each, with the ruling (a numbered "Ruling N" in `PROGRESS.md`) or decision that made it. Features are F1 to F18 of the spec. "Spec amended" marks a difference whose note is already in `docs/DESIGN.md`. Nothing here describes the later features (F19 onward): they are specified in `docs/DESIGN.md` on `main` and in progress.

What was checked: the statements below were read against the code of the `plan-5` branch and `curia --help`. Not checked by a live run: the Plan 5 live check with real sessions has not been run; probes X1 (Codex hooks), L1 (Linear), O1 (Obsidian), a real usage limit, the phone button and Remote Control are open and are the owner's.

## Data and storage

| Difference | Source |
| --- | --- |
| The schema grew past the spec's five tables. Ten migration files, applied in name order and tracked by file name: `0001_init`, `0002_decisions` (decisions with an integer key, `meta`), `0003_typed_confirmation`, `0004_tasks` (tasks, messages, team columns), `0005_reviews` and `0005_send_limits` (two files carry 0005), `0006_cost` (usage and cost samples, `sessions.recache_tokens`, `tasks.priority` and `agent`, `decisions.exported_at`), `0007_usage_forecast` (an index), `0008_context_live`, `0009_session_checked_cwd`, `0010_task_pane_socket`. Pushes, reviews and messages have tables the spec does not. | decisions 0014, 0016, 0023, 0024, 0027; Rulings 133, 176, 179, 182, 186 |
| A task's `review` and `done` statuses are derived from its newest review, never stored; `tasks.status` keeps only backlog, ready and dropped. | decisions 0016, 0023 |
| Every store write waits in a fair queue (a file lock beside the database plus an in-process queue); reads never wait for it. | decision 0032; Rulings 177, 187, 188 |
| A session records which tmux server its pane is on; every place that matches a pane id also matches the server. | decision 0025; Rulings 133, 184 |

## Sessions, attention and the dashboard (F1 to F4)

| Difference | Source |
| --- | --- |
| SessionStart sets a session `idle`, not `working`. Spec amended. | Ruling 37 |
| The badge is `D<n> Q<n> W<n> R<n>`: open blocking tickets, answers a live session has not received, waiting sessions, reviews. Spec amended. | Ruling 91 |
| A permission prompt shows up to 6 seconds late: Claude Code sends no hook until then. A denied prompt or an interrupt fires no hook; the next refresh settles the session from the transcript. | Rulings 48, 77 |
| Context and its percentage come from Claude Code's status line when it has reported; a table or a default is only a fallback; a figure over 100% shows `?`. Spec amended. | Rulings 142, 179 |
| Sessions whose pane is gone are marked stale and hidden after `stale_hide_after` (default 10 minutes). | `curia ls --help` |
| The tmux snippet adds to your window formats and binds a key only if it is free. | Rulings 94 to 96, 103 |
| The marker has a blue dot for a review you have not seen as well as the red one. | Plan 4 decision 20 |

## Decisions, messages and tasks (F5 to F9, F18)

| Difference | Source |
| --- | --- |
| Six MCP tools in V1, none of which answers a ticket, sends, spawns, pauses, approves or merges: `search_decisions`, `request_decision`, `check_decision`, `withdraw_decision`, `check_messages`, `complete_task`. Plan 6 adds a seventh, `file_task`, which only files a task (the code now has seven). | `curia mcp --help` |
| A superseded answer is never delivered as live; it goes out as a fixed retraction sentence. A `curia mcp` started before an upgrade keeps returning one as live until its session restarts. | Rulings 82, 90 |
| An answer is typed into a pane only after a capture shows Claude Code's empty prompt; otherwise it waits for the Stop hook or `check_decision`. | Rulings 85, 110; decision 0013 |
| `task edit` uses only the `editor` config option; `$EDITOR` and `$VISUAL` are not read. Spec amended. | Ruling 107 |
| A message status is spelled `canceled`. Spec amended. | Ruling 97 |
| The router call is `claude -p --model haiku` with no `--json-schema`, parsed strictly. Spec amended. | Ruling 99 |
| The user-only commands (`say`, `decide`, `decisions edit`, `install`, `uninstall`, `messages cancel`, `routing reset`, `setup approve`, `pause`, `resume`, `clean`, `review`, `restore`, `serve`, task-writing commands) need a controlling terminal, so over SSH they need a pty. A rule given to `curia say` goes through the same check: `say` as a whole needs a terminal. Spec amended. | Rulings 112, 113, 119, 123; decision 0021 |
| In V1 agents cannot write tasks. Plan 6 lets an agent FILE a task into the backlog through `file_task` (on by default at level `backlog`, `tasks.agent_filed.level`), but it cannot edit, ready or drop one and cannot spawn a task it filed. An agent's spawn takes only a ready task and refuses `--agent`, `--model`, `--team`, `--resume`; an agent's `send` is limited to 5 queued per target and 20 per sender in 10 minutes. | Rulings 113, 185 |
| Priority is P1 to P3, default P2, set with `--priority` on `task add` and `edit`; it is not shown on the task board. | Plan 5 decision 16 |
| Linear import is read-only with a fixed filter (assigned to you, backlog or unstarted, optionally one team, at most 200 in four pages); one failed page imports nothing; a re-import updates title, body and tags only. | decision 0030; Ruling 168 |

## Spawning and worktrees (F6)

| Difference | Source |
| --- | --- |
| Curia runs `git worktree add` and `tmux new-window` itself instead of wrapping Claude Code's `-w --tmux`; branches are `curia/<id>-<slug>` in `.curia-worktrees/` beside the repository. | Plan 3 decision 1; decision 0017 |
| `.curia/setup.sh` runs only if you approved its exact content (SHA-256, 8 characters typed at the terminal). Spec amended. | Rulings 108, 109; decision 0022 |
| `pause` keeps a worktree that holds ignored files and `clean` removes them only under `--force`, after naming them. Spec amended. | Ruling 111 |
| Under a limit warning a new Claude Code agent with no model starts on `usage.cheaper_model` (default `sonnet`) for that spawn only; `--model` wins. | Rulings 169, 185 |
| `--agent codex` starts Codex; a stored model is used only with the agent it was stored for. | Ruling 169; decision 0031 |

## Reviews, phone and restore (F10 to F12)

| Difference | Source |
| --- | --- |
| Approval runs git itself (`merge --no-ff` into your checkout, or push and `gh pr create`) and is bound to the full commit id you were shown; a prefix is refused. | Rulings 125, 126; decision 0023 |
| Curia runs `git reset --merge` or `git merge --abort` in your checkout only to undo its own interrupted merge, and only when it can prove the checkout holds nothing else. (Decision 0023's "never `git reset`" is older.) | Ruling 128 and later; `cmd/curia/review_git.go` |
| The spec's per-device token became per-device topics plus a one-time token per push, bound to one ticket and one device, stored only as a SHA-256. | decision 0024 |
| A push has at most three buttons; an irreversible ticket gets none; a review gets none. | decision 0024 |
| `curia serve` listens on 127.0.0.1:8787 and takes another address only when you name one that is loopback or Tailscale; nothing is detected; you put `tailscale serve` in front. (The spec says the Tailscale interface.) | Ruling 127; `curia serve --help` |
| `curia restore` resumes by `claude --resume`; it skips sessions whose pane was closed while tmux kept running unless `--include-closed`, and Codex sessions. Pane layouts are not restored. | Rulings 124, 130, 170; decision 0026 |
| Approving a review from the phone is not built. | Plan 4 decision 18 |

## Cost, forecast and integrations (F13 to F17)

| Difference | Source |
| --- | --- |
| The status line carries the prompt cache's expiry and rebuild size, so cache state is real in daily use without cost-lens; the spec and the plan said it would be blank. The rebuild is shown in tokens, not dollars. | Rulings 162, 163 |
| The cost-lens push does not exist yet (another repository); `docs/ingest-schema.json` is the contract. `curia ingest` is allowed inside an agent session and trusts nothing it reads. | decision 0027; Ruling 164 |
| Attribution is in dollars per hour, not tokens; no API-mode figure in the badge, the projected spend appears in `curia usage` and the dashboard. | decisions 0027, 0028 |
| Pausing on a forecast takes two keys, `P` then `y`, not one; it never happens by itself. | decision 0028 |
| `curia install` adds the status line only when you have none; with your own it prints a command that chains yours to `curia ingest --statusline`. `--no-statusline` skips the step. | Ruling 167 |
| Branch cost in a review comes from running cost-lens's analyzer on the task's worktree path, in the background; `currentProject` is the only part kept. It needs `node` and the analyzer (`[costlens] analyzer`, or the one beside the `cost-lens` command). | Ruling 165 |
| Obsidian: every decided decision is written as `<id> <title>.md` into an existing folder with all text inside code fences; a note without Curia's marker is never overwritten; nothing is deleted or read back. | decision 0029; Ruling 175 |
| Agent Commons: the entry for a global decision is printed, not sent. | decision 0029 |
| Codex support is partial: sessions are tracked through `curia hook <Event> --agent codex` and can be spawned; there is no `curia install --codex`, no Curia MCP tools or hooks for Codex, no typing into a Codex pane, no delivery at Stop, no restore. | decision 0031; Rulings 166, 170 |
| Not built: the operator skill, embeddings for precedent, cost-lens's model advisor, token-delta attribution, a one-key pause. | plan section 14; decision 0028 |

## Where the sources disagreed, and what the code says

1. Probes N1 and N2 (ntfy), S1, C2 and X2, R1 to R3: the older records said "not run", the ledgers said run. They ran, locally; fixtures are committed (`internal/notify/testdata/ntfy`, `internal/usage/testdata/statusline-captured*.json`). X1, L1, O1 and A1 have not run.
2. Cache state without cost-lens: the status line carries it (`internal/usage/ingest.go`, Ruling 162).
3. Interrupted merge: the code runs `git merge --abort` or `git reset --merge` in one proven case (`cmd/curia/review_git.go`).
4. Where `curia serve` binds: loopback by default, Tailscale only if named, nothing detected (`cmd/curia/serve.go`).
5. `--head`: a full 40 or 64 digit commit id; a prefix is refused (`reviewHeadPattern`).
6. The MCP server has six tools, not five (Plan 6 made it seven: `file_task`).
7. SessionStart is `idle` (`internal/state/state.go`).
8. A rule given to `curia say`: `say` is user-only and needs a controlling terminal for every message (`userOnly` runs first); without one, nothing is sent.
