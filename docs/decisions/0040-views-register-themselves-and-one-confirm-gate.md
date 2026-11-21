# 0040. Dashboard views register themselves; one confirm gate

Status: accepted (2026-11-20, Ruling 214; PR #199; code in `internal/ui/commands.go`, `confirm.go`).

## Context

Adding a dashboard view used to touch about seven places in `model.go`, which sat at the linter's complexity limit, and the "press y to confirm" logic (dwell, then a burst rule against pasted keys) had been copied into five views and had drifted.

## Decision

- A view is a file that calls `registerCommand(command{keys, help, order, run})` from its own `init`. The key handler and the help line are derived from the table; a key registered twice panics. The view's factory is attached in `cmd/curia/ui.go` with one `WithX` line. Do not add a branch to `commandKey`.
- Every destructive or user-level prompt uses `confirmGate`: pause, kill, the board's spawn, a review's approve and drop, an irreversible decision answer and a setting change. A `y` counts only after the prompt has been open for the dwell (and the quiet period after a refusal), not within the burst gap of another key, and a following key within the gap shows it came in a burst and refuses it. The final review found two sites outside the gate (the kill prompt and the board's spawn lacked the wait after `y`); PR #211 moved them behind it. A new prompt must use the gate; a test for each site sends a key and a `y` a few milliseconds later and expects nothing.
- Views that read data take it through a source interface, so the dashboard and the matching command (`curia inbox`, `show`, `timeline`, `time`) share one data path.

## Consequences

- The help line gained `n`, `T`, `L`, `S` and `P` automatically. A contributor adding a key picks an `order` for the help line.
