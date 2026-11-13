# Curia: V1 Feature Spec

Nov 11, 2026 · @Zachariah Farahany

## TLDR

V1 is a single binary, `curia`, that turns tmux into a control plane for parallel coding agents. Claude Code hooks write every session event into one local SQLite store. Everything else is a view or an action over that store: a session dashboard, attention signals, a task board, worktree-isolated agent spawning, a review queue, and the core differentiator, a decision queue with a searchable precedent log that agents consult before asking again. Session tracking, worktrees and approval queues already exist in open source, so V1 borrows their patterns (and code, where the license allows) and spends original effort on decisions, precedent and the task-to-merge lifecycle. Coordination inside a single job is left to Claude Code's Agent Teams; Curia treats each team as one unit to watch, route and cost.

## Contents

1. Problem
2. Goals and non-goals
3. Build vs borrow
4. Architecture
5. Data model
6. Feature specs (F1 to F18)
7. Safety and trust boundaries
8. Build order within V1
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

`curia` is one binary with subcommands, around one SQLite store. There is no long-running daemon in the core path: hooks write, UIs read on demand, and only the optional `curia serve` endpoint stays up.

&#91;embedded content: Curia architecture · 9 components around one store\]

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

The `sessions.state` value `needs_decision` is distinct from `waiting` on purpose: `waiting` means Claude Code's own permission or input prompt, `needs_decision` means an open ticket in the queue. The dashboard ranks them differently (section 6, F4).

## 6. Feature specs

Eighteen features, grouped by the four jobs from section 1 plus a cost layer: see who needs me (F1 to F4), start work (F5, F6), resolve judgments (F7 to F9), finish and sustain work (F10 to F14), and spend wisely (F15 to F17). F18 routes my own messages down to the right agent.

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

**How:** model and token usage come from the transcript (JSONL at `transcript_path`). Context used = the latest assistant message's input tokens including cache reads and writes. Context max comes from a per-model table in config, so new models need one config line. Cost comes from cost-lens when installed (F15), otherwise from the cost field in Claude Code's status-line JSON. Curia keeps no price table of its own. Parsing is incremental: `curia` stores a byte offset per transcript and reads only new lines.

**Display:** context as a percentage with color thresholds (green under 60%, yellow 60 to 85%, red above 85%, since compaction quality drops near the limit).

### F3. Dashboard

**What:** a popup listing every session, opened with `prefix + a` (configurable) via `tmux display-popup`, built as a TUI (Bubble Tea).

**Columns:** state icon, session:window, task id and title, repo@branch, model, context %, subagents, open decisions count, cost, last update, age of last update.

**Sort order:** needs\_decision, then waiting, then review-ready, then working, then idle. Within a group, oldest first.

**Keys and mouse:** Enter or click jumps (`switch-client -t <pane>`), `d` opens that session's decisions, `r` opens its review, `t` toggles a task-board view (F5), `n` spawns a new agent (F6), `x` kills with confirmation, `/` filters. Mouse clicks work when tmux `mouse on` is set.

**Second surface:** an optional always-on sidebar pane (`curia sidebar`), same data, narrower columns, for a large monitor.

### F4. Attention signals

**What:** I know something needs me without opening anything.

- **Status bar badge:** `#(curia status)` in status-right shows counts, e.g. `D2 Q1 W1 R1` (decisions, answers not yet delivered, waiting, reviews). Hidden when all zero. (amended, Ruling 91: the format is `D<n> Q<n> W<n> R<n>`, Q = answers not yet delivered)
- **Window flags:** window names get a colored marker: red for needs\_decision or waiting, blue for review-ready, none otherwise. Cleared when I focus the pane (`pane-focus-in` hook marks it seen).
- **Jump to next:** `prefix + N` jumps to the highest-priority pane in the F3 sort order.
- **Optional bell and macOS notification** when a blocking decision opens, rate-limited to one per 30 seconds.

### F5. Task board

**What:** a local backlog where each task is the unit an agent works on.

**Statuses:** backlog, ready, running, needs\_decision, review, done, dropped. Only backlog, ready and dropped are set by me; the rest are derived from session and review state, so the board never goes stale.

**Commands:** `curia task add "title" --repo . [--body file.md]`, `curia task ls`, `curia task edit T-42`, `curia task ready T-42`. The task body is the starting brief given to the agent, so writing a good task is writing a good prompt.

**Views:** a kanban view inside the F3 popup (`t`), and `curia task ls` for scripting.

**Sources:** local tasks by default. A Linear adapter imports issues as tasks, read-only: `curia` never writes Linear statuses back. A Backlog.md adapter reads markdown tasks from a repo. Adapters only map fields; status derivation is always local.

### F6. Spawning, worktrees and the agent CLI

**Spawn:** `curia spawn T-42 [--agent claude] [--model opus]` does, in order:

1. Create a worktree at `<repo>/../.curia-worktrees/<repo>-T-42` on branch `curia/T-42-<slug>` from the task's base branch.
2. Run the repo's setup script if one exists (`.curia/setup.sh`: install deps, copy `.env`).
3. Open a tmux window named `T-42` in a session named after the repo.
4. Launch the agent with the task body plus a standard preamble (how to use decision tools, how to signal completion).
5. Link session to task; task becomes `running`.

**Native worktrees first:** for Claude Code, steps 1 and 3 wrap Claude Code's own worktree sessions (`claude -w <name> --tmux`) instead of Curia running git and tmux itself. Curia keeps its own worktree path only for Codex. `--team` adds an Agent Teams lead (F14).

**Lifecycle commands:** `curia pause` (commit WIP, remove worktree, keep branch), `curia resume` (recreate worktree, `claude --resume <session>`), `curia clean` (remove worktree and branch after merge). Pattern from Claude Squad.

**Agent-facing CLI:** `curia ls --json`, `curia read <pane> [--lines 50]` (capture-pane), `curia send <pane> "text"`, `curia spawn`. Agents can call these, which is what the operator skill (F14) builds on. `send` only targets panes registered as agent sessions.

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

**Actions:** `o` opens the full diff in my configured tool (lazygit, delta, or nvim), `a` approves (merge to base, or open a PR with `gh pr create` per config), `c` requests changes (my note is delivered to the agent as in F7, task returns to `running`), `x` drops the task.

### F11. Notifications and remote access

**What:** I can leave my desk without agents stalling.

- **Push:** ntfy (self-hosted or ntfy.sh with a private topic) on new blocking tickets and new reviews. The message includes ticket id, question, options and recommendation. Assume-mode tickets don't push, by default.
- **Answer from phone, minimum path:** SSH (over Tailscale) from a phone terminal app and run `curia decide`. The popup works at phone width.
- **Answer from phone, convenient path:** ntfy action buttons hit `curia serve` (a small HTTP endpoint bound to the Tailscale interface only) with option keys. Requires a per-device token. Off by default.
- **Quiet hours:** no pushes in a configured window; blocking tickets queue until morning.

### F12. Persistence and restore

**What:** a reboot or tmux crash costs minutes, not context.

**How:** the store already has every session's pane, worktree, branch, agent and session id. `curia restore` recreates tmux sessions and windows, then relaunches each non-ended agent with `claude --resume <session_id>` in its worktree. Works alongside tmux-resurrect but does not require it.

**Teams:** Agent Teams teammates cannot resume, so restore relaunches only each team's lead with its session id and tells it to respawn its team from the shared task list.

### F13. Integrations

- **Obsidian:** each answered decision is exported as a note under a configured vault folder (`Decisions/D-117 Postgres for events.md`) with frontmatter (id, repo, task, scope, status, date) and the question, options, answer and reasoning in the body. One-way export; the store stays the source of truth.
- **Linear:** read-only import of issues into tasks (F5). Never writes statuses.
- **cost-lens:** if installed, it supplies per-session cost to F2.
- **Agent Commons:** decisions with `global` scope can be published as entries, so precedent is shared beyond `curia`.
- **Codex:** same hook adapter keyed by agent type, since Codex now exposes compatible lifecycle hooks; MCP tools work unchanged.

### F14. Agent Teams as first-class units

**What:** Curia does not build its own orchestrator. Coordination inside one job is left to Claude Code's [Agent Teams](https://code.claude.com/docs/en/agent-teams), where a lead session coordinates teammate sessions. Curia shows teams, routes their decisions and tracks their cost, the layer Agent Teams does not cover.

**Why this split:** Agent Teams works at the level of one job (one lead, one codebase, short-lived). Curia works at the level of the whole portfolio (many independent tasks across repos and days, plus decisions, precedent, cost and review). A Curia executor agent would duplicate the team lead and compete with an experimental Anthropic feature that will keep moving.

**Detection:** teammates are ordinary Claude Code instances, so F1's hooks fire for each. Curia reads team membership from the team config Claude Code maintains, where the lead's entry carries the agent type `team-lead`, and stores it as two new `sessions` columns: `team_id` and `team_role` (lead, teammate, or empty for solo sessions).

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

**Pull (history):** for per-branch and per-task cost in reviews (F10), Curia runs the cost-lens analyzer CLI and reads its JSON output.

**Without cost-lens:** Curia registers its own status-line command, which forwards `rate_limits` and cost from Claude Code's status-line JSON into the same ingest path. Cache state is then unknown and shown as blank.

**Contract ownership:** the JSON schema lives in Curia's repo as `docs/ingest-schema.json`; cost-lens adds the push behind a setting, off unless Curia is on `PATH`.

**Risk:** cost-lens is built and tested on macOS (its index lives under `~/Library/Caches`). Remote Linux use needs a Linux cache path in cost-lens first.

### F16. Usage forecasting and budget-aware scheduling

**What:** Curia projects when I'll run out and acts on it, instead of only showing a percentage.

**Data:** every ingest of `rate_limits` appends a row to a `usage_samples` table (`at`, `window`, `used_pct`, `resets_at`). The windows are account-wide, so samples from different sessions are merged into one series per window.

**Subscription math:** burn rate = change in `used_pct` over the trailing 20 minutes. Time to limit = (100 − `used_pct`) / burn rate. If the limit lands before `resets_at`, the status badge shows a warning, e.g. `5h 62% → limit ~48m (resets 1h50m)`.

**API math:** with no `rate_limits`, projected spend over the next 2 hours = the trailing 20-minute dollar burn rate across live sessions, extrapolated.

**Attribution:** per-session token deltas show which agent is eating the window; the dashboard sorts by burn on request.

**Acting on it:** tasks gain a `priority` field (P1 to P3, default P2). When the forecast says I'll hit the limit before reset, Curia (1) suggests pausing the lowest-priority running task, with one key to accept, and (2) defaults new spawns to the cheaper model, using cost-lens's model advisor where available. It never pauses anything on its own.

### F17. Cache-aware decision triage

**What:** an agent blocked on a decision is idle and its prompt cache is expiring. Answering after expiry makes its next turn re-write the whole context, so decision latency costs money.

**Queue order:** blocking tickets sort by the asking session's `cache_expires_at`, soonest first, then by age. Tickets from sessions with unknown cache state sort by age after them.

**Ticket display:** each ticket shows the stakes, e.g. `answer within 12m to keep cache warm (rebuild ≈ $1.68)`. The same line goes into the ntfy push (F11). Once the cache is cold, the line changes to `cache cold: next turn re-writes ~84k tokens`.

**Dashboard:** a cache column for every idle or waiting session: time left, or cold with the rebuild cost.

### F18. Message routing

**What:** the inbound mirror of the decision queue. Agents send judgment calls up to me (F7); this sends my thoughts down to them. I type a message once, in plain language, and Curia works out which agent, task or repo it belongs to and how it should land.

**Entry points:** `curia say "..."` from any shell (including SSH from my phone), and `prefix + m` in tmux, which opens a one-line prompt in a popup.

**Routing, in order:**

1. **Explicit address.** A message starting with `@<repo>`, `@<alias>` or a task id (`T-42:`) routes deterministically against repo names, branches, task ids and aliases. No model call.
2. **Haiku suggestion.** Otherwise Curia calls Haiku through `claude -p --model haiku --output-format json`, so it runs on my existing Claude login with no separate API key. The prompt holds my message plus a compact list of live sessions and open tasks (id, repo, branch, task title, latest update). Haiku returns `{target, kind, confidence, runner_up}`. Timeout: 5 seconds; on timeout or error I pick the target by hand.

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

**Data:** a `messages` table: `id`, `text`, `kind`, `target_session`, `target_task`, `target_repo`, `suggested_target`, `confidence`, `routed_by` (address, confirmed, overridden, auto), `status` (held, delivered, undone, cancelled), `created_at`, `delivered_at`. Labels for the accuracy gate come from `routed_by`.

**Safety:** routed text goes through the same sanitizing path as decision answers (section 7). The list Haiku sees contains only session metadata, never transcript content.

## 7. Safety and trust boundaries

The rule behind all of these: agents can propose and report, but only I can decide, approve or merge, and only through channels agents can't reach.

- **No self-answering.** No MCP tool answers tickets, approves reviews or confirms assumptions. Those actions exist only in `curia decide`, `curia review` and the token-protected `curia serve` endpoint.
- **Text injected into panes is treated as hostile input.** Answers and review notes sent with `send-keys` are passed with the literal flag, stripped of control characters and escape sequences, and capped in length, so a crafted ticket can't smuggle keystrokes.
- **`curia send` targets registered agent panes only.** An agent can't use it to type into my shell, an editor or an SSH session.
- **No auto-accept by default.** Unlike Claude Squad's experimental `--autoyes`, `curia spawn` launches agents with their normal permission settings. A per-repo opt-in can relax this for sandboxed repos.
- **Network surface is opt-in.** `curia serve` is off by default, binds only to the Tailscale interface, and requires a per-device token. ntfy messages contain ticket text, so use a private topic or a self-hosted server.
- **Hook installs are reversible.** `curia install` backs up `settings.json` first; `curia uninstall` removes only the entries it added.
- **Ticket content is data.** Ticket text shown in the popup, notifications or Obsidian export is never executed or interpreted as commands.

## 8. Build order within V1

Everything ships in V1, but it's built in an order where each step is usable by itself and the next step depends on it. The store comes first because every feature reads it.

1. **Store and hooks** (F1, F2). `curia install`, `curia hook`, schema, transcript parsing. Done when `curia ls` shows correct live state for 5 parallel sessions.
2. **Attention and dashboard** (F4, F3). Status badge, window flags, jump-to-next, popup. From here, `curia` replaces the existing status plugins in my own setup.
3. **Decision queue** (F7, F8). MCP server, ticket validation, `curia decide`, both delivery paths, agent skill. This is the differentiator, so it gets real use as early as possible.
4. **Precedent** (F9). FTS search, the second checkpoint in `request_decision`, reuse tracking. Needs a few weeks of real decisions to tune.
5. **Tasks and spawning** (F5, F6). Task board, worktrees, spawn, pause, resume, agent CLI, then message routing (F18), which needs the backlog and precedent in place.
6. **Review queue** (F10). Depends on tasks and decisions both existing.
7. **Remote and restore** (F11, F12). ntfy, `curia serve`, `curia restore`.
8. **Integrations** (F13). Obsidian export, Linear import, cost-lens, Agent Commons, Codex, then Agent Teams grouping and the escalation chain (F14), then the cost layer: cost-lens ingest, forecasting and cache-aware triage (F15 to F17).

**Language:** Go. Hooks run on every tool call, so a compiled binary with near-zero startup matters more than development speed, and Bubble Tea is the strongest TUI library for the popup. Claude Squad and ccmonitor are both Go, so their patterns translate directly. Python would work if startup stays under about 50 ms, but that's hard with imports.

## 9. Open questions

- Can Claude Code's built-in ask-the-user tool be intercepted with a PreToolUse hook and mirrored into the queue, instead of relying only on agents calling `request_decision`?
- Is FTS5 good enough for precedent matching, or do decisions need embeddings? Decide after about 50 real decisions.
- What's the right default expiry for `assume` tickets: 24 hours, or tied to task completion only?
- Should `curia` be AGPL-3.0, which would permit copying code from Claude Squad and cmux, or MIT, which keeps it pattern-only borrowing?
- Does the Stop-hook delivery path work reliably when an agent has subagents running?
- Does a separate sidebar pane earn its screen space, or is the popup plus status badge enough?
