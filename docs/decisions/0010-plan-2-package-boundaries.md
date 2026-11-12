# 0010. Plan 2 package boundaries

Status: accepted (Plan 2 setup). Extends decision 0003, whose rules and details still hold.

## Context

Plan 2 adds six packages: `sanitize`, `decision`, `precedent`, `notify`, `deliver` and `mcp`. As in Plan 1, parallel implementers do not see each other's code, so the order of the plan's File structure table has to be enforced by the gate. Two of the new packages hold pure rules that the store must apply inside its own transactions: `decision` (validation, resolving an answer) and `precedent` (the FTS5 match expression). `sanitize` is needed by both and by everything that prints.

## Decision

Table order (17 packages, then the command): `model`, `config`, `sanitize`, `decision`, `precedent`, `store`, `state`, `transcript`, `hook`, `tmux`, `fleet`, `attention`, `notify`, `deliver`, `mcp`, `ui`, `install`, `cmd/curia`.

| Package | Third-party allowed | Internal packages allowed |
| --- | --- | --- |
| internal/model | none | none (itself only) |
| internal/config | BurntSushi/toml | model |
| internal/sanitize **new** | none | model, config |
| internal/decision **new** | none | model, config, sanitize |
| internal/precedent **new** | none | model to decision |
| internal/store | modernc.org/sqlite | model, config, sanitize, decision, precedent |
| internal/state | none | model to store |
| internal/transcript | none | model to state |
| internal/hook | none | model to transcript |
| internal/tmux | none | model to hook |
| internal/fleet | none | model to tmux |
| internal/attention | none | model to fleet |
| internal/notify **new** | none | model to attention |
| internal/deliver **new** | none | model to notify |
| internal/mcp **new** | none | model to deliver |
| internal/ui | bubbletea, lipgloss | model to mcp |
| internal/install | none | model to ui |
| cmd/curia | cobra, bubbletea, lipgloss, modernc.org/sqlite | every package above |
| e2e | none | none |

- Every new rule is `list-mode: strict` with exact-match internal entries, as in decision 0003. Every existing rule below a new package gains an allow line for it; `model`, `config` and `e2e` are unchanged.
- The `unlisted` catch-all excludes the six new directories.
- `sanitize`, `decision` and `precedent` sit above `store` because the store applies their rules inside its transactions (one `BEGIN IMMEDIATE` per read-modify-write), so the store must be able to import them; they do no I/O of their own.
- `scripts/check-layout.sh` lists the same 17 packages (decision 0006).

## Consequences

- Verified during setup with temporary probe files and `make check` (all removed afterwards). Each forbidden import failed at the boundaries step: `sanitize`, `decision` and `precedent` importing `store`; `mcp` importing cobra; `notify` importing `deliver`; `deliver` importing `mcp`; `mcp` importing `ui`; `attention` importing `notify`; `model` importing `sanitize`; an unlisted `internal/foo` importing `model`. Each allowed import passed every gate: `store` importing `precedent`; `ui` importing `deliver`; `sanitize` importing `config`; `decision` importing `sanitize`; `precedent` importing `decision`; `notify` importing `tmux`; `deliver` importing `notify`; `mcp` importing `deliver`; `cmd/curia` importing `mcp`; `hook` importing `sanitize`. The layout check rejected `internal/sanitizer/` and a stray root file, and accepted `internal/install/skill/SKILL.md`, `testdata/decisions/`, `e2e/testdata/decisions/` and `cmd/curia/testdata/`.
- Two more rows of rules for two small pure packages. A later plan that adds a package or a module still needs a rule change by the orchestrator.
