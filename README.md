# Curia

Curia turns tmux into a control plane for several Claude Code sessions running at once. Claude Code hooks record every session event in one local SQLite database, and everything else is a view over it: a status-bar badge, window markers, a dashboard popup, and a key that jumps to the session that needs you. Agents can also raise decision tickets through an MCP server, with a recommendation, and search the answers you gave before; you answer from a popup or the command line. You can keep a backlog of tasks, start an agent on one in its own git worktree, and send a message to a running agent in plain language. When an agent says a task is finished, the work waits in a review queue where you merge it, ask for changes or drop it; a push can tell your phone when a decision or a review is waiting; and `curia restore` brings sessions back after a reboot or a tmux crash. It is one Go binary with no daemon and no cgo.

## Requirements

- tmux 3.3 or newer.
- [Claude Code](https://claude.com/claude-code), installed separately.
- macOS or Linux. Windows is not supported.

## Install

Homebrew (also installs tmux):

```sh
brew install zachfara/tap/curia
```

Without Homebrew, the install script downloads the release archive for your OS and architecture, checks its SHA-256 against the release's `checksums.txt` and puts `curia` in `~/.local/bin`:

```sh
curl -fsSL https://raw.githubusercontent.com/ZachFara/curia/main/scripts/install.sh | sh
```

`CURIA_VERSION=v0.1.0` installs a specific release and `CURIA_INSTALL_DIR=/some/dir` installs somewhere else. The script never uses sudo. If the directory is not on your `PATH` it tells you what to add.

With Go 1.26 or newer:

```sh
go install github.com/ZachFara/curia/cmd/curia@latest
```

This puts the binary in `$(go env GOBIN)`, or `$(go env GOPATH)/bin` (usually `~/go/bin`) if `GOBIN` is not set. That directory is not on your `PATH` by default, so add it, for example `export PATH="$HOME/go/bin:$PATH"`.

Check the install with `curia --version`.

Then register Curia with Claude Code and tmux:

```sh
curia install
```

## What `curia install` changes

It edits these places on your machine, keeps whatever you already have there, and is safe to run twice:

- **Claude Code hooks.** One hook entry for each of eight events (`SessionStart`, `UserPromptSubmit`, `PreToolUse`, `PostToolUse`, `Notification`, `Stop`, `SubagentStop`, `SessionEnd`) in `~/.claude/settings.json`, or in `$CLAUDE_CONFIG_DIR/settings.json` when that variable is set. Each runs `curia hook <Event>`. Before rewriting an existing settings file it copies it to `settings.json.bak.<unix time>`.
- **Permission rules.** The six Curia MCP tools (`mcp__curia__search_decisions`, `mcp__curia__request_decision`, `mcp__curia__check_decision`, `mcp__curia__withdraw_decision`, `mcp__curia__check_messages`, `mcp__curia__complete_task`) are added to `permissions.allow` in the same settings file, so agents can use them without a prompt. `curia install --no-allow-mcp-tools` skips this step.
- **Status line.** `statusLine` in the same settings file is set to `<path to curia> statusline`, only when you have no `statusLine` of your own; yours is never replaced, and install prints the command that would feed Curia from it (it reads the status-line JSON once and passes it to `curia ingest --statusline`, then to your command, in plain POSIX `sh`). `curia install --no-statusline` skips this step and prints nothing about it beyond a note. Without the status line, cost and usage numbers are missing. Uninstall removes it only if it is Curia's.
- **MCP server.** Registered with `claude mcp add --scope user curia -- <path to curia> mcp`. If `claude` is not on your `PATH`, install prints that command for you to run. A `curia` entry that is not this binary's is left alone.
- **Skill.** `~/.claude/skills/curia-decisions/SKILL.md` (under `$CLAUDE_CONFIG_DIR` if set) tells agents when and how to raise a decision ticket and how to finish a task. A `SKILL.md` there that is not Curia's is left alone.
- **tmux.** A snippet at `~/.config/curia/curia.tmux`, loaded from `~/.tmux.conf` by a block between the lines `# >>> curia >>>` and `# <<< curia <<<`. Curia edits nothing in `~/.tmux.conf` outside that block. The snippet turns on `focus-events`, sets `status-interval` to 2 and raises `status-right-length` to at least 80, appends the badge to `status-right`, adds a red dot to a window's tab when its session needs you, binds four keys and sets a `pane-focus-in` hook that marks a pane's sessions as seen. Curia adds to your window tab formats instead of replacing them, and it does not take over a key you have already bound. The four keys are configurable (`key_dashboard`, `key_decisions`, `key_next` and `key_message` in the config file). `curia install` does not load the snippet into a running tmux: run `tmux source-file ~/.tmux.conf` afterwards, or restart tmux.

Restart running Claude Code sessions afterwards so they pick up the hooks, skill and MCP server.

`curia uninstall` removes what install added: the hook entries, the six permission rules, the MCP server, the skill, the status line if it is Curia's, the tmux block and the snippet file. In each running tmux server it can reach it also undoes what Curia's snippet loaded (the pane-focus-in hook, Curia's own key bindings and its part of the status line), one line per change, touching only values that are Curia's (it leaves the `status-interval` and `status-right-length` the snippet changed as they are, and says so; known and deferred); with no server to reach it prints the commands to run by hand. Curia's database in `~/.local/share/curia/` is not deleted.

The hook, MCP and tmux entries record the path you would type to run `curia`: the `curia` found on `PATH` when it is the running binary (with Homebrew, the stable symlink `/opt/homebrew/bin/curia`, not the versioned path under `Cellar`), otherwise the path the binary was started by. `curia install` prints the path it recorded, and warns when that path is versioned or not on `PATH`, because the entries break if that file moves; put `curia` on `PATH` and run `curia install` again to fix that.

A `brew upgrade curia` needs nothing else. If you installed an older release, which recorded the `Cellar` path, run `curia install` once after upgrading: it replaces those entries with the stable path, and `curia uninstall` removes them either way.

## Keys and commands

With the default tmux keys (after your tmux prefix):

| Key | Action |
| --- | --- |
| `a` | Dashboard popup (`curia ui`) |
| `A` | Decision queue popup (`curia decide`) |
| `N` | Jump to the session that needs you most (`curia next`) |
| `m` | Message popup (`curia say`): type a message once and Curia works out which agent, task or repository it is for |

tmux binds `m` to mark-pane by default. Curia binds `m` only if it is free or still tmux's default; if you have bound it yourself, install leaves your binding and says so, and you pick another key with `key_message`.

In the dashboard, `j`/`k` or the arrows move, Enter or a click jumps to the session's pane, `d` opens that session's decisions, `r` opens the review of the row's task, `t` shows the task board (`D` there shows or hides done tasks), `/` filters, `x` then `y` kills the pane, and `q` or Esc quits. A team of agents shows as one row (the worst state of its members) that Space expands; Enter on it jumps to the member that needs you most. On the board, `j`/`k` move between and across the status columns, `r` toggles a task between backlog and ready, and `n` then `y` spawns the selected backlog or ready task; the `y` is ignored until the screen has been still for a moment, so a held or pasted key starts nothing.

| Command | What it does |
| --- | --- |
| `curia ls` | List sessions, the one that most needs you first (`--json` for scripts) |
| `curia next` | Switch to the session that needs you most |
| `curia ui` | The dashboard, meant for a tmux popup |
| `curia sidebar` | The dashboard with narrow columns, for an always-open pane |
| `curia decide` | Answer decision tickets: with no argument the queue, with an id and `--option`, `--accept` or `--text` a script-friendly answer |
| `curia decisions ls` | List decision tickets, answered ones included |
| `curia decisions edit` | Rescope or supersede an answered ticket |
| `curia task add`, `ls`, `edit`, `ready`, `import` | Keep tasks: `add "title" --repo . --body file.md --priority P1`; `ls` (`--json`); `edit` by flags or in your editor (also `--priority`); `ready` marks a task spawnable; `import --backlog-md [dir]` reads a Backlog.md directory, `import --linear` your Linear issues (see Cost, limits and integrations) |
| `curia spawn <id>` | Start an agent on a task (see below); `--agent codex` starts Codex instead of Claude Code (support is partial, see below) |
| `curia pause <id>`, `resume <id>`, `clean <id>` | Stop, restart and finish with a task's worktree (see below) |
| `curia say` | Send a message, make it a standing rule or a task |
| `curia send <pane> <text>`, `curia read <pane>` | For agents and scripts: send a message to another agent's pane, read the tail of its screen |
| `curia messages ls`, `cancel <id>`; `curia routing stats`, `reset` | Look at and cancel messages; see and reset the router's accuracy numbers |
| `curia setup approve <id>` | Approve a repository's setup script (see below) |
| `curia review [<T-id>\|<R-id>]` | The review queue: the view, or `--approve`, `--request-changes --note`, `--drop`, `--json` (see Reviews below) |
| `curia reviews ls` | List reviews (`--status <status>` (default pending, or all), `--json`); read-only, agents may run it |
| `curia restore` | Bring back sessions lost to a reboot or a tmux crash (see below) |
| `curia serve` | The endpoint phone buttons answer tickets through (see below) |
| `curia usage` | Usage windows, the limit forecast and which sessions burn it (`--json` for scripts); see Cost and limits below |
| `curia ingest`, `curia statusline` | Read cost, cache and usage numbers (from cost-lens, or from Claude Code's status line); they print nothing you need to run by hand |
| `curia export obsidian [<id>...]`, `export commons <id>` | Write decision notes to an Obsidian folder; print an Agent Commons entry (see Cost, limits and integrations below) |
| `curia status` | The badge tmux shows: `D<n> Q<n> W<n> R<n>`, for open blocking tickets, answers a live session has not yet received, waiting sessions and reviews |

## How decisions work and what the safety limits are

An agent that hits a judgment call searches earlier decisions through the MCP tools, then raises a ticket with two to four options and a recommendation. A ticket is either blocking (the agent finishes safe work and stops) or an assumption (the agent keeps going on its recommendation and you can override it later). The MCP server has no tool that answers a ticket; answers are written only by `curia decide`, which refuses to run inside an agent session.

An answer reaches the agent at most once, by the first of three paths:

1. `curia decide` types it into the agent's pane, but only when a screen capture shows Claude Code's empty input prompt. A draft, a dialog, a menu, a shell, a running turn, background subagents, a pane shared by two sessions or a tmux window with `synchronize-panes` on all stop it from typing. If Claude Code's prompt looks different from the versions Curia recognises, typing turns itself off.
2. The agent's next `Stop` hook blocks the stop and hands over the answer.
3. The agent calls `check_decision`.

Typed text is a fixed sentence beginning "Decision D-<n>" with a restricted character set, so it cannot start a slash command, and in a shell it is a failed command, not an executed one.

Claude Code labels a delivery by the Stop hook "Stop hook reported a problem" (2.1.330; earlier versions say "Stop hook error", and may show "Stop hook error occurred" in the footer for a few seconds) above Curia's text. This label appears on a SUCCESSFUL delivery: it is how Claude Code shows any Stop hook that asks the agent to continue, not a failure.

These are guards, not a security boundary. An agent that has a shell is not prevented from running `curia decide` itself, or from writing to the SQLite file; the refusal inside an agent session is a check on the environment, not a lock. Curia reads Claude Code's screen to decide whether it may type, so a Claude Code update can switch typing off until Curia is updated; answers still arrive through the second and third paths.

## Tasks, spawning and worktrees

A task is a row in Curia's own database with a title, a brief (its body), a repository and a base branch. You set only three statuses: backlog, ready and dropped; running, needs a decision and the rest are worked out from the sessions linked to the task, so the board does not go stale. `curia task edit` opens an editor only when the `editor` option is set in `~/.config/curia/config.toml` (for example `editor = "nvim"`: a program and its arguments split on spaces, run without a shell, with the file path added last); `$EDITOR` and `$VISUAL` are not read, and every field can also be changed with flags.

`curia spawn T-1` creates a git worktree for the task on a branch `curia/T-1-<slug>` beside the repository (`<repo>/../.curia-worktrees/<repo>-T-1`), opens a tmux window named `T-1` in a session named after the repository, and starts Claude Code there with the task's brief as its first prompt. The brief reaches `claude` as one argument, never through tmux's or a shell's command parser. `--team` lets the agent lead an Agent Teams team, `--model` picks the model. An agent can only spawn a task you have marked ready.

**Setup scripts.** If the repository has `.curia/setup.sh`, spawn runs it in the new worktree only if you have approved that exact content for that repository: `curia setup approve T-1` shows the whole script, refuses a script it cannot show faithfully (control or escape characters, an over-long line, too many lines), and asks you to type the first 8 characters of its hash at the terminal. An unapproved or changed script is skipped with a note. The approval binds the script's bytes, once per repository: two branches with different scripts replace each other's approval, and the script runs from a private copy, so paths relative to `$0` do not work. It does not bind what the script runs: an approved `npm install` runs whatever `package.json` says when a worktree is reused by `resume` or a second spawn.

**Pause, resume and clean.** `curia pause T-1` closes the task's panes, commits the work in progress (`--no-verify` skips the repository's commit hooks) and removes the worktree, keeping the branch. If the worktree holds ignored files (`node_modules`, `.venv`, `.env`) it keeps the worktree in place instead and says so; it never deletes ignored files. `curia resume T-1` re-creates the worktree if needed and runs `claude --resume` for the task's session. `curia clean T-1` removes the worktree and the merged branch, refuses a worktree with ignored files unless you pass `--force`, names the files first, refuses an unmerged branch unless you pass `--force` (it names the commit that would be lost), and marks the task dropped; `curia task edit T-1 --status ready` brings it back. Pause, resume and clean are for you at a terminal; an agent cannot run them.

## Messages and teams

`curia say "text"` (or `prefix + m`) sends a message down to an agent. A message that starts `T-42:` or `@<name>` (an alias from the `routing.aliases` config table, a task id, a repository or a branch) goes to that target with no model. Otherwise one bounded call to Claude (`claude -p --model haiku`, on your existing login, 5 seconds) suggests the target and the kind, and you confirm with Enter, change with Tab or `k`, or cancel with Esc; when the call fails you pick by hand and Curia says why. The kind is an instruction (typed into the agent's pane when it is idle, or handed over by its next Stop hook, or fetched when the agent calls `check_messages`), a standing rule (a decided precedent with no options, scoped to a repository or global) or a new task. A rule is never sent automatically. Automatic sending is off by default (`routing.auto_send`, `routing.auto_threshold`): it can switch on only after the router has been right at least 95% over the last 50 messages you confirmed, one in ten still asks, and an automatic message is held for 5 seconds so `u` can undo it. The text of a message reaches the agent through the same sanitiser and typing guards as an answer, with the same limits on when Curia types.

A message for a team goes to its lead. Curia reads Agent Teams from Claude Code's own team config and shows a team as one unit in the dashboard; only a team's lead may raise a decision ticket, and a teammate that tries is told to ask its lead.

## Reviews

When an agent has finished a task it calls `complete_task` with a summary and whether its tests passed, failed or were not run. That opens a review and does nothing else: the tool runs no git, cannot approve, merge or push, and takes no task argument (the task is the one its own session is linked to). The task then shows as `review`, and as `done` once you approve; both are worked out from the task's newest review. The badge counts waiting reviews as `R<n>`, a window whose agent has a review you have not seen gets a blue dot, and `curia reviews ls` lists them.

`curia review` (or `r` in the dashboard) shows the agent's summary, its test claim (labelled as the agent's report, not Curia's), the diff stat against the base branch at the commit shown, the decisions made on the task, any unconfirmed assumption in red at the top, and the session count, duration and cost. Keys: `a` approves (then `y` to confirm, after a short pause), `c` writes a change request, `x` drops the task, `o` opens the full diff in `review.diff_tool` (`git`, `delta`, `lazygit` or `nvim`). Like the decision view, a held, pasted or repeated key does nothing; and a confirmation key that arrives within 50 ms of another key is refused, so a pasted `y` followed by a line break cannot approve.

**Approving.** By default `a` runs `git merge --no-ff --no-edit` of the task's branch into its base branch in your checkout. It runs only when the base branch is checked out there, no merge or rebase is under way, the agents are idle, the agent's worktree is clean and the checkout has no staged changes; the repository's own hooks run; nothing is pushed and nothing is forced. With `--pr` (or `review.approve = "pr"`) Curia pushes the branch and runs `gh pr create` instead; the review then ends `approved`, not `merged`, and nothing later flips it. If a pull request for the branch already exists, Curia records it as this approval's only when it is open, from that branch into the base, at the commit you approved, with the title and text this approval would have created, and on the repository the remote names; any other pull request is left alone and the review stays pending (Curia never edits a pull request it did not create). Approving also expires the task's open assumption tickets and leaves blocking ones open.

**Approval is bound to the commit you were shown.** The view remembers the branch head it displayed; if the agent has pushed another commit since, the approval is refused, nothing is merged, and the view shows the new state. The merge uses that commit's id, not the branch name. The non-interactive form needs the same proof: `curia review R-3 --approve --head <full commit id>`. Without `--head` it prints the branch head, the diff stat line and the exact command, and exits 1; `--head` must be the full id (a prefix is refused). `curia review R-3 --json` prints the head.

**What a merge approval does and refuses.** `curia review --approve --merge` merges the approved commit into the base in your checkout with git's default strategy and options (it ignores `branch.<base>.mergeOptions` and `pull.twohead`), and records the review merged only when the base then contains that commit, never just because git exited 0. It refuses before anything changes when the merge would conflict, when it would add a file that already exists in your checkout untracked or ignored (move it out of the way and approve again; on a case-insensitive disk a file whose name differs only in case is also in the way, and so is one whose name differs only by Unicode normalisation), when your checkout has staged changes, another branch or an operation under way, and while an earlier approval's merge may still be running there (this applies to a pull-request approval, a drop and a change request as well; once that merge has finished, approving again records it).

**If an approval is interrupted.** Closing the popup or the terminal does not cut an approval short: Curia holds the hang-up and waits for the merge to finish and be recorded (at most 7 minutes). If Curia itself is killed, git finishes in its own process group and the next `curia review <id> --approve` records the merge it finds. If the merge is interrupted before it is recorded, Curia undoes it only when it can prove the checkout holds nothing but its own merge: the index equals the merge's expected result, no `index.lock` is left, and every modified file in your working tree was modified before with the same content. Otherwise it runs nothing, prints the state and the commands to finish or undo it yourself, and the review stays pending. Curia says "as it was" only after reading the checkout again. Limits in plain words: with more than 500 modified files the merge still runs but Curia will not undo it; with a git older than 2.38 it merges but never undoes; with a git older than 2.31 a `-s ours` in `branch.<base>.mergeOptions` would go unnoticed; and one window remains: a change you stage in the few milliseconds between Curia's last check and its undo is undone with the merge.

**Requesting changes and dropping.** `c` (or `--request-changes --note "..."`) sends your note to the task's agent as a message, typed into its prompt when it is idle, delivered by its next Stop hook when it is working, or fetched when it calls `check_messages`; the agent finishes and calls `complete_task` again, which opens the next review. Typed lines lose colons and some punctuation, so the full wording is also available through `check_messages`. `x` (or `--drop`) drops the task; its agent is not stopped, so kill it from the dashboard.

## Restore

`curia restore` looks for Claude Code sessions active within the last day (`--since`) that no pane runs any more: ones that were running when tmux or the machine went down, and ones Claude Code ended with reason "other" because their terminal went away. It skips a session that started after the running tmux server did, since its pane was closed while tmux kept running (you closed it, or killed it from the dashboard): `--include-closed` offers those too. It prints the plan, asks, and then opens one window per session with `claude --resume` (`curia launch` for a task's session), in the session's own directory, adding no permission flag. It types nothing into the new windows itself, and it resumes a team's lead, not its teammates: the lead gets a message through the usual message path (typed when its prompt is empty, otherwise at its next Stop) asking it to respawn its team. A session waiting at a dialog in its window (the trust dialog, or `/rate-limit-options` after a usage limit) is not offered again, and you answer the dialog yourself. `--dry-run` prints the plan and changes nothing; `--yes` skips the question. It needs a running tmux server, and a session that was on another tmux server is left alone.

## Cost, limits and integrations

**Where the numbers come from.** Cost-lens, when it pushes to `curia ingest` (it does not yet: that is for its owner to build against `docs/ingest-schema.json`), and otherwise the status line. `curia install` sets Claude Code's status line to `curia statusline` unless you have one of your own or pass `--no-statusline` (see "What `curia install` changes"). Claude Code runs that command after each response with a JSON document on stdin. Curia reads from it the session id, its cost so far, its context window and how full it is (once the status line has reported it is the only source of the context figure; a figure over 100% is not shown as a number, the dashboard shows `?`), the five-hour and seven-day usage percentages with their reset times, and the prompt cache's expiry and rebuild size. It prints one line such as `<model> · ctx 71% · $4.12 · 5h 62%` and always exits 0. It opens no network connection and starts no process. Curia computes no cost and keeps no price table: it stores what it is told, and where a dollar figure is unknown it shows tokens. `curia ingest` takes the same numbers from another program as one JSON document (the contract is `docs/ingest-schema.json`); any program on your machine can call it, so treat the numbers as advice.

**Cost and limit forecast.** From the usage samples Curia works out how fast a window fills. When the limit would land before the window resets, the status badge adds the sooner of the two warnings, for example `5h 62% → limit ~48m (resets 1h50m)`. No warning appears without a reset time. `curia usage` lists the windows and the sessions that burn most (in dollars per hour; with an API key and no usage windows it shows the projected spend for the next two hours instead). In the dashboard the cost and cache columns appear from 100 columns wide, and `b` sorts sessions by that rate. Under a warning the dashboard suggests pausing the lowest-priority running task: `P`, then `y`. It never pauses anything by itself. A task's priority is 1 to 3 (`--priority P1` to `P3` on `curia task add` and `edit`, default P2).

**The cheaper model.** Under a limit warning, `curia spawn` starts a new Claude Code agent on the model in `[usage] cheaper_model` (default `sonnet`; an empty string turns it off) and says why. That applies to that one spawn only and is not stored on the task. `--model` always wins. When an agent (not you at a terminal) spawns a task, `--agent`, `--model`, `--team` and `--resume` are refused and the task's stored values apply.

**Cache state and why a cold cache matters.** Claude Code keeps a conversation's prompt cached for a while after each turn (one hour on the account Curia was tested on). An agent that waits for your answer past that time pays to rebuild its whole context on its next turn. The dashboard shows, for idle and waiting sessions, the time left or `cold` with the rebuild size. A phone push for a blocking ticket adds one line, such as "answer within 12m to keep cache warm" or "cache cold: next turn re-writes ~84k tokens". The rebuild is shown in tokens, not dollars, because Claude Code's status line gives only tokens. Where Curia knows no cache state it shows nothing.

**Branch cost in a review.** `curia review` shows what the task's branch has cost, when cost-lens is installed. Curia runs cost-lens's analyzer in the background on the task's worktree, reads only the current project's figure from its output, and shows nothing until the answer arrives.

**Obsidian export.** Set `[obsidian] dir` to an existing folder in `~/.config/curia/config.toml` (empty means off; Curia never creates the folder). Each decided decision (answered, confirmed or overridden, standing rules included) is written as `<id> <title>.md` after `curia decide`, and `curia export obsidian [<id>...]` writes them on demand; `--all` rewrites every one. The note has front matter and the question, options, answer and reasoning, with all of that text inside code fences so Obsidian plugins do not interpret it. Curia rewrites a note only when it holds Curia's own marker (`curia_id: <id>` near the top). A note of yours with the same name is never touched: Curia reports it, skips that decision and remembers the skip, so the next sweep does not retry it. Rename your note, then run `curia export obsidian --retry-skipped`. Nothing is deleted and nothing is read back.

**Agent Commons.** `curia export commons <id>` prints the arguments of Agent Commons's `propose` for a global decision and sends nothing; no repository path, task, session or branch is included. Handing it to an agent that has the commons tools is up to you.

**Linear import.** `curia task import --linear [--repo <dir>] [--team <KEY>]` imports the Linear issues assigned to you that are in a backlog or unstarted state, at most 200 (four pages of 50; if there are more, the rest are not imported). The key comes only from the environment variable `CURIA_LINEAR_API_KEY`; the address is `[linear] url` in the config file (default `https://api.linear.app/graphql`). The import is read-only: Curia sends one fixed query and never changes anything in Linear. If a page fails, nothing is imported; run it again. A re-import updates a task's title, body and tags only, not its priority or status; when that replaces local text that differs from Linear's, it says so.

**Spawning with Codex (support is partial).** `curia spawn T-1 --agent codex` starts the Codex CLI on the task in the same worktree and window, and `--model` passes a model name to it (a stored model is used only with the agent it was set for). The hook records Codex sessions so they appear in the dashboard. Codex support is PARTIAL until `curia install --codex` lands: a Codex session has no Curia MCP tools and no Curia hooks yet, so it cannot raise a decision ticket or call `complete_task`; Curia never types into a Codex pane; `--team` is refused with Codex; and `curia restore` skips Codex sessions and prints the command to run by hand.

**What leaves the machine, in all.** Only these, each when you have turned it on or run it: phone pushes to your `[ntfy] url` (see Phone pushes below); `curia task import --linear`, one query to the Linear API (or the `[linear] url` you set) with your Linear key; and the one `claude -p` call `curia say` makes to route an unaddressed message, which carries your text and the candidate targets through your own Claude login. `curia serve` only receives. The status line, the forecast, the exports and the review's branch cost run locally (the branch cost runs cost-lens's analyzer, a local program). Curia reads secrets only from environment variables (`CURIA_LINEAR_API_KEY`, `CURIA_NTFY_TOKEN`); no key is a config option, and none is stored, logged or printed.

The config file is `$XDG_CONFIG_HOME/curia/config.toml` when that variable is an absolute path, else `~/.config/curia/config.toml`; the database and log follow `XDG_DATA_HOME` and `XDG_STATE_HOME` the same way, and `CURIA_DB` still wins.

**Config keys of this section** (in `~/.config/curia/config.toml`): `[usage] cheaper_model`, `[obsidian] dir`, `[linear] url` and `team`, and `[costlens] analyzer` (the path of cost-lens's analyzer script; empty means the one beside the `cost-lens` command on `PATH`, which also needs `node`).

## Phone pushes and `curia serve`

Pushes are off until `[ntfy] url` is set in `~/.config/curia/config.toml` and at least one device is listed under `[ntfy.devices]` (a device name and its topic). With neither, Curia opens no connection. `[ntfy] quiet = "22:00-07:00"` holds pushes back in that window (local time); the bell and the macOS notification are not affected. The access token for a protected server comes only from the environment variable `CURIA_NTFY_TOKEN`, never from the config file.

**A push is sent by whichever Curia process that flushes pushes is running:** each agent's `curia mcp`, or `curia serve`. A push queued when the last of them exits is sent when the next one starts.

**What leaves the machine, and only to the `[ntfy] url`** (no redirect is followed and no proxy is used):

- For a new blocking ticket (not an assumption): the title `D-<n> <repository directory name>: decision needed`, then the agent's question, one line per option (`key: label`), the recommended key, and a fixed line for an irreversible ticket or one with more than three options; at most 1200 characters. That is agent-written text, so whatever an agent puts in a question leaves the machine.
- With `[serve] public_url` set and a reversible ticket: up to three buttons, each with that URL plus `/v1/answer`, a body naming the ticket and the option, and the header `Authorization: Bearer <one-time token>`.
- For a new review (not an update of a pending one): the title `R-<n> T-<n>: ready for review`, the task title, "Tests (the agent's report): <status>" and the agent's summary cut to 600 characters. No button.
- `CURIA_NTFY_TOKEN` as a bearer header, when set.
- Never: file paths, branch names, diff content, the note of a change request, session ids or another ticket's text. Neither token appears in a log, an error or a URL.

A ticket answered, withdrawn or expired before its push is due is dropped; a retry after a failure keeps to the quiet hours. A duplicate can still happen if a sender died or stalled between ntfy's reply and Curia's record of it: the push is sent again after two minutes with a new token (the first notification's button then fails with `401`).

**The topic and the token.** A decision push with buttons carries a one-time token in its action headers. It rests on the ntfy server (its message cache) and on your phone, so anyone who can read the topic can answer that one ticket, once, within 24 hours, with any of its options. Use a self-hosted ntfy server or an access-controlled topic: on ntfy.sh a topic name is the only secret. Irreversible tickets never get buttons. An agent on your machine runs as you and can read the config and the topic; Curia does not protect against that.

**`curia serve` and Tailscale.** `curia serve` listens on 127.0.0.1:8787 unless you name another address with `--listen` or `[serve] listen`, which must be loopback or this machine's Tailscale address. Nothing is detected. Put `tailscale serve` in front of the loopback address so your phone reaches it over your tailnet, and set `[serve] public_url` to the https URL tailscale serve gives you. Never use Tailscale Funnel or a public address: the token is the only check.

**What the endpoint accepts.** One route, `POST /v1/answer`, and only while you run `curia serve`. Without having read a push, a caller can learn that the service is up (`/healthz`) and receive `400`, `401`, `404`, `405` or `429`; it cannot answer, learn whether a ticket exists or read anything. Having read a push, it can answer that one ticket once, with any option the ticket offers, while the ticket is open and not irreversible. The answer travels the same typing path as `curia decide`. Ten requests a minute with a token that does not match are refused with `429`; a request with a valid token is never limited.

**Two tmux servers.** Curia records which tmux server each session's pane is on (`#{socket_path}`). Pane ids repeat across servers, so the dashboard, `curia next`, `pause`, `clean`, typing answers and messages, and the attention markers act only on panes of the server they reach; a session on another server is shown, and refused with "on another tmux server". Sessions recorded before this version have no server until their next SessionStart and are treated as before.

## What the user guard does and does not do

Some commands are yours alone: `curia say`, `decide`, `decisions edit`, `install`, `uninstall`, `messages cancel`, `routing reset`, `setup approve`, `pause`, `resume`, `clean`, `review`, `restore`, starting `serve` and the task-writing commands (`task add`, `edit`, `ready`, `import`); `curia send` labels a message as from you only under the same check, and the dashboard's board actions (`r`, `n`, `x`, `d`) and its review view's `a`, `c` and `x` use it too (inside an agent session the review view is read-only). They run only for a person at a terminal: not inside an agent session, not without a controlling terminal (a script or cron job), and not from a process an agent started. A tmux popup counts as your terminal. A message sent from anywhere else is delivered as information from another agent session, and an agent's messages are limited to 5 queued per target and 20 per sender in 10 minutes. When tmux is unreachable the check relies on the process ancestry alone (no Claude Code process among the caller's parents), so a stopped or restarted tmux server never locks you out.

These checks stop mistakes, instructions an agent follows naively, and one-line evasions such as clearing environment variables. Because Curia and your agents run as the same operating-system user, they are not a security boundary against an agent that sets out to impersonate you. A determined agent could still:

- detach a process from its parent and open a pseudo-terminal of its own, so that it looks like a terminal of yours;
- start a job through tmux (`run-shell`, `new-window`, `display-popup`), which has the same ancestry as your own popup;
- type into your panes with `tmux send-keys`, or read them with `capture-pane`;
- write Curia's SQLite database directly; and
- point `curia ls` at another tmux server so that reconcile marks real sessions stale.

Separating agents from you needs a different operating-system user for them, or a secret they cannot read; Curia does not do that in this version.

## Status

V1, for one user on macOS and Linux. The full list of what V1 does differently from the spec is `docs/v1-as-built.md`; changes by release are in `CHANGELOG.md`.

**Not verified by a live run.** The tests drive Curia against stand-ins for tmux, git, ntfy, Linear and cost-lens, and probes of real Claude Code, tmux, git, ntfy and Codex's `mcp` command were run on the author's machine. The final live check with real sessions has not been run for the cost, forecast and integration features, and these open probes need the owner's accounts or devices: Codex hooks (X1), the real Linear API (L1), Obsidian rendering (O1), what Claude Code does at a real usage limit, the phone button of a push, and Remote Control.

**Known limits.**

- Codex support is partial (see above); `curia install --codex` does not exist yet.
- The cost-lens push does not exist yet, so dollar costs and rebuild dollars are blank unless cost-lens pushes; the status line gives tokens and the usage windows.
- Nothing is sent to Agent Commons, Linear is read-only, and Obsidian is one way.
- Priority is not shown on the task board; the forecast pause is two keys (`P`, `y`), never automatic.
- A permission prompt shows up to 6 seconds late, since Claude Code sends no hook until then.
- Typing into a pane depends on recognising Claude Code's screen; a Claude Code update can turn typing off until Curia is updated (answers still arrive at the next Stop or by `check_decision`).
- A later merge of a pull request opened by `curia review --pr` is not tracked; pane layouts are not restored; a phone cannot approve a review.
- The user guard is not a security boundary (see above).

## License

MIT. See [LICENSE](LICENSE).
