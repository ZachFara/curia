# Changelog

## Unreleased: Plan 6

Not verified against real Claude Code sessions yet; see `README.md` "Status" and `PROGRESS.md` "Plan 6". By capability:

### What needs you, and what happened
- The dashboard popup opens on an inbox (blocking decisions, permission prompts, reviews, assumptions to confirm); `Tab` goes to the session list. `curia inbox [--json]` prints the same.
- `i` (or `curia show <session|T-id|%pane>`) shows what one agent is doing: transcript tail, tickets, messages, diff, cost, activity.
- `T` and `curia timeline` list what happened across agents; `L` and `curia decisions log` search decisions with how often each was reused.
- `curia time` shows where agent time goes and how much is waiting on you.

### Settings
- `curia config list|get|set|edit|doctor|history|rollback` and the dashboard's `S` view; every change is a revision. A repository's `.curia/config.toml` can only tighten. `C!` in the badge marks an edit made outside Curia.

### Resources and Docker
- `curia monitor` samples CPU and memory per agent (`curia resources`, a dashboard column).
- Agents Curia launches get a `docker` wrapper that labels containers and refuses `run`, `create` and `compose up` at 3 containers per session, 8 in all or 80% of Docker's memory; each cap is switched off by 0.

### Permissions, modes and budgets
- `curia mode normal|edits|away|bypass`, `curia away` and `curia back`. Bypass means "allow what your Claude Code settings do not forbid".
- A per-task budget (`--budget`, marks in views) and an optional hard stop (`cost.budget_hard_stop`, off).
- Curia's hook and `curia install` now protect Curia's own configuration, data and state from Claude Code's edit tools.

### Tasks, messages and automation
- Agents can file tasks into your backlog with the `file_task` tool (a seventh MCP tool); only you start them.
- Broadcasts (`@all`, `@<repo>/*`), scheduled messages (`curia schedule`), auto-spawn of ready tasks (`curia auto-spawn`, off) and Remote Control (`curia rc`, never run against a real session).

### Phone
- An optional web page on `curia serve` (off): board, timeline, decision log, usage, settings; device tokens; with `serve.web_write`, three task moves and confirmed messages.

### Upgrade notes
- Run `curia install` again: it adds the seventh tool rule and `Edit(...)` deny rules for Curia's directories (the default ones, the `XDG_*_HOME` ones and a relocated database or log), and the tmux snippet gains a `client-attached` line that starts `curia monitor`; then `tmux source-file ~/.tmux.conf`. Restart Claude Code sessions.
- The database migrates itself. The monitor starts when you attach tmux (`monitor.autostart = false` stops that).
- Agents Curia launches have the Docker wrapper first on `PATH`; set the three `resources` caps to 0 to turn the caps off (the labels stay).
- A budget-stopped task can no longer file tasks; away agents still can.
- Agents may now file tasks into the backlog (`tasks.agent_filed.level = off` stops that).
- The popup now opens on the inbox.
- `curia mcp --help` still says six tools; there are seven.

## 1.0.0

The first release. Curia is one Go binary that turns tmux into a control plane for parallel coding agents. `README.md` says how to install it and what each part does; `docs/v1-as-built.md` lists where it differs from the spec. What you can do, by area:

### Sessions and attention

- See every Claude Code session in one place: `curia ls`, the dashboard popup (`curia ui`), a narrow always-on `curia sidebar`, and a status-bar badge `D<n> Q<n> W<n> R<n>`.
- Get told when an agent needs you: a red dot on the window, a blue one for a finished task, a bell, an optional macOS notification, and a key that jumps to the session that needs you most (`curia next`).
- Agent Teams show as one row that expands.

### Decisions and precedent

- Agents raise decision tickets with options and a recommendation through seven MCP tools; they can search earlier answers first. You answer in a popup or with `curia decide`; the answer reaches the agent by typing, at its next Stop, or when it asks.
- Every decision is stored with its reasoning, searchable, and can be rescoped or superseded (`curia decisions ls`, `edit`). Standing rules are decisions with no options.
- Decided decisions can be copied to an Obsidian folder (`curia export obsidian`) and a global one printed as an Agent Commons entry (`curia export commons`).

### Tasks, spawning, teams and messages

- Keep a backlog (`curia task`), with a priority P1 to P3, and import it from Backlog.md or from your Linear issues (read-only, `CURIA_LINEAR_API_KEY`).
- `curia spawn` starts Claude Code, or Codex with `--agent codex`, on a task in its own git worktree and tmux window; `pause`, `resume` and `clean` finish the job. An approved `.curia/setup.sh` can prepare each worktree.
- `curia say` sends a message to an agent, a team lead, a task or a standing rule, routed by an address or one bounded model call.

### Reviews, pushes, the phone endpoint and restore

- A finished task lands in a review queue (`curia review`): diff, decisions, unconfirmed assumptions, the agent's own test claim, cost. Approve (merge or pull request), ask for changes, or drop.
- Phone pushes through ntfy for blocking decisions and reviews; `curia serve` lets a button answer a reversible ticket once.
- `curia restore` reopens the sessions a reboot or a tmux crash closed.

### Cost, forecast and integrations

- `curia install` sets Claude Code's status line (when you have none) so Curia stores each session's cost, context, cache and usage windows; `curia ingest` takes the same numbers from another program such as cost-lens.
- From those readings the badge warns when a usage limit will land before it resets; `curia usage` and the dashboard (COST and CACHE columns, `b` sorts by burn) show who burns it; the dashboard suggests pausing the lowest-priority running task (`P`, then `y`; never automatic); `curia spawn` starts a new Claude Code agent on `sonnet` under a warning.
- The phone push for a blocking ticket says how long the agent's prompt cache stays warm.
- A review shows the task branch's cost when cost-lens is installed.
- Codex sessions are tracked and can be spawned by `curia spawn --agent codex`; support is partial (see below).

## Upgrading from 0.x

- Run `curia install` again. It adds the seven Curia tool rules (the sixth is `complete_task`, the seventh `file_task`), the status line when you have none (`--no-statusline` skips it) and the window marker's new value, and rewrites entries that name an old binary path. Then reload tmux (`tmux source-file ~/.tmux.conf`) so the new snippet is read.
- A `curia mcp` started before the upgrade keeps its old tool list and keeps returning a superseded answer as live until its Claude Code session restarts. Restart your sessions.
- The database migrates itself the first time a new `curia` opens it; nothing to run.

## Known limits

- Codex: sessions are tracked and spawned by `curia spawn --agent codex`, but a Codex session has no Curia MCP tools or hooks (`curia install --codex` is not built; it waits for the owner's probe of Codex's hooks), Curia never types into a Codex pane, and `curia restore` skips Codex sessions and prints the command to run by hand.
- No cost-lens push exists yet: dollar costs and rebuild dollars show only when cost-lens pushes; otherwise the status line gives tokens and the usage windows.
- Agent Commons entries are printed, not sent. Linear is read-only. Obsidian export is one way.
- Priority is not shown on the task board.
- A permission prompt shows up to 6 seconds late.
- Typing into panes depends on recognising Claude Code's screen; an update to Claude Code can turn it off.
- A pull request's later merge is not tracked; pane layouts are not restored; a review cannot be approved from the phone.
- Not verified by a live run: the cost, forecast and integration features with real sessions; Codex hooks, the real Linear API, Obsidian rendering, a real usage limit, the phone button and Remote Control are the owner's open probes.
