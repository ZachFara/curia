# 0013. Text typed into panes

Status: accepted (Plan 2 setup)

## Context

`curia decide` types an answer into the asking agent's pane. The text includes an option label written by the agent and my own free text. The session row can say `idle` while the agent has exited and the pane holds a shell, so `$(...)` or a backtick in a label would run in my shell. Spec section 7 asks only for the literal flag, stripped control characters and a length cap, which does not stop that. Separately, the spec's "no self-answering" rule keeps answers off the MCP surface, but an agent with a shell could run `curia decide` itself.

## Decision

- **One cleaner.** `internal/sanitize` is the only place untrusted text is cleaned (`Line`, `Text`, `Keys`). Text is cleaned when it enters the store and again wherever it is printed, typed or sent.
- **The `Keys` alphabet.** Text typed into a pane goes through `sanitize.Keys`, whose output holds only Unicode letters, digits, spaces and `. , : - _ /`. Brackets, quotes, `$`, backticks, `;`, `|`, `&`, `<`, `>` and every control character are removed. When anything was removed or cut, the typed line ends with ` Exact wording: call check_decision D-<n>.`
- **One typist.** Only `tmux.Client.SendKeys` types into a pane: `copy-mode -q` first (a pane in copy mode would swallow the keys and report success), then `send-keys -l --`, refusing control characters and malformed pane ids itself, and sending a trailing `;` as `\;`. Its only caller is `internal/deliver`, which types only into a session that is live in the store and on the tmux server and is `idle` or `needs_decision`, never `working`, `waiting`, `stale` or `ended`.
- **The `CLAUDECODE` guard.** `curia decide` and `curia decisions edit` refuse to run when `CLAUDECODE` is set and not empty in their environment (Claude Code sets it in every command it runs), checked before the store is opened; the dashboard's `d` key is not offered under the same condition. One function, `refuseInsideAgent`, decides it. Every flag-form answer is stored with `answered_via = cli`, so an answer that did not come from the popup is visible.

## Consequences

- What is typed cannot do anything in a shell, at the cost of lossy typed text; the agent fetches the exact wording with one extra tool call.
- The guard is not a boundary: an agent can unset the variable or write to the SQLite file, and an agent with a shell can already run `tmux send-keys`. The guard stops the default path, not a determined one. A real boundary needs something an agent cannot reach (plan section 14, open question 2).
- Tests that run `curia decide` must unset `CLAUDECODE`.
