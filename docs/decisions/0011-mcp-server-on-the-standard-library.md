# 0011. MCP server on the standard library

Status: accepted (Plan 2 setup)

## Context

Plan 2 adds `curia mcp`, a stdio MCP server that Claude Code starts once per session. It needs five methods (`initialize`, `notifications/initialized`, `ping`, `tools/list`, `tools/call`) and four tools. `go.mod` is a shared file (decision 0009), and every module added to the binary also starts with every hook and every session.

## Decision

- `internal/mcp` implements the protocol on the standard library (`bufio`, `encoding/json`), about 250 lines: one JSON-RPC 2.0 message per line, at most 1 MiB, HTML escaping off, nothing but protocol messages on stdout.
- Plan 2 adds no module. `go.mod`, `go.sum`, the Makefile and `lefthook.yml` are unchanged, and `go mod tidy` is still not run by any task.
- The depguard rule for `internal/mcp` allows no third-party module (decision 0010).
- The protocol is pinned by tests that drive `Serve` through real pipes (`os.Pipe`), not by a library's behaviour.
- Tolerance, from the pre-wave-1 live probe (Claude Code 2.1.322, 2026-11-11): the client's first message is `server/discover` (protocol 2026-07-28), and it falls back to `initialize` with `2025-11-25` only after a `-32601` error. So every unknown request gets `-32601` at once (never silence, never exit), unknown notifications are ignored, `_meta` on `tools/call` (`claudecode/toolUseId`, `progressToken`) is accepted and ignored, and `initialize` answers with the client's version when the server knows it (`2025-11-25`, `2025-06-18`, `2025-03-26`, `2024-11-05`), else `2025-11-25`. The `instructions` string reached the model.

## Consequences

- No new dependency, no `go.sum` change, nothing new in the binary's start-up.
- If a later Claude Code stops falling back when `server/discover` fails, the server must implement it.
- A feature Plan 2 does not implement (resources, prompts, structured output, batches) is absent. If Claude Code later needs one, it is added by hand, or the official Go SDK replaces the implementation behind `mcp.Server`; that would be a `go.mod` change made by the orchestrator, with a depguard allow line.
