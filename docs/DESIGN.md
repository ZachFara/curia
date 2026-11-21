# Curia: V1 Feature Spec

Nov 11, 2026 · @Zachariah Farahany

## TLDR

V1 is a single binary, `curia`, that turns tmux into a control plane for parallel coding agents. Claude Code hooks write every session event into one local SQLite store. Everything else is a view or an action over that store: a session dashboard, attention signals, a task board, worktree-isolated agent spawning, a review queue, and the core differentiator, a decision queue with a searchable precedent log that agents consult before asking again. Session tracking, worktrees and approval queues already exist in open source, so V1 borrows their patterns (and code, where the license allows) and spends original effort on decisions, precedent and the task-to-merge lifecycle. Coordination inside a single job is left to Claude Code's Agent Teams; Curia treats each team as one unit to watch, route and cost. After V1, eleven further features (F19 to F29) add resource limits, permission modes and an away routine, Remote Control, scheduled messages, an inbox, an agent detail view, a timeline and decision log, a local web view, activity-time measurement, one layered settings file and an opt-in auto-spawn of ready tasks; they are specified here and built later (section 8), around one new background process, `curia monitor` (section 4).

## Contents

1. Problem
2. Goals and non-goals
3. Build vs borrow
4. Architecture
5. Data model
6. Feature specs (F1 to F29)
7. Safety and trust boundaries
8. Build order (V1, then F19 to F29)
9. Open questions

## 1. Problem

With several agents running, compute is no longer the constraint; my attention is. One agent is a conversation. Five agents are a queue of interruptions, and tmux gives that queue no shape.

Start from what an agent actually needs from me. It needs only three things: a task to start, a judgment call when it hits ambiguity, and a review when it is done. Everything else it can do alone. So the cost of running N agents is roughly N times (tasks + decisions + reviews), plus the overhead of finding which agent needs which of those right now.

Today each of those costs is inflated:

- **Finding** who needs me means visiting panes one by one.
- **Decisions** arrive as free text buried in scrollback, usually without a recommendation, so I do the agent's analysis for it.
- **Repeat decisions** happen because agents have no memory of what I already decided in other sessions or last week.
- **Reviews** have no queue, so finished work sits unnoticed while I watch working agents.
- **Tasks** live in my head, so starting an agent means re-explaining context and hand-creating a branch.

It follows that the tool should do four things: make "who needs me" ambient, make each interruption as cheap as possible to resolve, make resolved judgments reusable, and make the task-to-merge path mechanical. The features in section 6 map onto exactly those four.

## 2. Goals and non-goals

**Goals for V1**

- Any agent needing me is visible within 2 seconds, from any tmux window, without visiting panes.
- A decision can be answered in under 30 seconds from a tmux popup, or from a phone over SSH.
- Every decision is stored with its reasoning and is searchable by agents before they raise a new one.
- A task goes from backlog to running agent in one command, with an isolated worktree and branch.
- Finished work lands in a review queue with diff, decisions, open assumptions and cost in one view.
- Works headless, over SSH, and survives laptop sleep or disconnect (the core advantage of tmux over GUI tools).
- Agent-agnostic where hooks allow: Claude Code first, Codex second.

**Non-goals for V1**

- A terminal emulator or GUI app. tmux is the UI.
- A built-in browser surface (cmux has one; agents can use Playwright MCP instead).
- Multi-user or team sharing. One person, one machine or one remote box.
- A custom orchestrator or executor agent. Coordination inside a job belongs to Claude Code's Agent Teams, which Curia shows and routes as first-class units (F14); a natural-language assistant across the portfolio is a skill over Curia's CLI and MCP tools, not new code.
- Replacing Linear. Linear stays the source of truth for work tracking; `curia` only reads it.

## 3. Build vs borrow

Yes, V1 can build on other people's work, and should. There are three ways to do it, and the license only matters for one of them.

1. **Reimplement the pattern.** Ideas, architectures and UX patterns are not copyrightable. "Hooks write per-pane state, tmux reads it" is free for anyone to use. Most of what we borrow is this kind.
2. **Run their tool as a separate program.** Calling another tool's CLI or reading its output generally does not make `curia` a derivative work, whatever its license.
3. **Copy or adapt their code.** Here the license governs. MIT: copy freely, keep their copyright notice. AGPL-3.0: the combined work must also be released under AGPL-3.0 with source available. Fine if `curia` is open source under AGPL; a problem if it ever needs a permissive or closed license.

I'm not a lawyer, and licenses below should be confirmed in each repo's LICENSE file before copying any code.

| Project | License | What we take | How |
| --- | --- | --- | --- |
| [Claude Squad](https://github.com/smtg-ai/claude-squad) | AGPL-3.0 | Worktree-per-task lifecycle, pause/resume, diff tab, push via `gh` | Pattern (reimplement) |
| [claude-tmux-sessions](https://github.com/threedaycat/claude-tmux-sessions) | MIT | Per-pane state file keyed by `$TMUX_PANE`, mark-seen on pane focus, fzf picker | Pattern; code adaptable with notice |
| [claudecode-tmux](https://github.com/ngocbh/claudecode-tmux) | Check repo | Hook-to-state mapping (running / asking / idle), clickable status chips, SSH socket forwarding | Pattern |
| [claude-tmux-sidebar](https://github.com/torikatsupg/claude-tmux-sidebar) | Check repo | Persistent sidebar layout, plugin-based hook install | Pattern |
| [ccmonitor](https://pkg.go.dev/github.com/martinwickman/ccmonitor) | Check repo | Go hook binary with no shell wrapper, stale-session cleanup | Pattern; lessons on hook flakiness |
| [cmux](https://github.com/manaflow-ai/cmux) | AGPL-3.0 per YC launch (a mirror lists MIT) | Attention rings, sidebar row content (branch, dir, ports, last line), CLI that agents can drive | Pattern |
| [mcp-approval-gate](https://github.com/kaitbellahs/mcp-approval-gate) | Check repo | Approval happens off the MCP surface; arguments hashed at request time; duplicate requests collapse | Pattern |
| [triage-mcp](https://pypi.org/project/triage-mcp/) | Check repo | Read-only MCP over a decision log (`search_decisions`) | Pattern |
| [Paperclip](https://github.com/paperclipai/paperclip) (amended, Ruling 144) | MIT (LICENSE file read 2026-11-19) | Hard budget stops (F16); routines with run history (F22) | Pattern |

**Paperclip (amended, Ruling 144).** Paperclip is the nearest neighbor in intent and the furthest in method. It runs agents as an autonomous organization: a server with roles, budgets and scheduled routines, where work proceeds without a person in the loop. Curia is for interactive sessions in tmux with a human in the room, where the scarce resource is my attention and each decision I make compounds into precedent. Curia does not borrow its organization model. It borrows two patterns: hard budget stops (F16) and routines with run history (F22). Its LICENSE file is MIT (read 2026-11-19); its features beyond those two were not verified for this spec (section 9).

The parts nobody has built, and where original effort goes: structured decision tickets with recommendations across many concurrent agents, the precedent loop, the assumption-override flow, and one store tying tasks, sessions, decisions and reviews together.

### Other inspiration

These overlap with parts of V1 and are worth reading before building the matching feature. None of them has a decision queue with precedent or cache-aware triage.

| Project | What it does | What to take | Informs |
| --- | --- | --- | --- |
| [docket](https://github.com/parzival1l/docket) | Task CLI and TUI; `docket start T-N` assembles a task into an agent brief and can open it in a fresh tmux session | Task-to-brief handoff, backlog states, piping a brief into `claude` | F5, F6 |
| [Switchboard (ritualapps)](https://github.com/ritualapps/switchboard) | Terminal dashboard for many Claude Code sessions: needs you, ready for review, running | "Ready for review" as a first-class state; agents declaring status through files | F3, F10 |
| [Switchboard (tdody)](https://github.com/tdody/switchboard/releases/tag/v0.1.0) | Browser kanban over tmux sessions, with interactive prompts rendered as click-to-answer overlays | Answering agent prompts without visiting the pane | F3, F7 |
| [Agent Roost](https://github.com/takezoh/agent-roost) | tmux-native control surface for Claude, Codex and Gemini across projects, with hook-driven status glyphs | Fixed 3-pane layout; per-agent `setup` command for hooks | F1, F3 |
| [ccmux](https://github.com/epilande/ccmux) | Discovers agents already running in tmux panes and jumps to the one that needs you; background agents and subagents get their own rows | Zero-config discovery of existing panes; subagent rows | F1, F2, F3 |
| [agent-dashboard](https://github.com/bjornjee/agent-dashboard) | tmux dashboard that dispatches your input to whichever agent needs it and gates sessions through hook-enforced workflow skills | Hook-enforced workflow gates (tests before merge) | F4, F10 |
| [tmux-ide](https://tmux-ide.com/docs/multi-agent-teams) | tmux layouts for agent work; shows Claude Code Agent Teams teammates by name, grouped under their team, with live status | Team grouping in the session list | F14 |

## 4. Architecture

`curia` is one binary with subcommands, around one SQLite store. There is no long-running daemon in the core path: hooks write, UIs read on demand, and only the optional `curia serve` endpoint stays up. (Amended, Ruling 145: F19 to F29 add one background process, `curia monitor`. V1 does not need it.)

&#91;embedded content: Curia architecture · 9 components around one store\]

The diagram above shows the V1 core. `curia monitor` (below) is a tenth component around the same store, added by F19 to F29.

Agents never talk to me directly. Hooks and MCP calls write into the store; the tmux UI and remote channels read from it; my answers go back into the store, and the delivery step carries them to the right pane.

| Subcommand | Role | Runs |
| --- | --- | --- |
| `curia hook <event>` | Hook adapter (F1, F2) | Per Claude Code hook event, under 20 ms |
| `curia mcp` | MCP server for decisions and task completion (F7, F9, F10) | Per agent session, started by Claude Code |
| `curia status` | Status-bar badge (F4) | Every tmux status-interval (2 s) |
| `curia ui`, `curia decide`, `curia review` | Popups (F3, F7, F10) | On keypress, inside `display-popup` |
| `curia task`, `curia spawn`, `curia pause`, `curia resume`, `curia clean` | Tasks and worktrees (F5, F6) | On demand |
| `curia serve` | Phone answers via ntfy actions (F11) | Optional, long-running |
| `curia restore`, `curia export` | Restore and integrations (F12, F13) | On demand |
| `curia install`, `curia uninstall` | Hook, MCP and skill registration | Once |
| `curia monitor` (amended, Ruling 145) | Sampler and scheduler: resources (F19), schedules (F22), background commands (F27), pending `/rc` (F21), mode and away expiry notices (F20), auto-spawn of ready tasks (F29) | Long-running, one instance |
| `curia mode`, `curia away`, `curia back`, `curia rc`, `curia schedule`, `curia inbox`, `curia show`, `curia timeline`, `curia time`, `curia resources`, `curia config`, `curia auto-spawn` | Features F19 to F29 | On demand |

**`curia monitor`.** One instance, held by a lock file at `~/.local/state/curia/monitor.lock`. It is started by `curia monitor start`, and by the tmux snippet that `curia install` already adds (additively, Ruling 95), which runs `curia monitor start --quiet` when a client attaches; `monitor.autostart = false` removes that line's effect. `curia monitor status` and `stop` report and end it. It writes a heartbeat row so other commands can say `monitor not running`.

When it is not running, nothing in V1 changes, and no safety rule depends on it: permission modes and their expiry are decided by the hook at call time (F20), the docker wrapper counts containers itself (F19), and every delivery rule is unchanged. What stops is the work that needs a clock: resource samples (the dashboard says so), schedules do not fire (missed triggers follow F22's 30-minute rule on the next start), background commands are not tracked to completion (F27), `/rc` for a busy session stays pending (F21), and nothing is auto-spawned (F29): ready tasks wait.

## 5. Data model

One SQLite database at `~/.local/share/curia/curia.db`, WAL mode so hooks can write while the dashboard reads. Five core tables; everything in section 6 reads or writes these.

```sql
-- A running or past agent session, tied to a tmux pane
CREATE TABLE sessions (
  id            TEXT PRIMARY KEY,   -- agent's own session id (Claude Code session_id)
  agent         TEXT NOT NULL,      -- claude | codex
  tmux_pane     TEXT,               -- e.g. %12, from $TMUX_PANE
  tmux_session  TEXT, tmux_window TEXT,
  cwd           TEXT, repo TEXT, branch TEXT, worktree TEXT,
  task_id       TEXT REFERENCES tasks(id),
  state         TEXT NOT NULL,      -- working | waiting | needs_decision | idle | ended | stale
  model         TEXT,
  context_used  INTEGER, context_max INTEGER,
  cost_usd      REAL,
  subagents_active INTEGER DEFAULT 0,
  last_update   TEXT,               -- one-line summary of latest activity
  seen_at       TEXT,               -- last time I focused this pane
  transcript_path TEXT,
  started_at TEXT, updated_at TEXT
);

-- Append-only raw hook events (debugging, replay, metrics)
CREATE TABLE events (
  id INTEGER PRIMARY KEY, session_id TEXT, kind TEXT, payload JSON, at TEXT
);

CREATE TABLE tasks (
  id TEXT PRIMARY KEY,              -- T-42
  title TEXT, body TEXT,            -- body = the agent's starting brief
  repo TEXT, base_branch TEXT,
  status TEXT NOT NULL,             -- backlog | ready | running | needs_decision | review | done | dropped
  source TEXT, source_ref TEXT,     -- local | linear (read-only) | backlog_md
  tags TEXT, created_at TEXT, updated_at TEXT
);

CREATE TABLE decisions (
  id TEXT PRIMARY KEY,              -- D-117
  session_id TEXT, task_id TEXT, repo TEXT,
  question TEXT NOT NULL,
  context TEXT,                     -- what the agent knows, links to files/diffs
  options JSON NOT NULL,            -- [{key, label, tradeoffs}]
  recommendation TEXT NOT NULL,     -- option key
  rationale TEXT NOT NULL,
  reversibility TEXT NOT NULL,      -- cheap | costly | irreversible
  mode TEXT NOT NULL,               -- blocking | assume
  assumption TEXT,                  -- for mode=assume: what the agent proceeds with
  scope TEXT NOT NULL,              -- task | repo | global (how far precedent applies)
  tags TEXT,
  status TEXT NOT NULL,             -- open | answered | confirmed | overridden | withdrawn | expired
  answer TEXT, answer_reasoning TEXT, answered_via TEXT,  -- popup | ssh | ntfy
  precedent_of TEXT,                -- decision id this one reused, if any
  args_hash TEXT,                   -- dedupe identical retries
  created_at TEXT, answered_at TEXT
);
CREATE VIRTUAL TABLE decisions_fts USING fts5(question, context, rationale, answer, answer_reasoning, tags);

CREATE TABLE reviews (
  id TEXT PRIMARY KEY, task_id TEXT, session_id TEXT,
  summary TEXT,                     -- agent's own completion summary
  diff_stat TEXT, tests_status TEXT,
  status TEXT NOT NULL,             -- pending | approved | changes_requested | merged
  my_notes TEXT, created_at TEXT, closed_at TEXT
);
```

**Additions for F5, F16 and F19 to F29.** Columns and tables added to the above, only as far as the spec needs.

```sql
ALTER TABLE tasks ADD COLUMN filed_by TEXT;        -- F5: filing session id; NULL for tasks I wrote
ALTER TABLE tasks ADD COLUMN filed_reason TEXT;    -- F5: the agent's reason, required
ALTER TABLE tasks ADD COLUMN filer_lineage JSON;   -- F5: filer, its team id, lead and teammates at filing time
ALTER TABLE tasks ADD COLUMN budget_usd REAL;      -- F16: NULL = no budget
ALTER TABLE tasks ADD COLUMN ready_hash TEXT;      -- F29: hash of title and body when it became ready by me at a terminal or by my level = ready; NULL otherwise (web move), cleared on leaving ready
ALTER TABLE tasks ADD COLUMN auto_spawned_at TEXT; -- F29: set when the monitor started it; NULL for a spawn I ran
ALTER TABLE sessions ADD COLUMN rc TEXT;           -- F21: off | pending | on | unknown
ALTER TABLE sessions ADD COLUMN rc_checked_at TEXT;
ALTER TABLE decisions ADD COLUMN coerced_from TEXT; -- F20: 'blocking' when raised while away
ALTER TABLE decisions ADD COLUMN near_match TEXT;   -- F25: decision id returned as a near-duplicate at raise time
ALTER TABLE messages ADD COLUMN broadcast_id TEXT;  -- F18: one id for the rows of one broadcast
ALTER TABLE messages ADD COLUMN via TEXT;           -- cli | popup | web | schedule

CREATE TABLE modes (            -- F20: current and past
  id INTEGER PRIMARY KEY, scope TEXT NOT NULL,      -- 'all' or a repo path
  mode TEXT NOT NULL,           -- normal | edits | away | bypass
  set_at TEXT, expires_at TEXT, ended_at TEXT, native INTEGER DEFAULT 0, away_run INTEGER
);
CREATE TABLE permission_events (  -- F20: what the hook allowed or denied under a mode
  id INTEGER PRIMARY KEY, session_id TEXT, at TEXT, mode TEXT,
  tool TEXT, summary TEXT, verdict TEXT             -- allow | deny
);
CREATE TABLE away_runs (
  id INTEGER PRIMARY KEY, started_at TEXT, ready_at TEXT, ends_at TEXT, ended_at TEXT,
  saved_modes JSON, not_ready JSON, summary JSON
);
CREATE TABLE resource_samples (   -- F19: session_id NULL = host or Docker VM
  at TEXT, session_id TEXT, cpu_pct REAL, rss_mb INTEGER, containers INTEGER,
  docker_mem_mb INTEGER, docker_mem_limit_mb INTEGER
);
CREATE TABLE containers (
  id TEXT PRIMARY KEY, session_id TEXT, name TEXT,
  attributed_by TEXT,           -- label | compose | none
  mem_mb INTEGER, started_at TEXT, seen_at TEXT
);
CREATE TABLE schedules (          -- F22
  id TEXT PRIMARY KEY,          -- S-3
  text TEXT NOT NULL, kind TEXT NOT NULL,           -- at | daily | on_reset
  spec TEXT NOT NULL, target TEXT NOT NULL,         -- 'all' | 'repo:<name>' | 'task:T-42'
  if_idle INTEGER, only_limited INTEGER,
  status TEXT NOT NULL,         -- active | paused | done | canceled
  next_run_at TEXT, created_via TEXT, created_at TEXT
);
CREATE TABLE schedule_runs (
  id INTEGER PRIMARY KEY, schedule_id TEXT, due_at TEXT, ran_at TEXT,
  outcome TEXT,                 -- fired | skipped_late | skipped_no_target | monitor_down; F29: schedule_id 'auto', fired | refused
  results JSON                  -- per session: delivered | skipped_busy | skipped_not_limited
);
CREATE TABLE activity (           -- F27
  session_id TEXT, task_id TEXT, class TEXT, start TEXT, end TEXT,
  rule TEXT, unfinished INTEGER DEFAULT 0
);
CREATE TABLE background_commands (
  id INTEGER PRIMARY KEY, session_id TEXT, command TEXT, class TEXT,
  pid INTEGER, started_at TEXT, ended_at TEXT
);
CREATE TABLE web_devices (        -- F26
  name TEXT PRIMARY KEY, token_sha256 TEXT NOT NULL, created_at TEXT, last_seen TEXT, revoked_at TEXT
);
CREATE TABLE config_revisions (   -- F28
  n INTEGER PRIMARY KEY, at TEXT, actor TEXT,       -- cli | dashboard | rollback | external
  key TEXT, old TEXT, new TEXT, file TEXT           -- full user file after the change
);
```

An agent-filed task is a `tasks` row with `filed_by` set; the per-agent daily cap counts such rows by `filed_by` and `created_at`. F29's daily cap counts `auto_spawned_at` over the last 24 hours, and its log is `schedule_runs` rows with `schedule_id = 'auto'`, which is not a `schedules` row.

The `sessions.state` value `needs_decision` is distinct from `waiting` on purpose: `waiting` means Claude Code's own permission or input prompt, `needs_decision` means an open ticket in the queue. The dashboard ranks them differently (section 6, F4).

## 6. Feature specs

Twenty-nine features. F1 to F18 are V1, grouped by the four jobs from section 1 plus a cost layer: see who needs me (F1 to F4), start work (F5, F6), resolve judgments (F7 to F9), finish and sustain work (F10 to F14), and spend wisely (F15 to F17). F18 routes my own messages down to the right agent. F19 to F29 are additions made on 2026-11-19 (Rulings 140 to 161), built after V1 (section 8): resources and limits (F19), permission modes and away (F20), Remote Control (F21), scheduled messages (F22), an inbox (F23), agent detail (F24), a timeline and decision log (F25), a web view (F26), activity time (F27), settings (F28) and auto-spawn (F29).

### F1. Session tracking

**What:** every agent session in tmux is known to `curia`, with a live state.

**How:** `curia install` registers hooks in `~/.claude/settings.json` (merged with existing hooks, backed up first; pattern from claude-tmux-sessions). Each hook runs `curia hook <event>`, which reads the hook's stdin JSON, reads `$TMUX_PANE` from the environment, and upserts the session row.

| Hook event | Resulting state | Also records |
| --- | --- | --- |
| SessionStart | idle, and seen (amended, Ruling 37) | pane, cwd, repo, branch, transcript path |
| UserPromptSubmit | working | clears unseen flag |
| PreToolUse / PostToolUse | working | last\_update ("Edit src/api.ts"), subagent count when the tool launches a subagent |
| Notification | waiting | the prompt text, so the dashboard can show what it wants |
| Stop | idle (or needs\_decision if a ticket is open) | last assistant line as last\_update |
| SubagentStop | unchanged | decrements subagents\_active |
| SessionEnd | ended |  |

**Stale detection:** every dashboard refresh checks `tmux list-panes -a`; a session whose pane no longer exists becomes `stale` and is hidden after 10 minutes. This addresses the duplicate and ghost sessions ccmonitor reports from hook gaps.

**Acceptance:** hook execution adds under 20 ms per event; a session started in any pane appears in the store within 1 second.

### F2. Session metrics

**What:** model, context used, cost and subagents per session.

**How:** model and token usage come from the transcript (JSONL at `transcript_path`). Context used = the latest assistant message's input tokens including cache reads and writes. Context max comes from a per-model table in config, so new models need one config line. (Amended, Ruling 142, revised 2026-11-19: the context window and the used percentage come from Claude Code's status-line payload for that session when Curia has it, the same payload F15 and F16 ingest through `curia statusline`: the window from `context_window.context_window_size`, and the percentage is Claude Code's own `used_percentage`, clamped to 0..100. It is null before the first response, and the rate-limit and cache fields appear only after it (observed on 2.1.329, section 9); a null counts as no status-line context data yet, never as 0. No table entry is needed for a new model. The `context_max` table and `default_context_max` remain only as the fallback for a session with no status-line data yet, because the status line is not installed or its first refresh has not come, and there the figure is computed from the transcript as before. Once a session has status-line context data, the status line is the only writer of its context fields.) Cost comes from cost-lens when installed (F15), otherwise from the cost field in Claude Code's status-line JSON. Curia keeps no price table of its own. Parsing is incremental: `curia` stores a byte offset per transcript and reads only new lines.

**Display:** context as a percentage with color thresholds (green under 60%, yellow 60 to 85%, red above 85%, since compaction quality drops near the limit). (Amended, Ruling 142, revised 2026-11-19: in the fallback, a value over 100% means the fallback window is wrong, not that the context is over full, so the dashboard, the status line and the web page show `?` instead of the number, uncolored, and `curia config doctor` names the model. A number above 100 is never shown anywhere. The same applies to the F3 context column.)

**Acceptance:** a session whose model is missing from the table shows the correct percentage after one status-line refresh; nothing renders above 100.

**Risks:** the payload's fields are Claude Code's and a release could rename or drop them; Curia then falls back and shows `?` rather than a wrong number. The payload is untrusted data and is range-checked (window positive, percentage clamped to 0..100). The percentage is null before the first response, and the fallback applies until then.

### F3. Dashboard

**What:** a popup listing every session, opened with `prefix + a` (configurable) via `tmux display-popup`, built as a TUI (Bubble Tea).

**Columns:** state icon, session:window, task id and title, repo@branch, model, context % (`?` while the percentage is null, F2), subagents, open decisions count, cost, last update, age of last update.

**Default view (amended, Ruling 154):** the popup opens on the inbox (F23); `Tab` switches to this session list.

**Sort order:** needs\_decision, then waiting, then review-ready, then working, then idle. Within a group, oldest first.

**Keys and mouse:** Enter or click jumps (`switch-client -t <pane>`), `d` opens that session's decisions, `r` opens its review, `t` toggles a task-board view (F5), `n` spawns a new agent (F6), `x` kills with confirmation, `/` filters. Mouse clicks work when tmux `mouse on` is set.

**Second surface:** an optional always-on sidebar pane (`curia sidebar`), same data, narrower columns, for a large monitor.

### F4. Attention signals

**What:** I know something needs me without opening anything.

- **Status bar badge:** `#(curia status)` in status-right shows counts, e.g. `D2 Q1 W1 R1` (decisions, answers not yet received, waiting, reviews). Hidden when all zero. (amended, Ruling 91: the format is `D<n> Q<n> W<n> R<n>`, Q = answers a live session has not yet received)
- **Window flags:** window names get a colored marker: red for needs\_decision or waiting, blue for review-ready, none otherwise. Cleared when I focus the pane (`pane-focus-in` hook marks it seen).
- **Jump to next:** `prefix + N` jumps to the highest-priority pane in the F3 sort order.
- **Optional bell and macOS notification** when a blocking decision opens, rate-limited to one per 30 seconds.

### F5. Task board

**What:** a local backlog where each task is the unit an agent works on.

**Statuses:** backlog, ready, running, needs\_decision, review, done, dropped. Only backlog, ready and dropped are set by me; the rest are derived from session and review state, so the board never goes stale.

**Commands:** `curia task add "title" --repo . [--body file.md]`, `curia task ls`, `curia task edit T-42`, `curia task ready T-42`. The task body is the starting brief given to the agent, so writing a good task is writing a good prompt. (amended, Ruling 107: `curia task edit` opens an editor only from the `editor` config option, an argument vector run without a shell; `$VISUAL` and `$EDITOR` are not read, and flags cover every field. amended, Ruling 113: `task add`, `edit`, `ready` and `import` are user-only; an agent cannot run them.)

**Agent-filed tasks (amended, Ruling 141; amends Ruling 113):** an agent may file a task through a seventh MCP tool, `file_task(title, body, reason)`; `reason` is required. The setting `tasks.agent_filed.level` is `off`, `backlog` (default) or `ready`. At `off` the tool returns an error. At `backlog` the task is created `backlog`; only I mark it ready (`curia task ready`, the board, or the web page with its token). At `ready` it is created `ready`, which is my standing choice and cannot come from a repository file (F28). Every such task records the filer (`filed_by`), the reason and the filer's lineage (`filer_lineage`): as far as it can be observed: the session itself and, from Curia's own spawn records, its team, its lead and its teammates (a subagent acts as the session that launched it). A subagent's tool calls carry an agent id and type and the parent's session id, so a subagent acts as its launcher session for the filing cap and for every hook rule (observed on 2.1.329, 2026-11-20); a teammate's link to its lead is still not observable from payloads (hooks page, read 2026-11-19), so lineage is partly observable. It is kept for display and audit only; no permission rests on it. Each agent may file `tasks.agent_filed.max_per_day` tasks (default 5) per rolling 24 hours; a further call returns an error. An agent's `spawn` is refused on any agent-filed task whatever its status, so an agent cannot file work and run it, whoever it is. No agent spawns an agent-filed task; I spawn one, or auto-spawn (F29), when it is on, starts a ready one, always in a new session and never by assigning it to a running agent (owner decision D-7, 2026-11-20). `task ready` records `ready_hash`, the hash of the title and body at that moment (a task created ready by `level = ready` records it at creation), and for a task from Linear, Backlog.md or an agent it first prints the body and waits for Enter; a ready move from the web page does not record it (F29). `task add`, `edit`, `ready` and `import` stay user-only. This answers Plan 3 Open Question 14.

**Views:** a kanban view inside the F3 popup (`t`), and `curia task ls` for scripting.

**Sources:** local tasks by default. A Linear adapter imports issues as tasks, read-only: `curia` never writes Linear statuses back. A Backlog.md adapter reads markdown tasks from a repo. Adapters only map fields; status derivation is always local.

### F6. Spawning, worktrees and the agent CLI

**Spawn:** `curia spawn T-42 [--agent claude] [--model opus]` does, in order:

1. Create a worktree at `<repo>/../.curia-worktrees/<repo>-T-42` on branch `curia/T-42-<slug>` from the task's base branch.
2. Run the repo's setup script if one exists (`.curia/setup.sh`: install deps, copy `.env`). (amended, Ruling 108: it runs only if the user approved that exact content for that repository with `curia setup approve`; an unapproved or changed script is skipped with a note. amended, Ruling 109: approval shows the whole script, refuses one the display cannot show faithfully, and needs the first 8 hex of its hash typed at the terminal.)
3. Open a tmux window named `T-42` in a session named after the repo.
4. Launch the agent with the task body plus a standard preamble (how to use decision tools, how to signal completion).
5. Link session to task; task becomes `running`.

**Worktrees (amended, Ruling 140; answers Plan 3 Open Question 1):** spawning uses Curia's own worktrees and tmux windows for every agent, not Claude Code's native `claude -w <name> --tmux`. Probe P1 showed the native path puts the worktree under `<repo>/.claude/worktrees/`, names the branch `worktree-<name>`, bases it on the current `HEAD` and opens a tmux session with a launcher that hangs without an attached client; it cannot carry the task's base branch, branch name or setup approval. The sentence that said native first is withdrawn. `--team` adds an Agent Teams lead (F14). Spawned sessions also get the F19 `docker` wrapper on their `PATH`.

**Lifecycle commands:** `curia pause` (commit WIP, remove worktree, keep branch; amended, Ruling 111: the worktree is removed only when it holds no ignored files, otherwise it is kept in place and the task is marked paused), `curia resume` (recreate worktree, `claude --resume <session>`), `curia clean` (remove worktree and branch after merge; amended, Ruling 111: ignored files are removed only under `--force`, after being named, and the task is marked dropped). All three are user-only (amended, Ruling 112). Pattern from Claude Squad.

**Agent-facing CLI:** `curia ls --json`, `curia read <pane> [--lines 50]` (capture-pane), `curia send <pane> "text"`, `curia spawn`. Agents can call these, which is what the operator skill (F14) builds on. `send` only targets panes registered as agent sessions. (amended, Ruling 112: `send` labels a message as from the user only for a person at a terminal, otherwise as from another agent session. amended, Ruling 113: an agent's `send` is refused when the target already holds 5 queued agent messages or the sender made 20 in 10 minutes, and an agent's `spawn` accepts only a task whose stored status is `ready`.)

### F7. Decision queue

**What:** agents raise structured decision tickets; I answer them from a popup, SSH or my phone; the answer gets back to the agent.

**MCP tools** (served by `curia mcp`, stdio, registered once with `claude mcp add`):

| Tool | Purpose | Returns |
| --- | --- | --- |
| `search_decisions(query, repo?, tags?)` | Check precedent before asking (F9) | Matching past decisions with answers and reasoning |
| `request_decision(ticket)` | Raise a ticket | Ticket id, plus any near-duplicate precedent it found |
| `check_decision(id)` | Poll status | open / answered with answer |
| `withdraw_decision(id, reason)` | Agent resolved it itself | ok |

There is deliberately no tool to answer a ticket. Answers only come through `curia decide`, outside the MCP surface, so a prompt injection in a file the agent reads cannot approve its own ticket (principle from mcp-approval-gate).

**Ticket validation:** `request_decision` rejects tickets missing question, at least 2 options, recommendation, rationale, reversibility or mode. Options cap at 4. Rationale caps at about 150 words, so tickets stay answerable in 30 seconds. Identical retries collapse by hashing the arguments.

**Two modes:**

- `blocking`: for irreversible or costly-to-reverse choices (schema migrations, deleting data, public API changes, adding a paid dependency). The agent finishes any safe work, then ends its turn. Session state becomes `needs_decision`.
- `assume`: the default for everything else. The agent states the assumption it will proceed with (normally its recommendation) and keeps working. The ticket waits for confirmation or override (F8).

**Answering:** `curia decide` (or `d` in the dashboard) shows open tickets oldest-blocking first. Each shows question, options with trade-offs, the recommendation highlighted, reversibility, and links. Keys: `1` to `4` pick an option, `Enter` accepts the recommendation, `w` writes a free-text answer, `r` adds reasoning (prompted, optional, but encouraged since reasoning is what makes precedent useful), `s` sets scope (task, repo, global).

**Delivery back to the agent:**

- Agent is idle (blocking ticket, turn ended): `curia` types a short message into the pane with `tmux send-keys`, e.g. `Decision D-117 answered: option B (Postgres). Reasoning: ... Continue.` This starts a new turn.
- Agent is working: the answer is queued and delivered by the Stop hook when the agent next tries to end its turn, using the hook's ability to block stopping and hand Claude a reason. No typing into a busy pane.

**Rejected alternative:** a long-polling `request_decision` that blocks until answered. It is simpler for agents, but MCP tool calls time out, and a held call ties up the session with no way to show progress.

**Agent instructions:** `curia install` adds a skill (or a CLAUDE.md block) telling agents when to raise a ticket, to search precedent first, which mode to use, and the ticket format, with one good and one bad example.

### F8. Assumption confirm and override

**What:** non-blocking tickets don't slow agents down, but I can still correct them.

**Flow:** for an `assume` ticket, my choices are confirm (one key), or override with a different option. On override, `curia` delivers the override as in F7 (send-keys if idle, Stop hook if working) with the instruction to revert work based on the old assumption. The ticket records `overridden`.

**Expiry:** an unanswered `assume` ticket becomes `expired` after a configurable time (default 24 hours) or when its task merges. Expired tickets are not precedent: only decisions I actually made count.

**Review link:** any still-open `assume` tickets on a task are shown at the top of its review (F10), so I can't merge work built on unconfirmed assumptions without seeing them.

### F9. Precedent

**What:** decisions I already made get reused instead of asked again.

**Search:** SQLite FTS5 over question, context, rationale, answer and my reasoning, filtered by scope: a `task` decision only matches within that task, `repo` within the repo, `global` everywhere.

**Two checkpoints:** agents are instructed to call `search_decisions` before raising. Independently, `request_decision` runs its own search and, if it finds a strong match, returns it with the ticket id and asks the agent to either withdraw (citing the precedent) or explain why this case differs. Both are needed because agents skip instructions.

**Reuse tracking:** when an agent cites a precedent, `precedent_of` links the two, so I can see which decisions get reused and which keep being asked (a sign the precedent is written too narrowly).

**Editing:** `curia decisions edit D-117` lets me rescope or supersede a decision. Superseded decisions stay in the log but drop out of search.

### F10. Review queue

**What:** finished work waits in one place with everything needed to judge it.

**Trigger:** the agent calls `complete_task(summary, tests_status)` (MCP), which creates a review and sets the task to `review`.

**Review view** (`r` in the dashboard or `curia review T-42`): agent summary, `git diff --stat` against base, test status, decisions made on this task, open assumptions (F8) at the top in red, session cost and duration.

**Actions:** `o` opens the full diff in my configured tool (lazygit, delta, or nvim), `a` approves (merge to base, or open a PR with `gh pr create` per config; amended, Ruling 125: the approval is bound to the commit the view showed, and a branch that moved since is refused; amended, Ruling 126: the flag form `curia review <id> --approve` needs `--head <full commit id>`; amended, Ruling 131: an existing pull request is adopted only when it is provably this approval's, and Curia never edits one it did not create; amended, Rulings 128 and 136 to 138: a merge that would conflict, would replace an untracked or ignored file, or would start while an earlier approval's merge may still be running is refused before it runs; it uses git's default options and counts as merged only when the base contains the approved commit; Curia undoes only its own interrupted merge, and only when the index provably holds nothing but that merge), `c` requests changes (my note is delivered to the agent as in F7, task returns to `running`), `x` drops the task.

### F11. Notifications and remote access

**What:** I can leave my desk without agents stalling.

- **Push:** ntfy (self-hosted or ntfy.sh with a private topic) on new blocking tickets and new reviews. The message includes ticket id, question, options and recommendation. Assume-mode tickets don't push, by default.
- **Answer from phone, minimum path:** SSH (over Tailscale) from a phone terminal app and run `curia decide`. The popup works at phone width.
- **Answer from phone, convenient path:** ntfy action buttons hit `curia serve` (a small HTTP endpoint bound to the Tailscale interface only; amended, Ruling 127: it listens on 127.0.0.1:8787 by default with no address detection, a non-loopback address only when set explicitly and limited to the Tailscale ranges, reached through `tailscale serve`) with option keys. Requires a per-device token (amended, Ruling 129: a one-time token per push, bound to one ticket and device, which rests on the ntfy topic and is accepted on two written conditions: a self-hosted or access-controlled topic, and an endpoint reachable from the tailnet only). Off by default.
- **Quiet hours:** no pushes in a configured window; blocking tickets queue until morning.

### F12. Persistence and restore

**What:** a reboot or tmux crash costs minutes, not context.

**How:** the store already has every session's pane, worktree, branch, agent and session id. `curia restore` recreates tmux sessions and windows, then relaunches each non-ended agent with `claude --resume <session_id>` in its worktree (amended, Ruling 124: also each agent whose session ended with reason `other`, which is what Claude Code reports when its terminal goes away; amended, Ruling 130: not one whose pane was closed while the tmux server kept running, unless `--include-closed`). Works alongside tmux-resurrect but does not require it.

**Teams:** Agent Teams teammates cannot resume, so restore relaunches only each team's lead with its session id and tells it to respawn its team from the shared task list.

### F13. Integrations

- **Obsidian:** each answered decision is exported as a note under a configured vault folder (`Decisions/D-117 Postgres for events.md`) with frontmatter (id, repo, task, scope, status, date) and the question, options, answer and reasoning in the body. One-way export; the store stays the source of truth. (amended, Ruling 175: a note without Curia's marker is never rewritten; the decision is skipped, the skip is remembered, and `curia export obsidian --retry-skipped` tries it again.)
- **Linear:** read-only import of issues into tasks (F5). Never writes statuses. (amended, Ruling 168: a failed page imports nothing; a retry imports everything.)
- **cost-lens:** if installed, it supplies per-session cost to F2.
- **Agent Commons:** decisions with `global` scope can be published as entries, so precedent is shared beyond `curia`.
- **Codex:** same hook adapter keyed by agent type, since Codex now exposes compatible lifecycle hooks; MCP tools work unchanged. (amended, Rulings 166 and 170: Curia never rewrites Codex's configuration and never calls `codex mcp add` when a `curia` table exists; `curia restore` skips a Codex session and prints the command to run by hand. Codex support is partial until `curia install --codex` exists, so a Codex session has no Curia tools or hooks yet.)

### F14. Agent Teams as first-class units

**What:** Curia does not build its own orchestrator. Coordination inside one job is left to Claude Code's [Agent Teams](https://code.claude.com/docs/en/agent-teams), where a lead session coordinates teammate sessions. Curia shows teams, routes their decisions and tracks their cost, the layer Agent Teams does not cover.

**Why this split:** Agent Teams works at the level of one job (one lead, one codebase, short-lived). Curia works at the level of the whole portfolio (many independent tasks across repos and days, plus decisions, precedent, cost and review). A Curia executor agent would duplicate the team lead and compete with an experimental Anthropic feature that will keep moving.

**Detection:** teammates are ordinary Claude Code instances, so F1's hooks fire for each. Curia reads team membership from the team config Claude Code maintains, where the lead's entry carries the agent type `team-lead`, and stores it as two new `sessions` columns: `team_id` and `team_role` (lead, teammate, or empty for solo sessions). (amended, Ruling 98: the config is `~/.claude/teams/session-<8 hex of the lead's session id>/config.json`; the lead is the member whose session is `leadSessionId`, its pane id is the literal `leader`, a teammate is identified only by its `tmuxPaneId`. amended, Ruling 115: a pane match counts only when the session is in the lead's tmux window and has no task or the lead's.)

**Dashboard:** a team shows as one collapsible group: the lead row on top, teammates indented beneath. The group row aggregates state (worst state wins, so one waiting teammate marks the team), cost, open decisions and cache state. Solo sessions display as before.

**Escalation chain:** teammates ask their lead first, which Agent Teams already supports. Only the lead raises Curia tickets. Enforcement is in the MCP server, not just instructions: `curia mcp` reads the caller's session id, and `request_decision` from a teammate returns an error telling it to ask its lead. The lead absorbs within-task questions, so I only see what the lead cannot settle.

**Spawning a team:** `curia spawn T-42 --team` launches the lead with Agent Teams enabled and `--teammate-mode tmux`, so teammates open as panes Curia can see. The task brief tells the lead it may form a team.

**Cost:** per-team cost is the sum of its sessions, shown on the group row and in the review (F10), so a team that burned five times a solo run is visible at merge time.

**Operator skill (deferred):** the natural-language interface the executor idea was after ("what's blocking the auth work?", "start tasks for these three bugs") comes from a skill, not new code. Any Claude session given the Curia operator skill can use the CLI and read-only MCP tools. It has the same limits as any agent: it cannot answer tickets, approve reviews or merge.

**Risks:** Agent Teams is experimental and off by default, so its config format and semantics may change. Curia treats team detection as optional: if it fails, teammates appear as solo sessions and nothing else breaks. Only one team runs per terminal session, and teammates cannot spawn nested teams, so a team is always one level deep.

### F15. cost-lens integration

**What:** [cost-lens](https://github.com/ZachFara/cost-lens) is the cost engine; Curia is the fleet and decision layer. Curia computes no cost itself, so there is one price table and one cold-rebuild detector, both in cost-lens.

**Push (live):** at each turn end, the cost-lens mod calls `curia ingest` through its existing `$.process.run`, passing one JSON document on stdin:

```json
{"schema_version": 1, "session_id": "…", "model": "…", "cost_usd": 4.12,
 "context_used": 142000, "context_max": 200000,
 "cache_expires_at": "2026-11-11T14:05:00Z", "cache_rebuild_usd": 1.68,
 "rate_limits": {"five_hour": {"used_percentage": 62, "resets_at": "…"},
                 "seven_day": {"used_percentage": 31, "resets_at": "…"}}}
```

Curia validates `schema_version`, ignores unknown fields, and never fails the caller: a bad document is logged and dropped.

**Pull (history):** for per-branch and per-task cost in reviews (F10), Curia runs the cost-lens analyzer CLI and reads its JSON output. (amended, Ruling 165: it is run on the task's worktree path, since sessions in a worktree are not counted under the main repository, and only `currentProject` is read.)

**Without cost-lens:** Curia registers its own status-line command, which forwards `rate_limits` and cost from Claude Code's status-line JSON into the same ingest path. Cache state is then unknown and shown as blank. (amended, Ruling 162: Claude Code's status-line JSON carries the session's prompt-cache expiry and rebuild size in tokens, so cache state is known without cost-lens; Ruling 167: the status line is added only when none exists and never replaces one.)

**Contract ownership:** the JSON schema lives in Curia's repo as `docs/ingest-schema.json`; cost-lens adds the push behind a setting, off unless Curia is on `PATH`.

**Risk:** cost-lens is built and tested on macOS (its index lives under `~/Library/Caches`). Remote Linux use needs a Linux cache path in cost-lens first.

### F16. Usage forecasting and budget-aware scheduling

**What:** Curia projects when I'll run out and acts on it, instead of only showing a percentage.

**Data:** every ingest of `rate_limits` appends a row to a `usage_samples` table (`at`, `window`, `used_pct`, `resets_at`). The windows are account-wide, so samples from different sessions are merged into one series per window.

**Subscription math:** burn rate = change in `used_pct` over the trailing 20 minutes. Time to limit = (100 − `used_pct`) / burn rate. If the limit lands before `resets_at`, the status badge shows a warning, e.g. `5h 62% → limit ~48m (resets 1h50m)`.

**API math:** with no `rate_limits`, projected spend over the next 2 hours = the trailing 20-minute dollar burn rate across live sessions, extrapolated.

**Attribution:** per-session token deltas show which agent is eating the window; the dashboard sorts by burn on request.

**Acting on it:** tasks gain a `priority` field (P1 to P3, default P2). When the forecast says I'll hit the limit before reset, Curia (1) suggests pausing the lowest-priority running task, with one key to accept, and (2) defaults new spawns to the cheaper model, using cost-lens's model advisor where available. (amended, Ruling 169: the cheaper model is the configured name `usage.cheaper_model`, applies to that one spawn only and is not stored on the task; the advisor has no command to call and is not used.) It never pauses anything on its own.

**Per-task budget (amended, Ruling 143):** a task may carry a budget, `curia task add --budget 5.00` or `curia task edit T-42 --budget 8` (user-only), or the default `cost.task_budget_default` (0, meaning none). Task cost is the sum of `cost_usd` over every session linked to the task, teams included. The dashboard cost column shows `$4.10/$5`, red past 100%. With `cost.budget_hard_stop = true` (default false) the PreToolUse hook denies every tool call of the task's sessions once the cost reaches 100%, except Curia's own MCP tools, so the agent can still raise a ticket or call `complete_task`; the deny reason tells it to stop and report. Raising the budget lifts the stop. This is the one case where Curia stops work without my keypress, and it is a stop, not a pause: nothing is removed or committed. Cost is known only at turn end (F15), so a stop lands up to one turn late; a session with no cost data cannot be stopped and shows `budget ?`. Agents cannot set or raise a budget. The stop blocks only by the hook's deny (or exit code 2), and the reason reaches the model as the tool error.

**Risks:** the stop is best-effort. A hook that exits 1 or times out fails open to Claude Code's normal prompt (observed on 2.1.329), so the stop can land late or be skipped when the hook cannot decide (section 7).

### F17. Cache-aware decision triage

**What:** an agent blocked on a decision is idle and its prompt cache is expiring. Answering after expiry makes its next turn re-write the whole context, so decision latency costs money.

**Queue order:** blocking tickets sort by the asking session's `cache_expires_at`, soonest first, then by age. Tickets from sessions with unknown cache state sort by age after them.

**Ticket display:** each ticket shows the stakes, e.g. `answer within 12m to keep cache warm (rebuild ≈ $1.68)`. (amended, Ruling 163: where the dollar figure is unknown, as with the status line, the line gives tokens, never an invented dollar figure.) The same line goes into the ntfy push (F11). Once the cache is cold, the line changes to `cache cold: next turn re-writes ~84k tokens`.

**Dashboard:** a cache column for every idle or waiting session: time left, or cold with the rebuild cost. (amended, Ruling 163: tokens where dollars are unknown.)

### F18. Message routing

**What:** the inbound mirror of the decision queue. Agents send judgment calls up to me (F7); this sends my thoughts down to them. I type a message once, in plain language, and Curia works out which agent, task or repo it belongs to and how it should land.

**Entry points:** `curia say "..."` from any shell (including SSH from my phone), and `prefix + m` in tmux, which opens a one-line prompt in a popup. (amended, Ruling 112: `curia say` is user-only and needs a controlling terminal, so SSH needs a pty; a message from anywhere else is not sent as the user's.)

**Routing, in order:**

1. **Explicit address.** A message starting with `@<repo>`, `@<alias>` or a task id (`T-42:`) routes deterministically against repo names, branches, task ids and aliases. No model call.
2. **Haiku suggestion.** Otherwise Curia calls Haiku through `claude -p --model haiku --output-format json` (amended, Ruling 99: with `--safe-mode --no-session-persistence --tools ""` and no `--json-schema`; the plain JSON answer is parsed strictly), so it runs on my existing Claude login with no separate API key. The prompt holds my message plus a compact list of live sessions and open tasks (id, repo, branch, task title, latest update). Haiku returns `{target, kind, confidence, runner_up}`. Timeout: 5 seconds; on timeout or error I pick the target by hand.

**Broadcasts and schedules (amended, Ruling 152):** the router also handles messages for many agents or for later. `@all` or "tell everyone X" targets every live agent session; `@<repo>/*` or "tell everyone on api-server X" targets one repository's. One message row is made per session (sharing a `broadcast_id`), and a team's goes to its lead (F14). "At 2am tell all agents to continue" creates an F22 schedule. Haiku's answer gains `scope` (`one`, `all`, `repo`) and, for a schedule, `when` as the user's words; Curia parses `when` with the same parser as `curia schedule --at` and, if it cannot, asks me for the time. A broadcast or a schedule always asks for confirmation, even when auto-send is on (below), and is never counted in the accuracy gate's labels.

**Other entry points (amended, Ruling 153):** the web page (F26) and `curia serve` accept messages too, so I can route one from my phone. They are held until I confirm on the page, never auto-sent, and need the web device token.

**Message kinds** (from the same Haiku call, or a prefix like `rule:` or `task:`):

- **Instruction:** delivered to a live agent now, through the same path as decision answers (F7): typed into the pane if idle, handed over by the Stop hook if working. A team's messages go to its lead (F14).
- **Standing rule:** becomes a precedent entry (F9) with repo or global scope, so every future agent sees it, not only the ones running now.
- **New task:** added to the backlog (F5) instead of interrupting anyone.

**Mode B, confirm (default):** the prompt shows Haiku's pick, e.g. `→ api-server · T-42 auth refactor · instruction (0.93)`. `Enter` sends, `Tab` cycles to the runner-up and other targets, `k` changes the kind, `Esc` cancels.

**Mode C, auto-send (opt-in):** above a confidence threshold, the message goes without confirmation. Guards:

- **Off by default.** Enabled with `routing.auto_send = true` and `routing.auto_threshold` (default 0.9) in config.
- **Earned, not assumed.** Every confirmation in mode B is logged as a label: accepted unchanged, or overridden. Auto-send can only switch on once Haiku's accuracy at or above the threshold is at least 95% over the last 50 labeled messages. `curia routing stats` shows the numbers.
- **Close calls still ask.** If the top two candidates are within 0.15 confidence, or the target session has ended since the list was built, Curia falls back to confirm.
- **Standing rules always ask.** They persist and shape every future agent in that scope, so they are never auto-sent. Instructions and new tasks can be.
- **Undo window.** An auto-routed message shows `→ api-server (u to undo, 5s)` and is held for 5 seconds before delivery. Undo cancels it outright, since nothing has reached the agent yet.
- **Keeps measuring.** One auto-eligible message in ten still asks for confirmation, so accuracy keeps being measured. If it drops below 95%, auto-send switches itself off and says so in the status badge.

**Data:** a `messages` table: `id`, `text`, `kind`, `target_session`, `target_task`, `target_repo`, `suggested_target`, `confidence`, `routed_by` (address, confirmed, overridden, auto), `status` (held, delivered, undone, canceled; amended, Ruling 97: the spelling is `canceled`), `created_at`, `delivered_at`; amended, Ruling 152: `broadcast_id` and `via`. Labels for the accuracy gate come from `routed_by`.

**Safety:** routed text goes through the same sanitizing path as decision answers (section 7). The list Haiku sees contains only session metadata, never transcript content. A prompt that is a task notification or an agent message is a system turn: Curia recognises it, it is not the user, and it neither confirms a typed line nor shows that I am here (observed to fire the prompt hook, section 9).

### F19. Resources

**What:** I see what each agent costs the machine, not only the bill: CPU and memory per session, and the Docker containers each one started, with limits so five agents do not fill a laptop. (Ruling 145 introduces `curia monitor`; Ruling 146 sets the limits.)

**How:**

- **Process tree.** Every `resources.interval` (default 10 seconds) `curia monitor` (section 4) reads `#{pane_pid}` for each live session and sums `%cpu` and resident memory over that pid and all its descendants from one `ps -axo pid,ppid,%cpu,rss` call. The sums go to `resource_samples`. The dashboard (F3) gains CPU, RAM and `ctr` (container count) columns; `curia resources [--json]` prints the same per session plus a total line.
- **Container attribution.** `curia launch` (F6) puts a directory holding a `docker` wrapper first on the session's `PATH`. For `docker run`, `docker create` and `docker container run|create` the wrapper adds `--label curia.session=<session id>` and then runs the real `docker`, whose path `curia install` resolved once with the wrapper directory removed from `PATH`. Every other subcommand passes through unchanged. `docker compose up` is not covered: the wrapper sees only the `compose` call, never a `run` or `create` (observed), so a `docker run` label is not added to Compose containers and they never carry `curia.session`. They are attributed by Compose's own labels: a container whose `com.docker.compose.project.working_dir` label lies under a session's worktree or cwd belongs to that session, and so does every container with the same `com.docker.compose.project` label. Anything left is listed as `unattributed` and counts toward the total only: a Compose file outside the worktree (`-f ../shared/compose.yml`), the SDK or Testcontainers calls on the socket, and an absolute path to `docker`. The working-directory label is present in Compose 5.1.0 (observed, with `project.config_files`) but not on the Compose-file docs page (documented there: the project and service labels only), so it may change.
- **Caps.** The caps apply to `run` and `create` only; for `compose up` the wrapper only checks the count before it starts (it refuses when a cap is already reached) and cannot know how many containers the file then creates. The wrapper refuses `run` and `create` when the session already runs `resources.max_containers_per_session` containers (default 3) or all sessions run `resources.max_containers_total` (default 8), counting live containers with `docker ps` at call time, not from the last sample. It also refuses when Docker's memory in use is above `resources.docker_memory_refuse_pct` (default 80) percent of Docker's memory total, from the last sample (the use is the containers' sum, so it undercounts). A refusal prints to stderr and exits 1 with a message the agent reads, e.g. `curia: refused: this session already runs 3 containers (resources.max_containers_per_session). Stop one, or ask the user to raise the cap.` It names the cap, never the config path's contents, and the agent cannot change it (F28).
- **As built:** each count cap and the memory threshold is switched off by setting it to 0 (owner decision, 2026-11-20; Ruling 207), and a repository file may only lower a nonzero value; the same switch governs the `compose up` refusal.
- **Docker memory.** On Linux the limit is host memory. On macOS containers run in the Docker Desktop VM. `docker info` `.MemTotal` reports the VM's total memory (static; observed on Docker Desktop, engine 29.2.1) and no command reports its current use. Current use is the sum of `docker stats --no-stream` MemUsage over running containers, which undercounts (it leaves out the VM's kernel and Docker's own daemons). The monitor shows `containers 5.1 of 8 GB` in `curia resources` and the dashboard header.
- **Stale data.** If the newest sample is older than 2 minutes (the monitor is not running), count caps still apply and the memory threshold is skipped with a log line; the dashboard shows `resources: monitor not running`.

**Acceptance:** a session running `sleep 300` under a child shell shows its summed RSS within one interval; a wrapped `docker run` carries the `curia.session` label; the fourth container of one session is refused with exit 1 and the message above, and a different session's `run` still passes; a Compose container whose working-directory label is under a worktree is attributed to that session (through that label, not `curia.session`), and one whose file lies outside it is `unattributed`; above the memory threshold a `run` is refused; with no monitor the count cap still refuses and the memory check is skipped; `docker ps` passes through byte for byte; the wrapper adds under 50 ms when Docker is up.

**Risks:** the wrapper is on `PATH`, so an agent that calls `/usr/local/bin/docker` or uses another runtime bypasses it. Compose containers are attributed by label and the rest are `unattributed`, but a cap cannot be enforced on a call Curia never saw, and a `compose up` is refused only when a cap is already reached, never for what it would create. The caps stop accidents (a loop that starts containers), not an agent that sets out to avoid them. Sessions not started by `curia launch` have no wrapper at all. CPU and RAM are samples, so a short spike between two samples is missed. Fail-open on stale memory data trades a missed refusal for not blocking work when the monitor has died.

### F20. Modes and away

**What:** one setting decides how much of Claude Code's permission prompting I am present for. Four modes, set per repository or for everyone, with expiry, so a long run does not stop on every prompt and a forgotten setting does not stay on. (Rulings 147 to 149; they amend "only the user decides", see section 7. Bypass is owner decision D-6, 2026-11-20.)

**Modes:** `curia mode normal | edits | away | bypass [--all | --repo [<path>]] [--for 6h]`. `curia mode` alone prints the effective mode per session. The default scope is the repository of the current directory (an error outside a registered repo); `--all` is every session.

| Mode | What Curia's hook answers |
| --- | --- |
| normal | Nothing: Claude Code prompts as usual. The default. |
| edits | `allow` for Edit, Write, MultiEdit and NotebookEdit when the path is inside the session's worktree (or cwd); no opinion on everything else. A user's ask rule still prompts. |
| away | As `edits`, plus `allow` for a Bash command that matches `mode.away_allow` (below). Anything else is `deny` with a reason telling the agent the owner is away, to raise an `assume` ticket (F7) and continue with other work. Every denial is stored. |
| bypass | `allow` for every call, except what the user's own settings forbid (owner decision D-6, 2026-11-20). A hook's `allow` cannot override a deny rule, an ask rule or a managed policy (permissions page, read 2026-11-19; a deny rule and an ask rule observed on 2.1.329), so the user's deny rules keep working in bypass: this is a safety property. Some prompts still appear: `AskUserQuestion` and tools that need interaction, recognized `rm` and `rmdir` critical-path removals, an explicit ask rule, and edits in `plan` mode. |

**How:**

- **Enforcement.** The PreToolUse hook (F1) already runs for every tool call. It now reads the session's effective mode from `modes` (read-only, within the 20 ms budget) and prints Claude Code's permission decision. Narrower scope wins: a repo row beats an all row. An expired row counts as absent at read time, so expiry needs no running process. If the store cannot be read the hook gives no opinion, which is `normal`. The hook gives up at its own short deadline (the 20 ms budget) and then prints no opinion and logs `undecided` with the reason to the hook log. Claude Code's own hook timeout is long (600 seconds by default). A hook that exits 1 or times out FAILS OPEN to Claude Code's normal prompt (observed on 2.1.329): the prompt appears and nothing is silently allowed, but every rule Curia enforces only through the hook (away's denials) is best-effort, bounded by the hook's own short deadline. Under `away` nobody is there to answer that prompt, so the session waits at it.
- **What the hook's answer can do (observed on 2.1.329).** `allow` removes the prompt only when no deny rule and no ask rule matches and the session is not in plan mode; in plan mode an `allow` lets neither an Edit nor ExitPlanMode through. `deny` and exit code 2 block, and the reason reaches the model as the tool error (`PreToolUse:Bash hook error: <reason>`), so it is written for the model.
- **`mode.away_allow`** is a list of globs, default `["git status*", "git diff*", "git log*", "git add *", "git commit *"]`. A command matches only if it contains none of these characters (semicolon, ampersand, pipe, dollar sign, parentheses, backtick, angle brackets, backslash, newline) and the whole string matches a glob; otherwise it does not match. Test and build commands are for me to add (they are my decision, not Curia's).
- **bypass.** `curia mode bypass` is user-only (the F6 guard, Ruling 119) and then prints what it will allow (and what will still prompt, as in the table) and reads the word `bypass` from `/dev/tty`, not stdin, as `curia setup approve` does (Ruling 109). Anything else cancels. `--for` defaults to `mode.bypass_for` (default `2h`) and is always set, so bypass always expires. The status badge (F4) shows a red `BYPASS` while any session is in it, and the dashboard header names the scope and time left.
- **`--native`.** `curia mode bypass --native` (needs `--for` and the typed word) asks each idle agent in scope to exit with `/exit` and relaunches it through the restore path (F12) as `claude --resume <session> --dangerously-skip-permissions` in the same worktree; `--resume` does not keep the flag, so the relaunch passes it again (sessions page, read 2026-11-19). Observed on 2.1.329: the flag is accepted with `--resume` and the session id is kept. Its first use in a Claude Code configuration shows a blocking acceptance dialog that Curia must never answer, so `--native` requires that I have accepted it once myself; when Curia sees the dialog in the pane it reports it and stops, and never types into it. It is the explicit stronger form: it skips prompts in Claude Code itself, not only through Curia's hook. It needs `--for`. It is refused as root, and managed policy can disable bypass mode (`permissions.disableBypassPermissionsMode`), in which case `--native` is refused with that reason. Busy agents are skipped and listed. When the mode expires or is changed, the same agents are relaunched without the flag (idle ones; busy ones are listed and the badge keeps `BYPASS` until none remain). A plain resume returns the session to Claude Code's default (auto) mode on 2.1.329, not to the mode it had before, so restoring covers Curia's own mode row and cannot restore Claude Code's own permission mode; the summary says both.
- **`curia away <duration>`** (user-only, e.g. `curia away 6h`):
  1. Curia sends every live agent a message (the F18 delivery path, never auto-skipped): `The owner is leaving for 6h. Raise every decision you can foresee now with request_decision, then end your turn.`
  2. It waits until every agent has ended its turn since the message was confirmed (the session is `idle` or `needs_decision`), or `away.prepare_wait` (default 10 minutes) passes. The dashboard shows who is ready. A turn that starts from a task notification or an agent message is a system turn: Curia recognises it and it is not the user. There is no new MCP tool for "ready".
  3. It saves the current modes into `away_runs`, replaces all of them with `away` for everyone, and sets the expiry to the duration.
  4. While away, `request_decision` with `mode: "blocking"` is stored as `assume` (`decisions.coerced_from = blocking`) and its result tells the agent to proceed with its recommendation; actions that are irreversible are denied by the hook anyway.
- **`curia back`** (user-only) ends the away run, restores the saved modes (never `bypass`: a saved bypass is dropped and must be typed again; this covers Curia's own mode rows and cannot restore Claude Code's own permission mode of a session that bypass or `--native` changed), and prints and stores a summary from the F25 timeline for the away window: work done (turns ended, reviews opened, commits), assumptions to confirm (open `assume` tickets created while away, one key to confirm each), actions denied (tool, command line, session, count), and cost (sum of F2 cost deltas). Expiry without `curia back` restores the same way and leaves the summary for the next `curia back`.
- **Visibility.** Mode changes append to `modes` history and to the timeline. `curia mode` and the dashboard show the effective mode per session.

**Acceptance:** in `edits` a Write inside the worktree returns allow and one outside returns no opinion; in `away` `git status` allows, `git status; rm -rf x` and `$(…)` deny, and the denial is stored; `curia mode bypass` without the typed word changes nothing, and an agent session cannot run it; a `bypass` row past its expiry is not applied by the hook; with a deny rule in the user's settings a call is still refused in bypass; `curia back` after a saved `bypass` restores `normal`; `away` waits at most 10 minutes for readiness and then proceeds, naming who was not ready; a blocking ticket raised while away is stored `assume` with `coerced_from = blocking`; the summary lists each denied action once with its count.

**Risks:** a hook that auto-allows is a standing permission grant: in `bypass` any prompt injection that reaches the agent runs unprompted, and `edits` lets an agent rewrite any file in the worktree, including `.curia/setup.sh` and `.claude/settings.json` (which a later session then trusts). Bounds: bypass is typed, scoped, red and short; `away` denies by default; modes live only in the store, never in a file an agent or a repository can write (F28), and agents have no tool to change them (the MCP surface has no mode tool and `curia mode` is user-only). Typed approval is a guard against mistakes and naive agents, not against one that scripts a terminal (README, "What the user guard does and does not do"). `--native` outlasts the hook: an agent relaunched with the flag runs without Curia's hook in the loop until it is relaunched. Away denial is a heuristic on a command string, not a sandbox. Everything the PreToolUse hook enforces (away's denials, the budget stop of F16, the protection of Curia's config in F28) is best-effort against a crashed or hung hook, which falls back to Claude Code's normal prompt and blocks nothing; the bound is the hook's own short deadline and the `undecided` line it logs, so a gap shows in the log but is not prevented (section 7). The hook's output format and precedence were observed on 2.1.329 (section 9).

### F21. Remote Control

**What:** every session can be driven from the Claude app on my phone, and I can see which ones are.

**How:**

- **On install.** `curia install` sets `remoteControlAtStartup` to `true` in the user settings file (`~/.claude/settings.json`; the key behind "Enable Remote Control for all sessions", remote-control page, read 2026-11-19) only if the key is absent (Ruling 150: never over an explicit `false` or any value I chose, backed up and removed by `curia uninstall` only if Curia added it, as with hooks). It reports the file and key it touched. Disabled by `rc.enable_on_install = false` (owner decision D-10, 2026-11-20).
- **`curia rc --all`** (user-only) types `/rc <name>` (documented: `/remote-control`, alias `/rc`, takes a name) into each live session's pane, except one known `on`, because a second `/rc` opens a status panel in the CLI and disconnects in the Desktop app (owner decision D-10, 2026-11-20). The name is `<task id> <title, 40 characters> · <repo>`, or `<branch> · <repo>` with no task, cleaned like every typed text (section 7). It follows the typing rules (AGENTS.md): only into a pane whose empty prompt was seen, one unconfirmed typed line per session, never into a window with `synchronize-panes`. A busy session gets a pending row (`sessions.rc = pending`) typed by the monitor at the session's next stop. `curia rc <session|T-id>` does one. `--remote-control [name]` exists as a command-line flag in 2.1.329: it is a candidate for sessions Curia launches itself, to be confirmed by the owner's probe.
- **Column.** The dashboard gains an `RC` column: `on`, `off`, `pending` or `unknown`. A fresh session is `off` until `/rc` is typed. Hooks carry no Remote Control field. The documented `/rc active` indicator in the pane (hidden when the pane is narrow) is the only signal Curia reads, and without it the column is `unknown`; whether the status-line JSON or a hook carries the state needs a live probe (section 9).
- **`curia rc --check`** types `/rc` only into a session Curia has positive evidence is disconnected (the documented failure indicator or the `Remote Control not started here` notice, in a pane capture), under the same rules. Otherwise it types nothing and reports `on`, `off` or `unknown` from the pane. It never reports `on` without the indicator.
- **Confirmation.** A `/rc` line carries no message id, so the prompt-name rule (Ruling 86) cannot confirm it. Built-in slash commands fire no UserPromptSubmit (observed with `/status`, `/usage` and an unknown command, 2.1.329), so a typed `/rc` cannot be confirmed by the prompt hook. Confirmation is by reading the pane after typing, bounded: it counts as confirmed when the next capture within 5 seconds shows the empty prompt again and no dialog; "unknown" when Curia cannot tell; otherwise it stays unconfirmed and blocks further typing into that session (the badge's `Q`). The first `/rc` may open a one-time confirmation dialog or a panel: Curia inspects the pane and sends nothing further until it is closed. `/rc` itself was not run (an owner-run probe, section 9). A system turn (a task notification or an agent message) is recognised and is not the user.

**Acceptance:** with all sessions idle, `curia rc --all` types one `/rc` line into each and no other pane; a session with a draft in its prompt gets nothing typed and the note says why; a working session becomes `pending` and is typed after its Stop; a second `--all` does not type into a session with an unconfirmed line or one known `on`; `--check` types nothing into a session with no disconnect evidence; `curia install` leaves an existing `remoteControlAtStartup` value, `false` included, alone and `uninstall` removes only what it added.

**Risks:** typing `/rc` depends on a documented command whose argument and panel may change; when the prompt is not recognized nothing is typed (AGENTS.md), so a UI change turns the feature off quietly. Remote Control exposes the session to anything holding my account: it is a reach extension, which is why repo files cannot enable it (F28). Because state is mostly `unknown`, the column can mislead if read as `on`; a pane narrower than the indicator hides it.

### F22. Scheduled messages

**What:** a message that is sent later or repeatedly: "continue", at 2 a.m., or when the usage window resets and agents have been stopped by the limit. (Ruling 151.)

**How:**

- **Commands.** `curia schedule "<text>" (--at <time> | --daily <HH:MM> | --on reset) (--all | --repo <name> | --task T-42) [--if-idle] [--only limited]`, plus `curia schedule ls [--json]`, `show S-3`, `cancel S-3`, `pause S-3`, `resume S-3`. A target is required. Times are local; `--at` accepts `HH:MM` (the next one), `YYYY-MM-DDTHH:MM` or `+2h`.
- **Always confirms.** Creating a schedule is user-only and prints the text, the trigger, the targets as they resolve now and the count, and waits for Enter on the terminal. There is no `--yes`. Created from the router (F18) or the web page (F26) it confirms there. Auto-send never applies.
- **Runs in `curia monitor`.** The monitor wakes at the earliest `next_run_at`. A fire resolves the targets then, makes one `messages` row per session (kind `instruction`, `via = schedule`, labeled as from the user, naming the schedule), and delivers through the F7 paths: typed into an idle pane with an empty prompt, handed over by the Stop hook, or fetched by `check_messages`. System turns (task notifications, agent messages) are recognised and are not the user: they do not confirm a typed line. Teams get theirs on the lead (F14).
- **Filters.** `--if-idle` delivers only to sessions that are idle at fire time; the rest are recorded as skipped for that session, not retried. `--only limited` delivers only to sessions stopped by the usage limit: the latest `usage_samples` row (F16) for a window is at or above `schedule.limited_pct` (100) and the session went idle after it. This stays derived from usage samples, "likely": the status line has no field for a limit stop, and a real limit stop was not observed (owner-run, section 9). A `StopFailure` event with a `rate_limit` error is documented (hooks page) but has not been seen; if it fires, it is preferred. `--on reset` fires `schedule.reset_grace` (default 1 minute) after a window's `resets_at` passes.
- **Missed triggers.** If the monitor was not running or the machine slept, a trigger fires on wake only if it is no more than `schedule.late_window` (default 30 minutes) late. Otherwise it is skipped, recorded `skipped_late`, shown in `curia schedule ls`, in the dashboard header and in the next `curia back`. A `--daily` schedule then waits for its next day. A `--at` schedule is done.
- **History.** Each fire writes a `schedule_runs` row: due time, run time, outcome (`fired`, `skipped_late`, `skipped_no_target`, `monitor_down`) and per-session results. `curia schedule show S-3` prints it.

**Acceptance:** `--at 02:00` creates a confirmed row and fires once; a fire at 31 minutes late is `skipped_late` and at 29 minutes fires; `--if-idle` skips a working session and delivers to an idle one in the same run; `--on reset` fires after `resets_at` plus the grace; a scheduled message is not typed into a pane with a draft or with an unconfirmed line; canceling stops all later runs; an agent session cannot create, edit or cancel a schedule.

**Risks:** a scheduled message types into panes while nobody is watching, under whatever mode is active; paired with `bypass` or `away` it starts unattended work. Bounds: the text and targets are fixed and confirmed at creation, delivery obeys every typing rule, `curia monitor` must be running (without it nothing fires), and `schedule.late_window` limits stale firing. Time handling around daylight-saving changes uses local time and is not exact to the minute. `--only limited` can miss or wrongly match sessions if the limit signal is unreliable.

### F23. Inbox

**What:** one list of everything that needs me, so the dashboard answers "what do I do next" before "what is everyone doing". (Ruling 154.)

**How:** `curia inbox [--json]`, and the default view of the `prefix + a` popup (F3 amended: `Tab` switches to the session list, `t` to the board). Items, in this order:

1. Blocking decisions, ordered as F17 orders them.
2. Permission prompts: sessions in `waiting`, showing the Notification text.
3. Reviews pending (F10).
4. Assumptions to confirm: open `assume` tickets (F8).

Within a group, oldest first. Each row shows age, session or task, and a one-line summary. Keys: `Enter` opens the matching view (`curia decide`, a jump to the pane for a prompt, the review); `a` accepts the recommendation of a decision or confirms an assumption (the F7 key, with the same confirmation); `/` filters. The counts equal the status badge's `D`, `W`, `R` and the assumption count.

**Acceptance:** the inbox rows equal the union of the four queries and sum to the badge; answering one removes it within one refresh; the popup opens on the inbox; an empty inbox says so; `--json` is stable for scripting.

**Risks:** an item can be gone by the time `Enter` lands (answered by phone), so each view re-reads and says so. Permission prompts are only shown and jumped to; Curia does not answer them (F20 is the only automatic answer).

### F24. Agent detail

**What:** a drill-in for one session that shows what it is doing without jumping to its pane.

**How:** `curia show <session|T-42|pane>` and `i` on a dashboard or inbox row. Panels:

- **Transcript tail:** the last 30 lines of assistant text and tool names from `transcript_path` (parsed incrementally, F2), tool output truncated to one line, all text cleaned of control and escape sequences.
- **Tickets:** this session's and task's decisions with status and answer.
- **Messages:** queued, delivered and unconfirmed, with the blocker (F18).
- **Diff:** `git diff --stat` of the worktree against the base, and `o` for the full diff in my tool (F10).
- **Cost:** session and task cost, budget progress (F16), cache state (F17).
- **Activity:** time by class (F27) and the last 10 events.

`Enter` jumps to the pane, `Esc` goes back. The view is read-only. `--json` prints the same.

**Acceptance:** each panel matches the underlying rows; a transcript with escape sequences displays none; a session with no worktree shows no diff panel; an ended session still opens; no write to any table.

**Risks:** transcripts can hold secrets and untrusted text, so they are shown only locally and never on the web page (F26). A missing or rotated transcript shows an empty tail with a note.

### F25. Timeline and decision log

**What:** what happened, in order, across all agents; and what I decided, searchable, with how often it was reused. (Ruling 160.)

**Timeline.** `curia timeline [--since 6h] [--repo R] [--task T-42] [--json]`. It is a union over existing rows, with no new table: session start and end, turn end (a Stop with its last line), ticket raised, answered, confirmed or overridden, review opened and closed, message delivered, mode changed, action denied, schedule run, task filed, push sent. Each line has time, session or task, kind and one line. It feeds `curia back` (F20) and the web timeline (F26).

**Decision log.** `curia decisions log [query] [--repo R] [--scope S] [--status X] [--since D]` searches with F9's FTS. Each row shows id, question, answer, scope, date and reuse: the count of decisions whose `precedent_of` is this one, the last reuse date, and `asked again`, the count of later tickets raised although `request_decision` returned this decision as `near_match`. One summary line gives the share of answered decisions reused at least once. `Enter` shows one in full; editing stays `curia decisions edit` (F9).

**Acceptance:** the timeline orders events by time across tables and a `--task` filter keeps only that task's; a ticket answered twice (override) appears twice; the log's reuse count equals a count over `precedent_of`; superseded decisions are marked and are not counted as reuse targets.

**Risks:** the union is only as detailed as the stored rows: state changes between hook events are not kept, so a working to waiting flip shows only as turn ends and tickets. Large stores need an index on the time columns and a default `--since`.

### F26. Web view

**What:** a local page for the questions a terminal answers badly from a phone: the board, the timeline, the decision log, usage, and a way to send a message. (Ruling 153; owner decision D-9, 2026-11-20.)

**How:**

- **Where.** `curia serve` (F11), when `serve.web = true` (default false), also serves the page on its existing listener: loopback by default, reached from the phone through `tailscale serve` (Rulings 127 and 129). No new listener, no new port.
- **Pages:** `/` (board), `/timeline` (F25), `/decisions` (the log, F25), `/usage` (F16 and F27 time), `/settings` (the effective config with sources, read-only, F28). Server-rendered HTML, one small script file, no external assets, a strict Content-Security-Policy, every value escaped. Polled every 5 seconds. Transcripts, diffs and ticket `context` are not shown.
- **Proof.** Every route needs a device token: `curia serve token <device>` (user-only) prints a URL once; the token is stored only as its SHA-256 in `web_devices` and exchanged for an `HttpOnly`, `SameSite=Strict` cookie; `curia serve revoke <device>` ends it. A request must also carry an `Origin` or `Host` equal to `serve.public_url`. This is the same kind of proof the answer buttons need (a bearer secret bound to a device, over an endpoint reachable from the tailnet only), but it is reusable, which Ruling 129's token is not.
- **Dragging.** Cards move only between `backlog`, `ready` and `dropped`, by `POST /v1/task/<id>/status`; any other column is not a drop target and the server refuses it. A move to `ready` is the owner marking a task ready (F5), including an agent-filed one; it does not make the task eligible for auto-spawn (F29).
- **Messages.** A box posts to `POST /v1/say`. The text is routed as in F18, the proposed target is shown, and nothing is sent until a second request confirms it (`POST /v1/say/<id>/confirm`). The web page never auto-sends and never skips the confirmation for broadcasts or schedules. A web message is delivered as the user's only with a valid token and Origin, and then through the same typing rules as every other message.

**Acceptance:** without a token or with a wrong Origin every route returns 401 or 403 and changes nothing; the board shows all columns and only the three drop targets accept a card; a message posted without confirm is stored `held` and never delivered; a broadcast posted and confirmed delivers one message per session; `serve.web = false` serves only the answer route as today; a revoked token fails at once; a response contains no transcript text.

**Risks:** the page widens what the endpoint exposes: Ruling 127/129 exposed one answer route and now it holds my tasks, decisions and a way to type into agents. Bounds: off by default, tailnet-only by the same conditions, token plus Origin, confirmation on every message. A device token that leaks is a standing credential until revoked. The same-user limit applies: an agent that can read the store cannot recover a token (only its hash is kept) but can reach loopback and would need one.

### F27. Activity time

**What:** where agent time goes, with one headline: the share of agent time spent waiting on me. (Ruling 155.)

**How:**

- **From hook intervals.** The hook (F1) closes the open interval and opens the next at each event. A tool interval runs from PreToolUse to PostToolUse and is classed by a rules table; the gap from a prompt or a PostToolUse to the next PreToolUse is `thinking`; a Notification until the next event, an open blocking ticket, and the time from a turn end to the next prompt are `waiting on me`. A prompt that is a task notification or an agent message is a system turn, recognised and not the user: it does not count as me being here. No model is called in a hook: classing is a first-match regular-expression pass over the tool name and the first 200 characters of a Bash command.
- **Classes and default rules:** `coding` (Edit, Write, MultiEdit, NotebookEdit), `reading` (Read, Grep, Glob, LS, WebFetch, WebSearch), `testing` (Bash matching `go test`, `npm test`, `pytest`, `make test`), `building` (`go build`, `npm run build`, `make`, `docker build`), `waiting on CI` (`gh run watch`, `gh pr checks --watch`), `git` (`git `, `gh `), `thinking`, `waiting on me`, `other`.
- **Rules table.** `[[activity.rules]]` entries (`class`, `tool`, `match`) go before the defaults. A repo's `.curia/config.toml` may add rules for that repo (they change only the measurement, F28).
- **Background commands.** A Bash call that runs in the background is tracked until it ends, from hooks alone (observed on 2.1.329): `run_in_background` is in the tool input, PostToolUse fires at once with a background task id in the response, the Stop payload's `background_tasks` lists what is still running, and the completion arrives as a prompt carrying a task notification (a system turn, above). The monitor (F19) is at most a fallback, sampling the pane's process tree when a completion event never arrives. After a turn ends while a tracked command runs, the time counts as that command's class (testing, building, waiting on CI), not as `waiting on me`.
- **Overlap.** Parallel tool calls count once: the interval opens at the first PreToolUse and closes at the last PostToolUse.
- **Idle cap.** A turn-end-to-prompt gap counts as `waiting on me` only up to `activity.idle_cap` (default 30 minutes); the rest is not agent time.
- **Report.** `curia time [--since 24h] [--repo R] [--task T-42] [--by class|task|type]` shows agent time (summed over sessions) and wall time (the clock window with any live session), and the headline `waiting on me: 38% of 14h 20m agent time (wall 6h 05m)`. The `/usage` page and the review (F10) show the task's breakdown.
- **Task type** is the first segment of the branch (`feat`, `fix`, `docs`, `chore`, `refactor`, `test`, `perf` by default, `activity.task_types`); any other branch is `untyped`.

**Acceptance:** a scripted event stream yields the expected seconds per class; `go test ./...` classes as testing and `git status` as git; a background `sleep 60` after a Stop counts as its class for 60 seconds and not as waiting; two overlapping tools count once; an idle gap of 2 hours adds 30 minutes of waiting; a repo rule overrides the default for that repo only; the hook stays under 20 ms.

**Risks:** the classes are guesses from a command string; a script named `check.sh` is `other`. Background tracking rests on the completion event; when it never arrives and the monitor is not running, the interval ends at the next event and is flagged unfinished. Agent time summed over parallel sessions can exceed wall time; the headline uses agent time on purpose.

### F28. Settings

**What:** one `config.toml` for every feature, layered so a repository can make Curia stricter and never looser, with history and rollback. (Rulings 156 to 159; auto-spawn is F29, owner decision D-8.)

**Layers:** defaults < `~/.config/curia/config.toml` (the user file) < `.curia/config.toml` in the repository < flags. The repo file is untrusted data: it is read with a 64 KiB cap, strict TOML, unknown keys ignored, and each key passes a compiled-in policy that says whether it may be set and in which direction. A value that would loosen is ignored, and `curia config doctor` says so. A flag is mine, so it may loosen what a repo file tightened.

**Keys (defaults) and what a repo file may do:**

| Key | Default | Repo file may |
| --- | --- | --- |
| `routing.auto_send`, `routing.auto_threshold` | false, 0.9 | set auto_send false; raise the threshold |
| `routing.aliases`, `editor`, `review.*`, `key_*`, `ntfy.*`, `serve.*` (including `serve.web`), `monitor.*`, `rc.*` | as today, false, true | nothing |
| `tasks.agent_filed.level` | backlog | choose a lower level (ready > backlog > off) |
| `tasks.agent_filed.max_per_day` | 5 | lower |
| `tasks.auto_spawn.enabled` | false | set false |
| `tasks.auto_spawn.max_agents`, `tasks.auto_spawn.max_per_day` | 3, 5 | lower |
| `tasks.auto_spawn.interval` | 5m | lengthen |
| `cost.budget_hard_stop` | false | set true |
| `cost.task_budget_default` | 0 (none) | lower, if nonzero |
| `resources.max_containers_per_session`, `resources.max_containers_total` | 3, 8 | lower |
| `resources.docker_memory_refuse_pct` | 80 | lower |
| `resources.docker_wrapper`, `resources.enabled` | true | nothing |
| `mode.bypass_for` | 2h | lower |
| `mode.away_allow` | five git entries | remove entries |
| `away.prepare_wait` | 10m | nothing |
| `schedule.late_window`, `schedule.reset_grace` | 30m, 1m | lower the window |
| `activity.rules` | table | add rules for this repo |
| `activity.idle_cap`, `activity.task_types` | 30m, list | nothing |
| `context_max`, `default_context_max` | table, 200000 | nothing |

A repo file can never: enable bypass (it is not a key; bypass is only ever typed), enable `routing.auto_send`, lower `routing.auto_threshold`, set `tasks.agent_filed.level = ready`, enable auto-spawn (`tasks.auto_spawn.enabled`), raise `tasks.auto_spawn.max_agents` or `tasks.auto_spawn.max_per_day`, or shorten `tasks.auto_spawn.interval` (F29, Ruling 159), raise any cap or limit above, add a `mode.away_allow` entry, turn `cost.budget_hard_stop` off, enable Remote Control, `serve.web` or any `serve` or `ntfy` key, or set `editor` or `review.*` (an exec path, Ruling 107). Any key added to F19 to F28 later is "nothing" until a ruling says otherwise.

**Agents never change settings.** `curia config set`, `edit`, `rollback` and `doctor` that fixes anything are user-only; the MCP surface has no config tool. In every mode, including bypass, the PreToolUse hook denies Edit, Write, MultiEdit and NotebookEdit on the user file, on `~/.local/share/curia/` and on `~/.config/curia/` (Ruling 157), by deny or exit code 2 only. Because a failing hook fails open, `curia install` also writes real permission deny rules for the same tools on Curia's config and data paths into the user settings file: additive, backed up, and removed by `curia uninstall` like the hooks; a deny rule beats a hook's allow and holds when the hook is down. The hook's deny text is written for the model. A Bash command that writes the file is not caught (README's same-user limit), and the hook's denial is best-effort against a crashed or hung hook (section 7).

**Commands:** `curia config list` (every key, its effective value and the layer it came from), `get <key>`, `set <key> <value>` (validated, written to the user file), `edit` (the `editor` option, Ruling 107; the result is validated and an invalid file is refused and the old one kept), `doctor` (validates both files; reports ignored loosening in a repo file, unknown keys, a missing Docker or `ps`, and out-of-band edits), `history [-n N]`, `rollback <n>`. A settings view in the dashboard (`S`, edits go through the `set` path and its guard) and a read-only `/settings` page (F26).

**History.** Every write by `set`, `edit` or `rollback` appends to `config_revisions`: number, time, actor (`cli`, `dashboard`, `rollback`, `external`), key (or `*`), old and new values and the full file. At each load Curia compares the file's hash with the last revision; a mismatch is recorded as an `external` revision (an edit made outside Curia), takes effect, and shows `C!` in the badge until `curia config doctor` is run at a terminal. `rollback <n>` writes the file as it was after revision `n` as a new revision (no history is deleted) and asks for confirmation.

**Acceptance:** each default above is returned by `config get` with no files; a repo file setting `tasks.agent_filed.level = "ready"` or `tasks.auto_spawn.enabled = true` or `routing.auto_send = true` or `resources.max_containers_total = 99` leaves the effective value unchanged and `doctor` names the key; a repo file setting `cost.budget_hard_stop = true` or lowering `max_per_day` takes effect; a flag overrides a repo value; `set` writes a revision, `rollback 3` restores revision 3's content as revision N+1; a file edited by hand yields an `external` revision; an agent session cannot run `set`; the hook denies an Edit of the user file in every mode.

**Risks:** the policy table is the security rule, so a key forgotten from it must default to "nothing" (it does, above). The user file is plain, so an out-of-band edit by an agent through Bash is detected afterward, not prevented. A tightened repo value can quietly make Curia refuse things in that repo, so `list` shows the layer for each value.

### F29. Auto-spawn

**What:** when I allow it, Curia starts the next ready task by itself, within caps, while the usage forecast and the machine have room. It is off by default and only I can turn it on. (Ruling 159, owner decision D-8: a feature of its own, because work that starts while I am away needs caps and risks of its own.)

**How:**

- **Setting.** `tasks.auto_spawn.enabled` (default false), `tasks.auto_spawn.max_agents` (default 3), `tasks.auto_spawn.max_per_day` (default 5) and `tasks.auto_spawn.interval` (default 5m). They are read from the user file and flags only: a repository's `.curia/config.toml` can set `enabled = false` for its own repository, lower the two caps and lengthen the interval, and can never enable it or raise a cap (F28). `curia auto-spawn on | off | status` is user-only (the F6 guard, Ruling 119) and is `curia config set tasks.auto_spawn.enabled` with a printout of the caps and a revision in `config_revisions`. Agents have no tool for it, and the hook denies their edits to the user file in every mode (F28).
- **Who runs it.** `curia monitor` (F19; section 4), on its own tick of 30 seconds. With no monitor running nothing is auto-spawned, and that is safe: a task simply stays ready. Missed ticks are not made up.
- **Which task.** The next task by the board's order (priority, then age, F5 and F16) that is `ready`, has `ready_hash` equal to the hash of its title and body now, and is in a repository that does not have `tasks.auto_spawn.enabled = false`. An agent-filed task is eligible like any other (owner decision D-7, 2026-11-20): whether I marked it ready or it was filed ready because I set `tasks.agent_filed.level = ready`. `ready_hash` is set when the task becomes ready by me at a terminal (`curia task ready`, or the board key) or by the agent-filed `level = ready` creation, and is cleared when it leaves `ready`. I kept it for two purposes only: a task whose text changed after it became ready (an edit, a re-import from Linear) is held back until it is marked ready again, and a task moved to ready from the web page (F26) has no hash and is never eligible, so a stolen device token cannot start work. It no longer separates who made the task ready. Auto-spawn starts the task in a new session through the normal spawn path (below), never by assigning it to an agent that is already running, and no agent can spawn it (F5). For a task with `source` other than `local` or with `filed_by` set, `curia task ready` first prints the body and waits for Enter, so marking it ready by hand is the moment I read it; a task filed ready is not read first.
- **Checks, in this order,** each tick, stopping at the first that refuses. 1. enabled, and no mode row of any scope (F20) is `bypass`: while bypass is active nothing is auto-spawned, a new agent with no permission prompts and nobody present being the worst combination. 2. live agents (sessions in `working`, `waiting`, `needs_decision` or `idle`, whoever started them) are fewer than `max_agents`. 3. fewer than `max_per_day` auto-spawns in the last 24 hours (`tasks.auto_spawned_at`). 4. the last auto-spawn was at least `interval` ago, because F16's burn rate is a 20-minute window and a new agent has not yet shown in it. 5. F16's forecast has no warning (the limit is not projected before `resets_at`) and no window is at or above `schedule.limited_pct`; the latest sample of a window whose `resets_at` has passed counts as 0. 6. if `resource_samples` are fresh (F19), Docker memory is not above `resources.docker_memory_refuse_pct`; stale or no samples skip this check with a log line, as in F19. Then the candidates in order: a task whose cost is at or above its budget (F16, `budget_usd` or the default) is skipped, and so is one in a repository whose `.curia/setup.sh` is not approved (Rulings 108 and 109), because a script I have not approved never runs unattended; the first task left is spawned.
- **The spawn.** Exactly the `curia spawn T-42` path of F6: same worktree, branch, approved setup, tmux window, preamble, default model (F16) and `docker` wrapper, one task per tick. On success `tasks.auto_spawned_at` is set. A failed spawn changes nothing and is retried after `interval`.
- **Log.** Reused from F22: `schedule_runs` rows with `schedule_id = 'auto'` (not a `schedules` row, so `curia schedule ls` does not list them): outcome `fired` with `results` naming the task and session, or `refused` with `results` naming the check (`bypass`, `max_agents`, `max_per_day`, `interval`, `forecast`, `memory`, `budget`, `setup_unapproved`, `spawn_failed`) and the task. A `refused` row is written only when it differs from the previous `auto` row, so a standing refusal is one line, not one per tick. No eligible task writes nothing.
- **Visibility.** The dashboard header (F3) shows `auto: on 2/3 agents, 1/5 today` or `auto: off`, and, when the last `auto` row is a refusal, `auto: waiting (forecast)`. The task row on the board and the session row show `auto` for a task started this way. `curia auto-spawn status` prints the same plus the last ten `auto` rows. The timeline (F25) already unions `schedule_runs` and tasks, so every start and every refusal is on it.
- **Modes (F20).** `normal` and `edits`: it runs. `away`: it runs, which is the point (work continues while I am gone). The new agent is covered by the `away` row like any session, so its tool calls the hook does not allow are denied, and its questions become `assume` tickets with nobody watching. Its launch preamble gets the F20 away sentence with the time I return. `curia back` lists, under their own heading, every task auto-spawned during the away window with its time and session, and the refusals counted by check. `bypass`: auto-spawn does not run (check 1), including when a repository's own mode is `normal`; a task already running is not stopped.

**Data model:** `tasks.ready_hash` and `tasks.auto_spawned_at` (section 5), and the `schedule_runs` rows above. No new table.

**Acceptance:** with defaults and a ready task, nothing is spawned, and `curia config get tasks.auto_spawn.enabled` is false; a repository file setting `enabled = true`, `max_agents = 9` or a shorter `interval` leaves the effective value unchanged and `doctor` names the key, while `enabled = false` or `max_agents = 1` takes effect; with it enabled and a monitor running, the highest-priority, oldest ready task starts through the same code path as `curia spawn` and carries `auto_spawned_at`; with no monitor nothing starts; a task edited after `task ready` and a task moved to ready from the web page are skipped and the next one starts; an agent-filed task, marked ready at a terminal or created ready by `file_task` at `level = ready`, starts in a new session and is never assigned to a running agent; at `max_agents` live agents, at `max_per_day` starts in 24 hours, within `interval` of the last start, with a forecast warning, with a window at 100%, over the Docker memory threshold with fresh samples, or with any `bypass` row, no task starts and one `refused` row names the check, and a second tick with the same cause writes no second row; with stale samples the memory check is skipped; a task at its budget is skipped and the next one starts; a task in a repository with an unapproved `setup.sh` is skipped and the script never runs; in `away` a task starts and appears in `curia back` under its own heading; an agent session cannot run `curia auto-spawn on`, and the hook denies its Edit of the user file; the dashboard header shows `auto: off` after `curia auto-spawn off`, and a tick right after it starts nothing.

**Risks:** work starts without me and spends usage while I am away. Bounds: it is off until I turn it on, three agents and five starts a day by default, one start per 5 minutes, my own `bypass` stops it, and `curia auto-spawn off` is one command. A forecast can be wrong: it is a 20-minute burn rate, a new agent's cost shows only after its first turns, and under API billing F16 has a projection but no warning, so check 5 passes there and only the caps and the task budget bound spend (check 2 to 4 and the budget skip). The status line is the only source of `rate_limits`, so a stale sample before an idle night may understate the day's burn. Accepted by the owner (D-7): with `tasks.agent_filed.level = ready` and auto-spawn both on, an agent can cause a new agent session to start without my reading the task first, bounded by the per-agent daily filing cap, the auto-spawn caps and the usage forecast. A task body that came from Linear or an agent could carry hostile instructions: a changed body loses eligibility, but a task filed ready is not read first, and I may mark any other without reading it; for imported or agent-filed tasks marked by hand the body is printed first. An auto-spawned agent runs under whatever the mode is: in `away` it can do only what the F20 allow-list lets it and its questions become `assume` tickets I confirm later, and none of that is a sandbox. The hook failing open (a crashed or hung hook; section 7) removes away's denials and the budget stop for that agent, so the unattended agent then has the permission prompts of its normal settings, not Curia's denials; the bound is the hook's own deadline and the `undecided` log line, and the prompts of Claude Code itself still stop an agent with nobody to answer, which is slow rather than unsafe. The spawn checks read the store, not the hook, and do not fail open. `ready_hash` is written only by user-only commands and by my own `level = ready` setting; a same-user process that writes the store directly can forge it (README's same-user limit).

## 7. Safety and trust boundaries

The rule behind all of these: agents can propose and report, but only I can decide, approve or merge, and only through channels agents can't reach.

- **No self-answering.** No MCP tool answers tickets, approves reviews or confirms assumptions. Those actions exist only in `curia decide`, `curia review` and the token-protected `curia serve` endpoint.
- **Text injected into panes is treated as hostile input.** Answers and review notes sent with `send-keys` are passed with the literal flag, stripped of control characters and escape sequences, and capped in length, so a crafted ticket can't smuggle keystrokes.
- **`curia send` targets registered agent panes only.** An agent can't use it to type into my shell, an editor or an SSH session.
- **No auto-accept by default.** Unlike Claude Squad's experimental `--autoyes`, `curia spawn` launches agents with their normal permission settings. A per-repo opt-in can relax this for sandboxed repos.
- **Network surface is opt-in.** `curia serve` is off by default, binds only to the Tailscale interface (amended, Ruling 127: to loopback by default, and to a Tailscale address only when set explicitly), and requires a per-device token (amended, Ruling 129: a one-time token per push, readable by anyone who can read the topic). ntfy messages contain ticket text and that token, so use a self-hosted server or an access-controlled topic; never expose the endpoint with Tailscale Funnel.
- **Hook installs are reversible.** `curia install` backs up `settings.json` first; `curia uninstall` removes only the entries it added.
- **Modes and bypass (F20; amended, Rulings 147 to 149, which touch "only I decide").** A hook may answer a permission check only because I set a mode with a user-only command. Agents never change settings or modes: there is no MCP tool, no config an agent or a repository can write that sets one, and the hook denies agent edits to Curia's config and data. `bypass` needs the word typed on a terminal that passes the user guard, shows red, always expires, and a saved bypass is never restored by `curia back` or by expiry of `away`. It allows only what the user's own deny, ask and managed rules do not forbid, which stay in force.
- **Hook enforcement is best-effort.** When a PreToolUse hook exits 1, crashes or times out (default 600 seconds), Claude Code falls back to its normal prompt (observed on 2.1.329): nothing is silently allowed, but away's denials and the budget hard stop (F16, F20) hold only while the hook runs. The protection of Curia's config (F28) also has real permission deny rules, which hold without the hook. The bound is the hook's own short deadline, after which it prints no opinion and logs `undecided`; a gap is visible afterward, not prevented.
- **Agent-filed tasks (F5; amends Ruling 113).** An agent may add to the backlog, with a reason and a cap. It cannot mark a task ready (unless I chose `level = ready`) or spawn any task it or any agent filed. Auto-spawn may start one that is ready, in a new session. Accepted (D-7): with `level = ready` and auto-spawn both on, an agent can cause a new agent session to start without my reading the task first, bounded by the filing cap, the auto-spawn caps and the usage forecast. Lineage is recorded for display and audit and no permission rests on it.
- **The web view and messages (F26, F18; widens Rulings 127 and 129).** A message that arrives over HTTP is mine only with a device token and a matching Origin, is held until I confirm it, is never auto-sent, and then follows every typing rule. The page is off by default and exposes no transcripts.
- **Auto-spawn (F29; Ruling 159).** Off by default; on only from my user file or `curia auto-spawn on`, never from a repository or an agent. It starts only a ready task whose text is unchanged since it became ready (`ready_hash`; a web-page move leaves none), in a new session, through the same spawn path as `curia spawn` (setup approval included), within caps on live agents, starts per day and spacing, never while `bypass` is active, the forecast warns, a limit is reached, memory is over the threshold or the task's budget is spent. Every start and refusal is logged and shown, and `curia auto-spawn off` stops it at once. The checks read the store and do not fail open; the unattended agent is still bound by the hook, which does (see the item above).
- **Settings layering (F28).** The user file is mine; a repository's `.curia/config.toml` is untrusted data that may only tighten, by a compiled-in per-key table. Anything that gives an agent more power (bypass, auto-send, `agent_filed = ready`, auto-spawn and its caps, higher caps, a longer away allow-list, Remote Control, the network surface, an exec path) cannot come from a repository. Everything is stated in Ruling 156.
- **Typed lines stay bounded.** `/rc` lines and scheduled or broadcast messages are typed only into a pane whose empty prompt was seen, one unconfirmed line per session, and only into registered agent panes.
- **Ticket content is data.** Ticket text shown in the popup, notifications or Obsidian export is never executed or interpreted as commands.

## 8. Build order (V1, then F19 to F29)

Everything ships in V1, but it's built in an order where each step is usable by itself and the next step depends on it. The store comes first because every feature reads it.

1. **Store and hooks** (F1, F2). `curia install`, `curia hook`, schema, transcript parsing. Done when `curia ls` shows correct live state for 5 parallel sessions.
2. **Attention and dashboard** (F4, F3). Status badge, window flags, jump-to-next, popup. From here, `curia` replaces the existing status plugins in my own setup.
3. **Decision queue** (F7, F8). MCP server, ticket validation, `curia decide`, both delivery paths, agent skill. This is the differentiator, so it gets real use as early as possible.
4. **Precedent** (F9). FTS search, the second checkpoint in `request_decision`, reuse tracking. Needs a few weeks of real decisions to tune.
5. **Tasks and spawning** (F5, F6). Task board, worktrees, spawn, pause, resume, agent CLI, then message routing (F18), which needs the backlog and precedent in place.
6. **Review queue** (F10). Depends on tasks and decisions both existing.
7. **Remote and restore** (F11, F12). ntfy, `curia serve`, `curia restore`.
8. **Integrations** (F13). Obsidian export, Linear import, cost-lens, Agent Commons, Codex, then Agent Teams grouping and the escalation chain (F14), then the cost layer: cost-lens ingest, forecasting and cache-aware triage (F15 to F17).

**After V1 (amended, Ruling 161): Plan 6, after Plan 5 merges.** F19 to F29 are specified and not built. V1's order above does not change. Within Plan 6 each step is usable alone and depends on the ones before it:

9. **Settings and the monitor** (F28, then `curia monitor`). The config layering with its policy table, history and the hook's denial of agent writes come first, because every later feature adds keys to it.
10. **Modes and away** (F20), then **resources** (F19) and **activity time** (F27), which both read the monitor's process samples, then the budget stop (F16's amendment) and agent-filed tasks with `ready_hash` (F5's amendment), then **auto-spawn** (F29), which needs the monitor, modes, resources, the forecast and budget, and the F5 amendment all in place.
11. **Routing extensions and schedules** (F18's broadcasts, F22), then **Remote Control** (F21).
12. **Views** (F23, F24, F25), then the **web view** (F26) last, since it widens the network surface and needs every page's data to exist.

**Language:** Go. Hooks run on every tool call, so a compiled binary with near-zero startup matters more than development speed, and Bubble Tea is the strongest TUI library for the popup. Claude Squad and ccmonitor are both Go, so their patterns translate directly. Python would work if startup stays under about 50 ms, but that's hard with imports.

## 9. Open questions

- Can Claude Code's built-in ask-the-user tool be intercepted with a PreToolUse hook and mirrored into the queue, instead of relying only on agents calling `request_decision`?
- Is FTS5 good enough for precedent matching, or do decisions need embeddings? Decide after about 50 real decisions.
- What's the right default expiry for `assume` tickets: 24 hours, or tied to task completion only?
- Should `curia` be AGPL-3.0, which would permit copying code from Claude Squad and cmux, or MIT, which keeps it pattern-only borrowing?
- Does the Stop-hook delivery path work reliably when an agent has subagents running?
- Does a separate sidebar pane earn its screen space, or is the popup plus status badge enough?
- Which agent time counts toward `waiting on me` when one person-hour of attention serves several agents at once? F27 counts each session's wait in full, so the share can exceed what one person could have saved.
- Should the web settings view (F26, F28) become editable later? It is read-only now (D-9).
- Spawned branches are named `curia/T-<n>-<slug>`, so F27's task type from the branch prefix is `untyped` for every spawned task. Is a type tag on the task wanted?
- Are 5 filed tasks per agent per day (F5) and 3 per session and 8 in total containers (F19) the right defaults? Both are first guesses (Rulings 141 and 146). The same holds for F29's 3 live agents, 5 starts a day and 5 minutes between starts (Ruling 159).

**Conflicts in the 2026-11-19 request, answered by the owner on 2026-11-20.** Written for the safer reading and confirmed:

- **F26 is "read-only" yet accepts drag, messages and a settings view.** Read as: read-only except task moves between backlog, ready and dropped and confirmed messages, both behind a device token. The settings view on the web page is read-only (F28 says "a settings view" without saying editable); editing from a phone would let a stolen token change caps and `auto_send`. (D-9.)
- **F5 "only the owner marks backlog tasks ready" against `level = ready`.** At `ready` the agent's task is created ready by my standing setting. To keep Ruling 113's intent, an agent can still not spawn any agent-filed task (Ruling 141), and auto-spawn (F29) may start one that is ready, in a new session (D-7.)
- **F21 asks for `/rc` typed into panes, and typing needs a confirmable line.** A slash command carries no message id, so the confirmation rule differs for it (Ruling 150). (D-10; D-6 settled the bypass wording in F20.)

**Conflicts in the 2026-11-19 request, still open.** Written for the safer reading; the owner should confirm.

1. **F20 away "wait for ready" has no signal.** There is no MCP tool for it in the request. Ready means the agent's turn ended after the message was confirmed (Ruling 148). An agent that never ends its turn is waited on for 10 minutes and then skipped.
2. **F22 schedules and F20 bypass or away together.** A scheduled message types into panes unattended under whatever mode is set. Nothing in the request forbids it, so the spec allows it and says so in both Risks sections.

**Third-party facts, checked against the documentation on 2026-11-19** (code.claude.com/docs, docs.docker.com, github.com/paperclipai/paperclip).

Confirmed from documentation:

- PreToolUse output (`permissionDecision` allow, deny, ask, defer; reason; precedence deny over defer over ask over allow; exit 2 blocks): hooks page.
- A hook's `allow` does not override a deny rule, an ask rule or managed settings; in `plan` mode it does not approve edits: permissions page, permission-modes page.
- PreToolUse fires for every tool but `EndConversation`, MCP tools and subagent calls included: hooks page.
- `--dangerously-skip-permissions` works with `--resume` and must be passed again, since bypass is not restored; refused as root; managed policy can disable it: sessions, permission-modes and cli-reference pages.
- `/remote-control` and `/rc` with a name argument; `remoteControlAtStartup` in the user settings file, ignored as `true` in project settings: remote-control page.
- Status-line fields used by F2, F3 and F16 (`context_window`, `rate_limits`, `cost`, `prompt_cache`): statusline page.
- `run_in_background` on Bash and `background_tasks` on Stop: hooks page.
- `agent_id` and `agent_type` in hook payloads: hooks page.
- `docker stats` memory fields and the Compose project and service labels: Docker CLI and Compose pages; the working-directory label: Compose source only.
- Paperclip's LICENSE file is MIT; its README states budget hard stops and routines with run history: repository.

Contradicted, and corrected above:

- `/rc` is documented, not undocumented (F21).
- A repeated `/rc` is not harmless: it opens a status panel (CLI) or disconnects (Desktop), so `--check` no longer re-types it (F21).
- The install key is `remoteControlAtStartup` in `~/.claude/settings.json`, and an explicit `false` is the user's choice (F21).
- Bypass does not mean `allow` for every call: deny, ask and managed rules and some prompts remain (F20).
- `--resume` drops bypass, so `--native` passes the flag again (F20).
- A hook timeout or failure does not block the call (it falls back to the normal prompt), so hook-enforced rules are best-effort (F16, F20, F28).
- The `docker` wrapper does not label `docker compose up` containers; Compose's labels attribute them (F19).
- Hook payloads give no link from an agent to a parent, so lineage cannot carry permission (F5).

Observed on 2.1.329 (2026-11-20), isolated configuration:

- Hook precedence: a deny rule and an ask rule beat a hook `allow`; in `plan` mode an `allow` passes neither an Edit nor ExitPlanMode (F20).
- Hook `deny` and exit code 2 block and the reason reaches the model; exit 1 and a timeout fail open to the normal prompt (F16, F20, F28).
- `--dangerously-skip-permissions` with `--resume` keeps the session id; the first use shows a blocking acceptance dialog; a plain resume returns to the default (auto) mode (F20).
- Built-in slash commands (`/status`, `/usage`) and unknown commands fire no UserPromptSubmit; custom commands do; task notifications and agent messages do too (F18, F21, F22, F27).
- Background Bash: `run_in_background` in the input, a background task id in the PostToolUse response, `background_tasks` on Stop, completion as a task-notification prompt (F27).
- Status line: the context percentage is null before the first response; `rate_limits` and `prompt_cache` appear only after it; no field marks a limit stop (F2, F22).
- A subagent's calls carry an agent id and type and the parent's session id (F5).
- Docker Desktop: `docker info` `.MemTotal` is the VM's total with no current-use field; Compose containers carry the project, working-directory and config-file labels and never `curia.session`; the wrapper sees only the `compose` call (F19).
- `--remote-control [name]` is a flag of this version's command line (`claude --help`, F21).

Still open (owner-run, or not yet run):

- Remote Control: `/rc <name>` in a temporary config, with the status-line JSON diffed before and after, whether `/rc` fires UserPromptSubmit (inferred not to), what a second `/rc` shows, whether a one-time dialog appears, and whether `--remote-control [name]` suits launched sessions (F21).
- A real usage-limit stop: a logging hook for `StopFailure` and `Notification`, read when a session next hits the limit (F22).
- MCP tool calls and teammate payloads in hooks: whether a teammate's own hooks carry a team name or any link to its lead (F5, F14; display only).
- The `updatedInput` and `defer` hook decisions.
- Paperclip needs none.

All of this is specific to Claude Code 2.1.329 and Docker Desktop engine 29.2.1 with Compose 5.1.0. The captured payloads were kept outside the repository by the probe run; the redacted copies used as test data live beside the tests that use them (`testdata/` directories). These observations must be re-probed on a new Claude Code version.
