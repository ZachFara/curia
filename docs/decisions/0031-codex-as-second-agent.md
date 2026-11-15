# 0031. Codex as a second agent

Status: accepted (Plan 5 setup). The plan numbers this record 0029. Probes X1 (the owner's) and X2 have not run; their results can amend this record and Tasks 8 and 10.

## Context

Spec goal 7: agent-agnostic where hooks allow, Claude Code first, Codex second. Verified at the desk with `codex-cli` 0.166.0: `hooks` is stable and on by default, hooks need a recorded trust (`--dangerously-bypass-hook-trust` exists), `-m/--model`, `resume [SESSION_ID]` and `mcp add <NAME> -- <COMMAND>...` exist, and configuration lives in `$CODEX_HOME` or `~/.codex`. Not verified: where hooks are declared, their payloads, what a Stop hook's output does, whether a hook sees `TMUX_PANE`.

## Decision

- **Same hook, keyed by agent:** `curia hook <Event> --agent codex` sets the event's agent; `state.Apply` gives a new session that agent (`claude` when empty). For a Codex event the transcript is not read and a Stop claims nothing, so the hook does less, not more. No process, statement or file read is added to the hook path.
- **No typing, no handover at Stop.** An answer or message reaches a Codex agent only through `check_decision` and `check_messages`; the pane check refuses any screen that is not Claude Code's prompt.
- **`curia install --codex` is opt-in** and registers the MCP server and Curia's hook entries, only Curia's, backup first, never reading a credential file. Its task waits for probes X1 and X2.
- **Spawn:** `curia spawn <id> --agent codex` stores `tasks.agent`; `curia launch` runs `codex [--model <m>] <brief>` or `codex resume <uuid>`, with no sandbox or approval flag. `--team` with Codex is refused; the cheaper-model default does not apply. An unlinked Codex session is not restored.
- Codex's hook payloads, config and any file read from `$CODEX_HOME` are untrusted data.

## Consequences

- A Codex row shows no model or context; an answer reaches a Codex agent only when it asks.
- Codex support may land after the rest of Plan 5; nothing else depends on it.
- If X1 shows Claude-compatible payloads and a working Stop block, the Codex Stop claim is turned on and this record is amended.
