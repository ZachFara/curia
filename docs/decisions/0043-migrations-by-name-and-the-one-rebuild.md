# 0043. Migrations are tracked by name; the one that rebuilds a table

Status: accepted (2026-11-21, Ruling 243; PRs #209, #210 and #212).

## Context

Plan 5 and Plan 6 were built in parallel and each added migrations, so numbers collide: `0008`, `0009` and `0025` each exist twice, and there is no `0015`. The store applies migrations by file name, each with its record of the name in one transaction.

## Decision

- Migrations are tracked and ordered by file name, never by number. A new one uses only the name it is assigned, and the store tests (which derive the count from the files) list it.
- Migrations only add tables, columns and indexes, with one exception: `0025_schedule_run_interrupted.sql` rebuilds `schedule_runs` (new table with the `interrupted` outcome, rows copied, old table dropped, renamed, index recreated) because SQLite cannot change a `CHECK`. That table exists only on this branch and was never released, so no installed database holds it. `0024_permission_counts.sql` also collapses old duplicate denial rows. `0025_timeline_indexes.sql` only adds indexes.

## Consequences

- Do not copy the rebuild for a table that has shipped: copy-and-rename loses rows or indexes that a later change adds. The review checked that the migrations apply identically from an empty database and from `main`'s, and that an older binary does not reset the new columns.
