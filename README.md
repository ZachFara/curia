# Curia

Curia turns tmux into a control plane for several Claude Code sessions running at once. Claude Code hooks record every session event in one local SQLite database, and everything else is a view over it: a status-bar badge, window markers, a dashboard popup, and a key that jumps to the session that needs you. Agents can also raise decision tickets through an MCP server, with a recommendation, and search the answers you gave before; you answer from a popup or the command line. It is one Go binary with no daemon and no cgo.

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
- **Permission rules.** The four Curia MCP tools (`mcp__curia__search_decisions`, `mcp__curia__request_decision`, `mcp__curia__check_decision`, `mcp__curia__withdraw_decision`) are added to `permissions.allow` in the same settings file, so agents can use them without a prompt. `curia install --no-allow-mcp-tools` skips this step.
- **MCP server.** Registered with `claude mcp add --scope user curia -- <path to curia> mcp`. If `claude` is not on your `PATH`, install prints that command for you to run. A `curia` entry that is not this binary's is left alone.
- **Skill.** `~/.claude/skills/curia-decisions/SKILL.md` (under `$CLAUDE_CONFIG_DIR` if set) tells agents when and how to raise a decision ticket. A `SKILL.md` there that is not Curia's is left alone.
- **tmux.** A snippet at `~/.config/curia/curia.tmux`, loaded from `~/.tmux.conf` by a block between the lines `# >>> curia >>>` and `# <<< curia <<<`. Curia edits nothing in `~/.tmux.conf` outside that block. The snippet turns on `focus-events`, sets `status-interval` to 2 and raises `status-right-length` to at least 80, appends the badge to `status-right`, adds a red dot to a window's tab when its session needs you, binds three keys and sets a `pane-focus-in` hook that marks a pane's sessions as seen. Curia adds to your window tab formats instead of replacing them, and it does not take over a key you have already bound. The three keys are configurable. Run `tmux source-file ~/.tmux.conf` afterwards, or restart tmux.

Restart running Claude Code sessions afterwards so they pick up the hooks, skill and MCP server.

`curia uninstall` removes what install added: the hook entries, the four permission rules, the MCP server, the skill, the tmux block and the snippet file. Restart tmux, or follow the commands it prints, to drop the key bindings and the hook from a running server. Curia's database in `~/.local/share/curia/` is not deleted.

The hook, MCP and tmux entries record the path you would type to run `curia`: the `curia` found on `PATH` when it is the running binary (with Homebrew, the stable symlink `/opt/homebrew/bin/curia`, not the versioned path under `Cellar`), otherwise the path the binary was started by. `curia install` prints the path it recorded, and warns when that path is versioned or not on `PATH`, because the entries break if that file moves; put `curia` on `PATH` and run `curia install` again to fix that.

A `brew upgrade curia` needs nothing else. If you installed an older release, which recorded the `Cellar` path, run `curia install` once after upgrading: it replaces those entries with the stable path, and `curia uninstall` removes them either way.

## Keys and commands

With the default tmux keys (after your tmux prefix):

| Key | Action |
| --- | --- |
| `a` | Dashboard popup (`curia ui`) |
| `A` | Decision queue popup (`curia decide`) |
| `N` | Jump to the session that needs you most (`curia next`) |

In the dashboard, `j`/`k` or the arrows move, Enter or a click jumps to the session's pane, `d` opens that session's decisions, `/` filters, `x` then `y` kills the pane, and `q` or Esc quits.

| Command | What it does |
| --- | --- |
| `curia ls` | List sessions, the one that most needs you first (`--json` for scripts) |
| `curia next` | Switch to the session that needs you most |
| `curia ui` | The dashboard, meant for a tmux popup |
| `curia sidebar` | The dashboard with narrow columns, for an always-open pane |
| `curia decide` | Answer decision tickets: with no argument the queue, with an id and `--option`, `--accept` or `--text` a script-friendly answer |
| `curia decisions ls` | List decision tickets, answered ones included |
| `curia decisions edit` | Rescope or supersede an answered ticket |
| `curia status` | The badge tmux shows: `D<n> Q<n> W<n> R<n>`, for open blocking tickets, answers a live session has not yet received, waiting sessions and reviews |

## How decisions work and what the safety limits are

An agent that hits a judgment call searches earlier decisions through the MCP tools, then raises a ticket with two to four options and a recommendation. A ticket is either blocking (the agent finishes safe work and stops) or an assumption (the agent keeps going on its recommendation and you can override it later). The MCP server has no tool that answers a ticket; answers are written only by `curia decide`, which refuses to run inside an agent session.

An answer reaches the agent at most once, by the first of three paths:

1. `curia decide` types it into the agent's pane, but only when a screen capture shows Claude Code's empty input prompt. A draft, a dialog, a menu, a shell, a running turn, background subagents, a pane shared by two sessions or a tmux window with `synchronize-panes` on all stop it from typing. If Claude Code's prompt looks different from the versions Curia recognises, typing turns itself off.
2. The agent's next `Stop` hook blocks the stop and hands over the answer.
3. The agent calls `check_decision`.

Typed text is a fixed sentence beginning "Decision D-<n>" with a restricted character set, so it cannot start a slash command, and in a shell it is a failed command, not an executed one.

These are guards, not a security boundary. An agent that has a shell is not prevented from running `curia decide` itself, or from writing to the SQLite file; the refusal inside an agent session is a check on the environment, not a lock. Curia reads Claude Code's screen to decide whether it may type, so a Claude Code update can switch typing off until Curia is updated; answers still arrive through the second and third paths.

## Status

Pre-1.0. Curia is written for one user on macOS and Linux, and its behaviour and database layout may change between releases.

## License

MIT. See [LICENSE](LICENSE).
