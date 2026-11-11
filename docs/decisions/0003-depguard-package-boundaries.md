# 0003. depguard package boundaries

Status: accepted (Phase 1 setup)

## Context

The plan's File structure table says each package "depends only on packages above it in this table, so every package below `model` can be tested alone". Parallel implementers will not see each other's code, so the boundary has to be enforced by the gate, not by review.

## Decision

`.golangci.yml` encodes the table literally as depguard rules, one per directory, every rule in `list-mode: strict` (fail closed: an import not on the allow list is rejected).

Table order: `model`, `config`, `store`, `state`, `transcript`, `hook`, `tmux`, `fleet`, `attention`, `ui`, `install`, `cmd/curia`.

| Package | Standard library | Third-party allowed | Internal packages allowed |
| --- | --- | --- | --- |
| internal/model | yes | none | none (itself only) |
| internal/config | yes | BurntSushi/toml | model |
| internal/store | yes | modernc.org/sqlite | model, config |
| internal/state | yes | none | model to store |
| internal/transcript | yes | none | model to state |
| internal/hook | yes | none | model to transcript |
| internal/tmux | yes | none | model to hook |
| internal/fleet | yes | none | model to tmux |
| internal/attention | yes | none | model to fleet |
| internal/ui | yes | bubbletea, lipgloss | model to attention |
| internal/install | yes | none | model to ui |
| cmd/curia | yes | cobra, bubbletea, lipgloss, modernc.org/sqlite | every package above |
| e2e | yes | none | none |

Details:

- Internal entries use depguard's exact-match form (`.../internal/store$`), so a package named `storex` is not let in by prefix. Each rule also allows the package itself, for external `_test` packages.
- Each rule denies the module root `github.com/ZachFara/curia` with a message naming the allowed packages; the longer allow entries win for permitted imports.
- `cmd/curia` may import `modernc.org/sqlite` because Task 5's `TestHookExitsZeroWhenStoreLocked` holds a lock through a second connection.
- A catch-all rule `unlisted` applies to every Go file outside the directories above and denies any import of this module. Together with the layout check (decision 0006), a new package cannot appear without a rule.
- Tests follow the same rules as the package they test; there are no test-helper modules (no testify or similar).

## Consequences

- Verified during setup: `internal/model` importing `internal/store` fails `make check` at the boundaries step; `model` importing cobra, `e2e` importing `model`, `state` importing `hook`, and an unlisted `internal/foo` importing `model` all fail; `store` importing `model` and `hook` importing `store` pass.
- Strict mode means a package that needs a new third-party module, or a new package in a later plan, needs a rule change. That is a shared-file change made by the orchestrator between waves, recorded as a ruling.
