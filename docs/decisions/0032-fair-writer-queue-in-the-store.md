# 0032. A fair writer queue in the store

Status: accepted (Plan 5 records, 2026-11-19; Ruling 177). Written for PR #145, which was open and not merged when this record was written: until it merges this describes the decision, not the code.

## Context

`TestConcurrentUpserts` failed in CI with `SQLITE_BUSY` (Task 8's CI run, and again later). The DSN already had `_txlock=immediate` and `busy_timeout(2000)`, but `busy_timeout` is a poll (sleeps of 1, 2, 5 up to 100 ms), not a queue: under steady contention from several processes (hooks, status lines, `curia mcp`) a waiter can miss every try and fail after 2 s. A new test, `TestWritersInSeveralProcesses`, failed 5 of 5 runs before the fix; `TestConcurrentUpserts` itself did not reproduce locally (0 of 15 failures before, 0 of 30 after).

## Decision

- `internal/store/writelock.go`: an exclusive `flock` on `<db>-writer` plus an in-process FIFO gate, wrapped around the driver connection in `store.Open` (three lines there), covering `BeginTx`, `Exec` and every non-SELECT `Query`. A writer waits its turn instead of polling.
- No retry loop is added, no timeout is raised, and no test is loosened.

## Consequences

- The hook's p50 moved by +0.3 to +1.2 ms in single noisy runs (PostToolUse 4.9 to 5.2 ms, Stop with none queued 6.3 to 7.5 ms, UserPromptSubmit naming a message 5.96 to 6.41 ms).
- It is the largest design change of the fix wave and sits on the hook path. The final review looks at: deadlock or ordering between the `flock` and SQLite's own lock; the lock held across a slow transaction; a crash or leaked lock; the read paths; an older binary or the `sqlite3` shell writing without the lock; a file system where `flock` does nothing (NFS); whether the wrapped driver covers every write statement; the latency gates' headroom.
- `statusline: store: context deadline exceeded`, seen once in CI, is plausibly the same starvation; re-check after this merges (Open Question 16).
