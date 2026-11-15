# Curia

Curia turns tmux into a control plane for several Claude Code sessions running at once. Claude Code hooks record every session event in one local SQLite database, and everything else is a view over it: a status-bar badge, window markers, a dashboard popup, and a key that jumps to the session that needs you. Agents can also raise decision tickets through an MCP server, with a recommendation, and search the answers you gave before; you answer from a popup or the command line. You can keep a backlog of tasks, start an agent on one in its own git worktree, and send a message to a running agent in plain language. It is one Go binary with no daemon and no cgo.

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
- **Permission rules.** The five Curia MCP tools (`mcp__curia__search_decisions`, `mcp__curia__request_decision`, `mcp__curia__check_decision`, `mcp__curia__withdraw_decision`, `mcp__curia__check_messages`) are added to `permissions.allow` in the same settings file, so agents can use them without a prompt. `curia install --no-allow-mcp-tools` skips this step.
- **MCP server.** Registered with `claude mcp add --scope user curia -- <path to curia> mcp`. If `claude` is not on your `PATH`, install prints that command for you to run. A `curia` entry that is not this binary's is left alone.
- **Skill.** `~/.claude/skills/curia-decisions/SKILL.md` (under `$CLAUDE_CONFIG_DIR` if set) tells agents when and how to raise a decision ticket. A `SKILL.md` there that is not Curia's is left alone.
- **tmux.** A snippet at `~/.config/curia/curia.tmux`, loaded from `~/.tmux.conf` by a block between the lines `# >>> curia >>>` and `# <<< curia <<<`. Curia edits nothing in `~/.tmux.conf` outside that block. The snippet turns on `focus-events`, sets `status-interval` to 2 and raises `status-right-length` to at least 80, appends the badge to `status-right`, adds a red dot to a window's tab when its session needs you, binds four keys and sets a `pane-focus-in` hook that marks a pane's sessions as seen. Curia adds to your window tab formats instead of replacing them, and it does not take over a key you have already bound. The four keys are configurable (`key_dashboard`, `key_decisions`, `key_next` and `key_message` in the config file). Run `tmux source-file ~/.tmux.conf` afterwards, or restart tmux.

Restart running Claude Code sessions afterwards so they pick up the hooks, skill and MCP server.

`curia uninstall` removes what install added: the hook entries, the five permission rules, the MCP server, the skill, the tmux block and the snippet file. Restart tmux, or follow the commands it prints, to drop the key bindings and the hook from a running server. Curia's database in `~/.local/share/curia/` is not deleted.

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

In the dashboard, `j`/`k` or the arrows move, Enter or a click jumps to the session's pane, `d` opens that session's decisions, `t` shows the task board, `/` filters, `x` then `y` kills the pane, and `q` or Esc quits. A team of agents shows as one row (the worst state of its members) that Space expands; Enter on it jumps to the member that needs you most. On the board, `j`/`k` move between and across the status columns, `r` toggles a task between backlog and ready, and `n` then `y` spawns the selected backlog or ready task; the `y` is ignored until the screen has been still for a moment, so a held or pasted key starts nothing.

| Command | What it does |
| --- | --- |
| `curia ls` | List sessions, the one that most needs you first (`--json` for scripts) |
| `curia next` | Switch to the session that needs you most |
| `curia ui` | The dashboard, meant for a tmux popup |
| `curia sidebar` | The dashboard with narrow columns, for an always-open pane |
| `curia decide` | Answer decision tickets: with no argument the queue, with an id and `--option`, `--accept` or `--text` a script-friendly answer |
| `curia decisions ls` | List decision tickets, answered ones included |
| `curia decisions edit` | Rescope or supersede an answered ticket |
| `curia task add`, `ls`, `edit`, `ready`, `import` | Keep tasks: `add "title" --repo . --body file.md`; `ls` (`--json`); `edit` by flags or in your editor; `ready` marks a task spawnable; `import --backlog-md [dir]` reads a Backlog.md directory |
| `curia spawn <id>` | Start an agent on a task (see below) |
| `curia pause <id>`, `resume <id>`, `clean <id>` | Stop, restart and finish with a task's worktree (see below) |
| `curia say` | Send a message, make it a standing rule or a task |
| `curia send <pane> <text>`, `curia read <pane>` | For agents and scripts: send a message to another agent's pane, read the tail of its screen |
| `curia messages ls`, `cancel <id>`; `curia routing stats`, `reset` | Look at and cancel messages; see and reset the router's accuracy numbers |
| `curia setup approve <id>` | Approve a repository's setup script (see below) |
| `curia status` | The badge tmux shows: `D<n> Q<n> W<n> R<n>`, for open blocking tickets, answers a live session has not yet received, waiting sessions and reviews |

## How decisions work and what the safety limits are

An agent that hits a judgment call searches earlier decisions through the MCP tools, then raises a ticket with two to four options and a recommendation. A ticket is either blocking (the agent finishes safe work and stops) or an assumption (the agent keeps going on its recommendation and you can override it later). The MCP server has no tool that answers a ticket; answers are written only by `curia decide`, which refuses to run inside an agent session.

An answer reaches the agent at most once, by the first of three paths:

1. `curia decide` types it into the agent's pane, but only when a screen capture shows Claude Code's empty input prompt. A draft, a dialog, a menu, a shell, a running turn, background subagents, a pane shared by two sessions or a tmux window with `synchronize-panes` on all stop it from typing. If Claude Code's prompt looks different from the versions Curia recognises, typing turns itself off.
2. The agent's next `Stop` hook blocks the stop and hands over the answer.
3. The agent calls `check_decision`.

Typed text is a fixed sentence beginning "Decision D-<n>" with a restricted character set, so it cannot start a slash command, and in a shell it is a failed command, not an executed one.

These are guards, not a security boundary. An agent that has a shell is not prevented from running `curia decide` itself, or from writing to the SQLite file; the refusal inside an agent session is a check on the environment, not a lock. Curia reads Claude Code's screen to decide whether it may type, so a Claude Code update can switch typing off until Curia is updated; answers still arrive through the second and third paths.

## Tasks, spawning and worktrees

A task is a row in Curia's own database with a title, a brief (its body), a repository and a base branch. You set only three statuses: backlog, ready and dropped; running, needs a decision and the rest are worked out from the sessions linked to the task, so the board does not go stale. `curia task edit` opens an editor only when the `editor` option is set in `~/.config/curia/config.toml` (for example `editor = "nvim"`: a program and its arguments split on spaces, run without a shell, with the file path added last); `$EDITOR` and `$VISUAL` are not read, and every field can also be changed with flags.

`curia spawn T-1` creates a git worktree for the task on a branch `curia/T-1-<slug>` beside the repository (`<repo>/../.curia-worktrees/<repo>-T-1`), opens a tmux window named `T-1` in a session named after the repository, and starts Claude Code there with the task's brief as its first prompt. The brief reaches `claude` as one argument, never through tmux's or a shell's command parser. `--team` lets the agent lead an Agent Teams team, `--model` picks the model. An agent can only spawn a task you have marked ready.

**Setup scripts.** If the repository has `.curia/setup.sh`, spawn runs it in the new worktree only if you have approved that exact content for that repository: `curia setup approve T-1` shows the whole script, refuses a script it cannot show faithfully (control or escape characters, an over-long line, too many lines), and asks you to type the first 8 characters of its hash at the terminal. An unapproved or changed script is skipped with a note. The approval binds the script's bytes, once per repository: two branches with different scripts replace each other's approval, and the script runs from a private copy, so paths relative to `$0` do not work. It does not bind what the script runs: an approved `npm install` runs whatever `package.json` says when a worktree is reused by `resume` or a second spawn.

**Pause, resume and clean.** `curia pause T-1` closes the task's panes, commits the work in progress (`--no-verify` skips the repository's commit hooks) and removes the worktree, keeping the branch. If the worktree holds ignored files (`node_modules`, `.venv`, `.env`) it keeps the worktree in place instead and says so; it never deletes ignored files. `curia resume T-1` re-creates the worktree if needed and runs `claude --resume` for the task's session. `curia clean T-1` removes the worktree and the merged branch, refuses a worktree with ignored files unless you pass `--force`, names the files first, refuses an unmerged branch unless you pass `--force` (it names the commit that would be lost), and marks the task dropped; `curia task edit T-1 --status ready` brings it back. Pause, resume and clean are for you at a terminal; an agent cannot run them.

## Messages and teams

`curia say "text"` (or `prefix + m`) sends a message down to an agent. A message that starts `T-42:` or `@<name>` (an alias from the `routing.aliases` config table, a task id, a repository or a branch) goes to that target with no model. Otherwise one bounded call to Claude (`claude -p --model haiku`, on your existing login, 5 seconds) suggests the target and the kind, and you confirm with Enter, change with Tab or `k`, or cancel with Esc; when the call fails you pick by hand and Curia says why. The kind is an instruction (typed into the agent's pane when it is idle, or handed over by its next Stop hook, or fetched when the agent calls `check_messages`), a standing rule (a decided precedent with no options, scoped to a repository or global) or a new task. A rule is never sent automatically. Automatic sending is off by default (`routing.auto_send`, `routing.auto_threshold`): it can switch on only after the router has been right at least 95% over the last 50 messages you confirmed, one in ten still asks, and an automatic message is held for 5 seconds so `u` can undo it. The text of a message reaches the agent through the same sanitiser and typing guards as an answer, with the same limits on when Curia types.

A message for a team goes to its lead. Curia reads Agent Teams from Claude Code's own team config and shows a team as one unit in the dashboard; only a team's lead may raise a decision ticket, and a teammate that tries is told to ask its lead.

## What the user guard does and does not do

Some commands are yours alone: `curia say`, `decide`, `decisions edit`, `install`, `uninstall`, `messages cancel`, `routing reset`, `setup approve`, `pause`, `resume`, `clean` and the task-writing commands (`task add`, `edit`, `ready`, `import`); `curia send` labels a message as from you only under the same check, and the dashboard's board actions (`r`, `n`, `x`, `d`) use it too. They run only for a person at a terminal: not inside an agent session, not without a controlling terminal (a script or cron job), and not from a process an agent started. A tmux popup counts as your terminal. A message sent from anywhere else is delivered as information from another agent session, and an agent's messages are limited to 5 queued per target and 20 per sender in 10 minutes. When tmux is unreachable the check relies on the process ancestry alone (no Claude Code process among the caller's parents), so a stopped or restarted tmux server never locks you out.

These checks stop mistakes, instructions an agent follows naively, and one-line evasions such as clearing environment variables. Because Curia and your agents run as the same operating-system user, they are not a security boundary against an agent that sets out to impersonate you. A determined agent could still:

- detach a process from its parent and open a pseudo-terminal of its own, so that it looks like a terminal of yours;
- start a job through tmux (`run-shell`, `new-window`, `display-popup`), which has the same ancestry as your own popup;
- type into your panes with `tmux send-keys`, or read them with `capture-pane`;
- write Curia's SQLite database directly; and
- point `curia ls` at another tmux server so that reconcile marks real sessions stale.

Separating agents from you needs a different operating-system user for them, or a secret they cannot read; Curia does not do that in this version.

## Status

Pre-1.0. Curia is written for one user on macOS and Linux, and its behaviour and database layout may change between releases.

## License

MIT. See [LICENSE](LICENSE).
