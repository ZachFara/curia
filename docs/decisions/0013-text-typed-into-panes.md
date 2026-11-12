# 0013. Text typed into panes

Status: accepted (Plan 2 setup)

## Context

`curia decide` types an answer into the asking agent's pane. The text includes an option label written by the agent and my own free text. The session row can say `idle` while the agent has exited and the pane holds a shell, so `$(...)` or a backtick in a label would run in my shell. Spec section 7 asks only for the literal flag, stripped control characters and a length cap, which does not stop that. Separately, the spec's "no self-answering" rule keeps answers off the MCP surface, but an agent with a shell could run `curia decide` itself.

## Decision

- **One cleaner.** `internal/sanitize` is the only place untrusted text is cleaned (`Line`, `Text`, `Keys`). Text is cleaned when it enters the store and again wherever it is printed, typed or sent.
- **The `Keys` alphabet.** Text typed into a pane goes through `sanitize.Keys`, whose output holds only Unicode letters, digits, spaces and `. , : - _ /`. Brackets, quotes, `$`, backticks, `;`, `|`, `&`, `<`, `>` and every control character are removed. When anything was removed or cut, the typed line ends with ` Exact wording: call check_decision D-<n>.`
- **One typist.** Only `tmux.Client.SendKeys` types into a pane, as three separate tmux invocations, never joined with `\;`: `copy-mode -q` (a pane in copy mode would swallow the keys and report success), `send-keys -l --` with the text, `send-keys Enter`. It refuses control characters, malformed pane ids, a single character and more than 800 runes itself, and sends a trailing `;` as `\;`. Its only caller is `internal/deliver`, which types only into a session that is live in the store and on the tmux server and is `idle` or `needs_decision`, never `working`, `waiting`, `stale` or `ended`. The state is read again inside the claim transaction immediately before typing.
- **Why these limits** (live probe, Claude Code 2.1.322 and tmux 3.8, 2026-11-11): text and a separate Enter submitted 5 times of 5 with no pause, but in one tmux invocation a long line's Enter became a newline inside the prompt; from about 1,000 characters the prompt treats input as a paste and `send-keys -l` splits it, while 600 and 800 characters submitted exactly; at a permission prompt a typed Enter approved the highlighted "Yes" and a single digit selected an option at once; text typed during a running turn is absorbed into that turn at its next tool boundary.
- **The `CLAUDECODE` guard.** `curia decide` and `curia decisions edit` refuse to run when `CLAUDECODE` is set and not empty in their environment (Claude Code sets it in every command it runs), checked before the store is opened; the dashboard's `d` key is not offered under the same condition. One function, `refuseInsideAgent`, decides it. Every flag-form answer is stored with `answered_via = cli`, so an answer that did not come from the popup is visible.

## Consequences

- What is typed cannot do anything in a shell, at the cost of lossy typed text; the agent fetches the exact wording with one extra tool call.
- The guard is not a boundary: an agent can unset the variable or write to the SQLite file, and an agent with a shell can already run `tmux send-keys`. The guard stops the default path, not a determined one. A real boundary needs something an agent cannot reach (plan section 14, open question 2).
- Tests that run `curia decide` must unset `CLAUDECODE`.
- A few milliseconds remain between the claim's commit and the keystrokes, and the `permission_prompt` Notification arrives about 6 seconds after a prompt opens; for those seconds the store can say `working` (never `idle`), which the typing rule already refuses. No hook closes that window (PROGRESS.md, Ruling 48).

Amended 2026-11-12: Ruling 51 (PROGRESS.md) clarifies that `sanitize.Keys` output is never typed on its own; only the sentence built by `decision.TypedMessage`, which begins with the fixed words "Decision D-<n>", is typed, so a typed line can never start with `/` or a command word. Rulings 54 and 58 add `tmux.ErrPartialSend`: after it nothing is typed again. Ruling 73 narrows who may be typed into: only a session that is the only live (non-ended, non-stale) session bound to its pane; an answer for a session sharing its pane goes through the Stop hook or `check_decision`. Ruling 59 drops the "Exact wording" suffix from the Stop hook's reason. The owner accepted (2026-11-12) that typing does not check the pane's foreground program.
