# 0032. A fair writer queue in the store

Status: accepted (2026-11-19, Ruling 177; merged in PR #145, amended by the final-review fix wave, PR #177, Rulings 183 and 184).

## Context

`TestConcurrentUpserts` failed in CI with `SQLITE_BUSY` (Task 8's CI run, and again later). The DSN already had `_txlock=immediate` and `busy_timeout(2000)`, but `busy_timeout` is a poll (sleeps of 1, 2, 5 up to 100 ms), not a queue: under steady contention from several processes (hooks, status lines, `curia mcp`) a waiter can miss every try and fail after 2 s. A new test, `TestWritersInSeveralProcesses`, failed 5 of 5 runs before the fix; `TestConcurrentUpserts` itself did not reproduce locally (0 of 15 failures before, 0 of 30 after).

## Decision

- `internal/store/writelock.go`: an exclusive `flock` on `<db>-writer` plus an in-process FIFO gate, wrapped around the driver connection in `store.Open` (three lines there), covering every write transaction (`BeginTx` unless read-only), every `Exec`, and every `Query` that `isRead` cannot prove to be a read (a `SELECT`, `VALUES`, `EXPLAIN`, a `WITH` whose main statement is a `SELECT`, a read-only `PRAGMA`; anything unclear counts as a write and takes the lock). A writer waits its turn instead of polling.
- No retry loop is added, no timeout is raised, and no test is loosened.

## Consequences

- The hook's p50 moved by +0.3 to +1.2 ms in single noisy runs (PostToolUse 4.9 to 5.2 ms, Stop with none queued 6.3 to 7.5 ms, UserPromptSubmit naming a message 5.96 to 6.41 ms).
- It is the largest design change of the fix wave and sits on the hook path. The final review looked at: deadlock or ordering between the `flock` and SQLite's own lock; the lock held across a slow transaction; a crash or leaked lock; the read paths; an older binary or the `sqlite3` shell writing without the lock; a file system where `flock` does nothing (NFS); whether the wrapped driver covers every write statement; the latency gates' headroom.
- `statusline: store: context deadline exceeded`, seen once in CI, is plausibly the same starvation; the final review (review A) traced it to `Open`, which waited in the queue (see the amendment below).

Amended 2026-11-20 (final review A, PR #177; Rulings 183 and 184):

- Reads never take the writer lock, and `Open` no longer waits behind writers. `Open` used to run `PRAGMA journal_mode=WAL`, which is not a `SELECT`, so every `Open` queued behind a writer (1.5 s behind a 1.5 s writer: the tmux badge, the dashboard and the status line's 200 ms budget waited with it). It now reads `journal_mode` and sets WAL only when the database is not in it. A read that starts with `WITH` (such as `CostSince`'s recursive query) no longer takes the lock and holds it until its rows close.
- Where `flock` is not supported (`ENOTSUP`, `ENOLCK`, `ENOSYS`) the store falls back to SQLite's own locking and logs one line saying so; it does not fail closed (Ruling 183).
- A writer that cannot get the lock within the 2 s `busyTimeout` returns `writer lock busy` (as SQLite returns `SQLITE_BUSY`); the lock is also released when the caller's context ends.
